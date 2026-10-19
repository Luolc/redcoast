package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/redcoast/claude"
)

// TokenURL is the token endpoint of ChatGPT logins, where a renewal is sent unless the
// configuration names a loopback one.
const TokenURL = "https://auth.openai.com/oauth/token"

// clientID is the OAuth client of the Codex CLI, whose logins the gateway renews.
const clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// The Codex CLI renews a login whose access token expires within renewWindow, or,
// when the token's expiry cannot be read, one last renewed renewInterval ago.
const (
	renewWindow   = 5 * time.Minute
	renewInterval = 8 * 24 * time.Hour
)

// refusedCodes are the token endpoint's error codes after which the refresh token
// will not work again, whatever the status.
var refusedCodes = map[string]bool{"refresh_token_expired": true, "refresh_token_reused": true, "refresh_token_invalidated": true}

// errorCode is the shape of an error code worth repeating in a log line.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// renewer renews and imports the logins of l through the token endpoint at tokenURL.
type renewer struct {
	logins   *logins
	tokenURL string
	now      func() time.Time
}

// renew renews alias's login through client, under the account's locks and after
// reading auth.json again, so that a renewal another process wrote is taken up rather
// than repeated: at once when the upstream refused the login at refusedAt, unless a
// renewal of this process settled after that, whose outcome then stands; with a zero
// refusedAt, only when it is due. It wraps
// claude.ErrRenewalRefused when only a new import can help: the account has no login,
// the token endpoint refused the refresh token, or the renewed login fails the
// identity check. Errors never carry a token.
func (r renewer) renew(ctx context.Context, alias string, client *http.Client, refusedAt time.Time) error {
	st := r.logins.lookup(alias)
	if st == nil {
		return errors.New("no such account")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.unsaved.Load() != nil {
		return r.save(ctx, st)
	}
	forced := !refusedAt.IsZero()
	if forced && st.settled.After(refusedAt) {
		return st.outcome
	}
	before := st.tokens.Load()
	err := r.renewLocked(ctx, st, client, forced)
	if after := st.tokens.Load(); forced || err != nil || after != nil && (before == nil || after.access != before.access) {
		st.settled, st.outcome = r.now(), err
	}
	return err
}

// renewLocked is renew with st.mu held.
func (r renewer) renewLocked(ctx context.Context, st *loginState, client *http.Client, forced bool) error {
	served := st.tokens.Load()
	// The lock file is in the account's directory, which only an import creates.
	if _, err := readAuth(st.dir); err != nil {
		return fmt.Errorf("%w: %s: %v", claude.ErrRenewalRefused, authFile, err)
	}
	unlock, err := lockAccount(ctx, st.dir)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := readAuth(st.dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", claude.ErrRenewalRefused, authFile, err)
	}
	st.tokens.Store(doc.served())
	reason := "unauthorized_recovery"
	if forced && served != nil && doc.Tokens.AccessToken != served.access {
		// Another process renewed the login since this one served it: the token the
		// upstream refused is no longer the stored one.
		return nil
	}
	if !forced {
		if reason = r.due(doc); reason == "" {
			return nil
		}
	}
	renewed, err := r.refresh(ctx, client, doc.Tokens.RefreshToken, reason)
	if err != nil {
		return err
	}
	if renewed.IDToken == "" {
		renewed.IDToken = doc.Tokens.IDToken
	}
	if renewed.RefreshToken == "" {
		renewed.RefreshToken = doc.Tokens.RefreshToken
	}
	if !headerValue(renewed.AccessToken) {
		return errors.New("the token endpoint answered without a usable access token")
	}
	if err := checkIdentity(renewed.IDToken, *st.email.Load(), doc.Tokens.AccountID); err != nil {
		return fmt.Errorf("%w: the renewed login %v", claude.ErrRenewalRefused, err)
	}
	data, err := doc.renewed(renewed, r.now())
	if err != nil {
		return err
	}
	st.tokens.Store(&tokens{access: renewed.AccessToken, accountID: doc.Tokens.AccountID})
	if err := writeAuth(st.dir, data); err != nil {
		// The refresh token on disk is spent: the renewed login is served from memory
		// and its write retried, with no renewal, until it is saved.
		st.unsaved.Store(&unsavedLogin{data: data, spent: doc.Tokens.RefreshToken})
		log.Printf("renewed login not saved alias=%s: %v; it needs a new import if this process stops first", filepath.Base(st.dir), err)
		return fmt.Errorf("the renewed login is served but not saved: %v", err)
	}
	return nil
}

// unsavedLogin is a renewed login whose write failed: the document to write and the
// refresh token it spent, which the stored login still holds.
type unsavedLogin struct {
	data  []byte
	spent string
}

// save writes st's unsaved login under the account's lock, with st.mu held. When the
// stored login no longer holds the spent refresh token, another login was imported
// since, and that one is served instead.
func (r renewer) save(ctx context.Context, st *loginState) error {
	unlock, err := lockAccount(ctx, st.dir)
	if err != nil {
		return err
	}
	defer unlock()
	unsaved := st.unsaved.Load()
	doc, err := readAuth(st.dir)
	if err == nil && doc.Tokens.RefreshToken != unsaved.spent {
		st.tokens.Store(doc.served())
		st.unsaved.Store(nil)
		return nil
	}
	if err := writeAuth(st.dir, unsaved.data); err != nil {
		return fmt.Errorf("the renewed login is served but still not saved: %v", err)
	}
	st.unsaved.Store(nil)
	st.settled, st.outcome = r.now(), nil
	return nil
}

// problems names the accounts whose renewed login is served but not saved, for the
// health endpoint.
func (l *logins) problems() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for alias, st := range l.byAlias {
		if st.unsaved.Load() != nil {
			out = append(out, alias+": renewed login not saved; a new import is needed if the gateway stops first")
		}
	}
	slices.Sort(out)
	return out
}

// due returns the reason the Codex CLI gives for renewing doc now, or "" when it is
// not due.
func (r renewer) due(doc authDoc) string {
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if jwtClaims(doc.Tokens.AccessToken, &claims) == nil && claims.Exp > 0 {
		if !time.Unix(claims.Exp, 0).After(r.now().Add(renewWindow)) {
			return "proactive_expiry"
		}
		return ""
	}
	last, err := time.Parse(time.RFC3339Nano, doc.LastRefresh)
	if err == nil && last.Before(r.now().Add(-renewInterval)) {
		return "scheduled"
	}
	return ""
}

// refreshed is the token endpoint's answer; a token it leaves out stays as it was.
type refreshed struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// refresh exchanges refreshToken at the token endpoint as the Codex CLI does.
func (r renewer) refresh(ctx context.Context, client *http.Client, refreshToken, reason string) (refreshed, error) {
	if refreshToken == "" {
		return refreshed{}, fmt.Errorf("%w: the login has no refresh token", claude.ErrRenewalRefused)
	}
	body, err := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": refreshToken, "refresh_reason": reason})
	if err != nil {
		return refreshed{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.tokenURL, bytes.NewReader(body))
	if err != nil {
		return refreshed{}, errors.New("cannot build the token request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return refreshed{}, errors.New("the token endpoint was not reached through the account's exit")
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxAuthFile))
	if err != nil {
		return refreshed{}, errors.New("the token endpoint's answer could not be read")
	}
	if res.StatusCode != http.StatusOK {
		return refreshed{}, refreshFailure(res.StatusCode, data)
	}
	var out refreshed
	if json.Unmarshal(data, &out) != nil {
		return refreshed{}, errors.New("the token endpoint's answer is not JSON")
	}
	return out, nil
}

// refreshFailure classifies a failed refresh as the Codex CLI does: a 401, a 400 with
// invalid_grant and the codes of refusedCodes mean the refresh token will not work
// again; anything else may pass on a retry. Only the status and an error code of
// errorCode's shape are kept.
func refreshFailure(status int, body []byte) error {
	var answer struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	_ = json.Unmarshal(body, &answer)
	var code string
	if json.Unmarshal(answer.Error, &code) != nil || code == "" {
		var nested struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(answer.Error, &nested)
		code = nested.Code
	}
	if code == "" {
		code = answer.Code
	}
	code = strings.ToLower(code)
	shown := ""
	if errorCode.MatchString(code) {
		shown = " " + code
	}
	if status == http.StatusUnauthorized || refusedCodes[code] || (status == http.StatusBadRequest && code == "invalid_grant") {
		return fmt.Errorf("%w: the token endpoint answered %d%s", claude.ErrRenewalRefused, status, shown)
	}
	return fmt.Errorf("the token endpoint answered %d%s", status, shown)
}

// renewed returns the document with renewed's tokens and the time of the renewal; the
// fields the gateway does not use stay as they were.
func (d authDoc) renewed(renewed refreshed, at time.Time) ([]byte, error) {
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(d.raw["tokens"], &stored); err != nil || stored == nil {
		return nil, errors.New("the login's tokens are not an object")
	}
	for name, value := range map[string]string{"id_token": renewed.IDToken, "access_token": renewed.AccessToken, "refresh_token": renewed.RefreshToken} {
		stored[name], _ = json.Marshal(value)
	}
	out := make(map[string]json.RawMessage, len(d.raw))
	for name, value := range d.raw {
		out[name] = value
	}
	out["tokens"], _ = json.Marshal(stored)
	out["last_refresh"], _ = json.Marshal(at.UTC().Format(time.RFC3339Nano))
	return json.MarshalIndent(out, "", "  ")
}

// importLogin replaces alias's login with data once it is a complete login that passes
// the identity check: the account's first login, or one of the same ChatGPT account as
// the stored login. Replacing the account means deleting its directory first. Errors
// never carry a token.
func (r renewer) importLogin(ctx context.Context, alias string, data []byte) error {
	st := r.logins.lookup(alias)
	if st == nil {
		return errors.New("no such account")
	}
	doc, err := parseAuth(data)
	if err != nil {
		return fmt.Errorf("%s: %v", authFile, err)
	}
	if doc.Tokens.IDToken == "" || doc.Tokens.RefreshToken == "" {
		return fmt.Errorf("%s: tokens.id_token and tokens.refresh_token are required", authFile)
	}
	if err := checkIdentity(doc.Tokens.IDToken, *st.email.Load(), doc.Tokens.AccountID); err != nil {
		return fmt.Errorf("%s: %v", authFile, err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.Mkdir(st.dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("cannot create the account directory")
	}
	stored, err := readAuth(st.dir)
	if err != nil && !errors.Is(err, errNoLogin) {
		return fmt.Errorf("stored %s: %v", authFile, err)
	}
	unlock, err := lockAccount(ctx, st.dir)
	if err != nil {
		return err
	}
	defer unlock()
	if stored, err = readAuth(st.dir); err != nil && !errors.Is(err, errNoLogin) {
		return fmt.Errorf("stored %s: %v", authFile, err)
	}
	if err == nil && stored.Tokens.AccountID != doc.Tokens.AccountID {
		return errors.New("tokens.account_id: not the stored login's account; delete the account directory to replace the account")
	}
	if err := writeAuth(st.dir, data); err != nil {
		return err
	}
	st.tokens.Store(doc.served())
	st.unsaved.Store(nil)
	return nil
}

// checkIdentity checks a login against the account: the email claim of its id_token
// equals email, ignoring case, and the chatgpt_account_id of the token's
// https://api.openai.com/auth claim equals accountID. The token's signature is not
// verified: logins come only through the management socket.
func checkIdentity(idToken, email, accountID string) error {
	var claims struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := jwtClaims(idToken, &claims); err != nil {
		return errors.New("tokens.id_token: not a JWT")
	}
	if claims.Email == "" {
		claims.Email = claims.Profile.Email
	}
	switch {
	case !strings.EqualFold(claims.Email, email):
		return errors.New("tokens.id_token: its email is not the account's")
	case claims.Auth.AccountID != accountID:
		return errors.New("tokens.account_id: not the id_token's chatgpt_account_id")
	}
	return nil
}

// jwtClaims decodes the claims of token, a JWT, into claims without verifying it.
func jwtClaims(token string, claims any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, claims)
}
