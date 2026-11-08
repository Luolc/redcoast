package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// maxWriteBody bounds the body of an account write; a real one is a few hundred bytes.
const maxWriteBody = 8 << 10

// The action names an account write carries in X-Redcoast-Action.
const (
	actionCreate   = "account-create"
	actionUpdate   = "account-update"
	actionVerify   = "account-verify"
	actionActivate = "account-activate"
)

// aliasPlaceholder is what a token reference template names the alias by.
const aliasPlaceholder = "{alias}"

// The shapes of the fields an account write takes. A field that matches none of them
// is refused by name, so a credential value pasted into a field is never stored.
var (
	emailShape = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
	hostShape  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	// kindShape is a proxy's provider or type: a short word, as in the inventory.
	kindShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	planShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
)

// CheckTokenRefTemplate reports whether template can build a token reference from an
// alias: it holds the placeholder once, and with a made-up alias it is a credential
// reference.
func CheckTokenRefTemplate(template string) error {
	if strings.Count(template, aliasPlaceholder) != 1 {
		return errors.New("must contain " + aliasPlaceholder + " once")
	}
	if err := credential.Check(strings.Replace(template, aliasPlaceholder, "sample", 1)); err != nil {
		return errors.New("must give a credential reference")
	}
	return nil
}

// AccountWriterConfig is what the account writes need.
type AccountWriterConfig struct {
	Store      *session.Store
	Accounts   *Accounts
	Egress     *Egress
	Entrypoint *Entrypoint
	// Reload applies the accounts table to the served set, as the management
	// interface's reload does.
	Reload func(ctx context.Context) (Reload, error)
	// Resolve returns the resolver of credential references, as a reload builds it.
	Resolve func(ctx context.Context) (Resolver, error)
	// AllowFrom are the TCP peer addresses writes are accepted from; empty accepts none.
	AllowFrom []netip.Prefix
	// TokenRefTemplate is the token reference of a new account with the alias in place of
	// {alias}; empty lets no account be created.
	TokenRefTemplate string
	// SetupEnabled turns the setup routes on; off, they answer 501.
	SetupEnabled bool
	// AdminWrite turns the account commands of the management socket that write on; off,
	// they answer 501 account_write_unavailable.
	AdminWrite bool
	// SetupSecrets opens the secret store that a setup handover writes the token to, with
	// the credential the gateway reads its references with. It is called when a
	// handover writes and when tokens are dated, and the store is not kept.
	SetupSecrets func(ctx context.Context) (credential.Store, error)
}

// AccountWriter is the dashboard's write path over the accounts table: create, edit,
// verify and activate an account. One write runs at a time, so each reads the table
// the previous one left.
type AccountWriter struct {
	cfg AccountWriterConfig
	now func() time.Time

	// mu serializes the writes, so each reads the table the previous one left. A verify
	// or an activate holds it through its network checks, up to verifyTimeout plus
	// probeWait, and every other write waits for it, even when its client has left.
	mu sync.Mutex

	// setup is the gateway's side of setting tokens; nil while setup is off.
	setup *Setup
}

// NewAccountWriter returns the write path for cfg.
func NewAccountWriter(cfg AccountWriterConfig) *AccountWriter {
	w := &AccountWriter{cfg: cfg, now: time.Now}
	if cfg.SetupEnabled {
		w.setup = &Setup{w: w, expiries: map[string]tokenExpiry{}}
	}
	return w
}

// writeError is the answer to a refused write: the status, a message that never holds
// request content, and the field or check it concerns.
type writeError struct {
	status       int
	message      string
	field, check string
	// code is the fixed code the management socket answers with in place of message; the
	// dashboard answers with message.
	code string
}

func (e writeError) Error() string { return e.message }

// fieldProblem is a 400 about field.
func fieldProblem(field, message string) error {
	code := codeFieldInvalid
	if message == "missing" {
		code = codeFieldMissing
	}
	return writeError{status: http.StatusBadRequest, message: message, field: field, code: code}
}

// peer returns the TCP peer address of r, never read from a header.
func peer(r *http.Request) (netip.Addr, bool) {
	addr, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Addr().Unmap(), true
}

// writeStatus says whether a write from the peer of r would be accepted, and if not
// why, in words the dashboard shows.
func (w *AccountWriter) writeStatus(r *http.Request) (enabled bool, reason string) {
	if len(w.cfg.AllowFrom) == 0 {
		return false, "writes are off: the gateway lists no source address"
	}
	ip, ok := peer(r)
	if !ok || !slices.ContainsFunc(w.cfg.AllowFrom, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return false, "writes are off for this source address"
	}
	return true, ""
}

// view returns whether accounts are editable here and what the writes are for r; w
// may be nil, a provider without a write path.
func (w *AccountWriter) view(r *http.Request) (editable bool, v writeView) {
	if w == nil {
		return false, writeView{Reason: "this provider's accounts are not edited here"}
	}
	v.Enabled, v.Reason = w.writeStatus(r)
	v.CanCreate = w.cfg.TokenRefTemplate != ""
	return true, v
}

// writeBody is the decoded body of a request and the answer's parts.
type writeResponse struct {
	Account *accountConfig `json:"account,omitempty"`
	Reload  *Reload        `json:"reload,omitempty"`
	Verify  *verifyReport  `json:"verify,omitempty"`
	// PlanRefresh says what became of the quota refresh a raised quota unit asks for:
	// refreshed, or not refreshed; absent when none was due.
	PlanRefresh string `json:"plan_refresh,omitempty"`
}

// route returns the handler of one write route. It refuses, in this order and before
// reading the body: a source outside AllowFrom (403), a request without exactly one
// Origin that is the page's own (403), and without exactly one X-Redcoast-Action with
// the route's action name (403). Then the body must be JSON of at most maxWriteBody
// bytes, with no unknown or repeated key. Every refusal is logged without request
// content and answers without it.
func route[B any](w *AccountWriter, action string, handle func(r *http.Request, alias string, body *B, entry session.AuditEntry) (any, error)) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		var body B
		ip, _ := peer(r)
		listed, _ := w.writeStatus(r)
		refuse := func(err error) { refuseWrite(rw, ip, listed, action, err) }
		if err := w.admit(r, action); err != nil {
			refuse(err)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxWriteBody))
		if err != nil {
			refuse(writeError{status: http.StatusRequestEntityTooLarge, message: "request body larger than 8 KiB", code: errorBadRequest})
			return
		}
		if err := decodeStrict(data, &body); err != nil {
			refuse(writeError{status: http.StatusBadRequest, message: "request body is not a JSON object with known, unrepeated keys", code: errorBadRequest})
			return
		}
		alias := r.PathValue("alias")
		if strings.Contains(r.Pattern, "{alias}") && !accountAlias.MatchString(alias) {
			refuse(fieldProblem("alias", "not an account alias"))
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		answer, err := handle(r, alias, &body, session.AuditEntry{SourceIP: ip.String(), Action: action})
		if err != nil {
			refuse(err)
			return
		}
		if done, ok := answer.(accepted); ok {
			writeJSON(rw, http.StatusAccepted, done.body)
			return
		}
		writeJSON(rw, http.StatusOK, answer)
	}
}

// refuseWrite logs a refused write of a listed source without request content and answers it: the status,
// the message and the field or check it concerns.
func refuseWrite(rw http.ResponseWriter, ip netip.Addr, listed bool, action string, err error) {
	var we writeError
	if !errors.As(err, &we) {
		we = writeError{status: http.StatusInternalServerError, message: "write failed"}
	}
	// A source outside the list could add log lines without limit, so only the refusals
	// of listed sources are logged.
	if listed {
		log.Printf("account write refused ip=%s action=%s status=%d reason=%q", ip, action, we.status, we.message)
	}
	answer := map[string]string{"error": we.message}
	for key, value := range map[string]string{"field": we.field, "check": we.check} {
		if value != "" {
			answer[key] = value
		}
	}
	writeJSON(rw, we.status, answer)
}

// admit runs the checks that come before the body is read, and returns the 403 or 415
// of the first that fails: the source address, the Origin, the action header and the
// content type.
func (w *AccountWriter) admit(r *http.Request, action string) error {
	forbidden := func(message string) error { return writeError{status: http.StatusForbidden, message: message} }
	if len(w.cfg.AllowFrom) == 0 {
		return forbidden("writes disabled")
	}
	if enabled, _ := w.writeStatus(r); !enabled {
		return forbidden("source address not allowed")
	}
	if !sameOrigin(r.Header["Origin"], r) {
		return forbidden("cross-origin request")
	}
	if values := r.Header.Values(QuotaRefreshHeader); len(values) != 1 || values[0] != action {
		return forbidden(QuotaRefreshHeader + ": " + action + " is required")
	}
	if types := r.Header.Values("Content-Type"); len(types) != 1 || !strings.HasPrefix(strings.ToLower(types[0]), "application/json") {
		return writeError{status: http.StatusUnsupportedMediaType, message: "Content-Type: application/json is required"}
	}
	return nil
}

// decodeStrict decodes data, one JSON object with no key repeated at any depth, into
// v, refusing unknown keys and anything after the object. The error says nothing of
// the content.
func decodeStrict(data []byte, v any) error {
	check := json.NewDecoder(bytes.NewReader(data))
	if token, err := check.Token(); err != nil || token != json.Delim('{') {
		return errors.New("not an object")
	}
	check = json.NewDecoder(bytes.NewReader(data))
	if err := noRepeatedKeys(check); err != nil {
		return err
	}
	if _, err := check.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing content")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errors.New("not the expected shape")
	}
	return nil
}

// noRepeatedKeys reads one JSON value and fails when an object in it repeats a key.
func noRepeatedKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			// encoding/json matches a key to a field whatever its case, so a key repeated in
			// another case is a repeat.
			name, _ := key.(string)
			name = strings.ToLower(name)
			if seen[name] {
				return errors.New("repeated key")
			}
			seen[name] = true
			if err := noRepeatedKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case json.Delim('['):
		for decoder.More() {
			if err := noRepeatedKeys(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	return nil
}

// proxyBody is the proxy object of a create or an edit; a key left out is nil.
type proxyBody struct {
	Provider    *string `json:"provider"`
	Type        *string `json:"type"`
	Direct      *bool   `json:"direct"`
	Host        *string `json:"host"`
	Port        *int    `json:"port"`
	ExpectedIP  *string `json:"expected_egress_ip"`
	UsernameRef *string `json:"username_ref"`
	PasswordRef *string `json:"password_ref"`
}

// applyProxy puts the keys of b into row.
func (b *proxyBody) applyProxy(row *session.AccountRow) {
	if b == nil {
		return
	}
	if b.Provider != nil {
		row.ProxyProvider = *b.Provider
	}
	if b.Type != nil {
		row.ProxyType = *b.Type
	}
	if b.Direct != nil {
		row.Direct = *b.Direct
	}
	if b.Host != nil {
		row.ProxyHost = *b.Host
	}
	if b.Port != nil {
		row.ProxyPort = *b.Port
	}
	if b.ExpectedIP != nil {
		row.ExpectedIP = *b.ExpectedIP
	}
	if b.UsernameRef != nil {
		row.UsernameRef = *b.UsernameRef
	}
	if b.PasswordRef != nil {
		row.PasswordRef = *b.PasswordRef
	}
}

// refPair is the proxy login of an account.
type refPair struct{ Username, Password string }

// proxyRefOptions returns the proxy logins accounts use now, each pair once, sorted.
func proxyRefOptions(rows []session.AccountRow) []proxyRefOption {
	seen := map[refPair]bool{}
	var out []proxyRefOption
	for _, row := range rows {
		pair := refPair{row.UsernameRef, row.PasswordRef}
		if row.Direct || pair.Username == "" || pair.Password == "" || seen[pair] {
			continue
		}
		seen[pair] = true
		out = append(out, proxyRefOption{UsernameRef: pair.Username, PasswordRef: pair.Password})
	}
	slices.SortFunc(out, func(a, b proxyRefOption) int {
		return strings.Compare(a.UsernameRef+"\x00"+a.PasswordRef, b.UsernameRef+"\x00"+b.PasswordRef)
	})
	return out
}

// proxyRefOption is a proxy login that exists already, as references.
type proxyRefOption struct {
	UsernameRef string `json:"username_ref"`
	PasswordRef string `json:"password_ref"`
}

// checkRow refuses a row whose fields have the wrong shape or do not fit together: the
// exit of a direct account has only an expected IP, any other exit has a host, a port
// and, when checkLogin is set, one of the proxy logins in options. The error names one
// field.
func checkRow(row session.AccountRow, options []proxyRefOption, checkLogin bool) error {
	if len(row.Email) > 254 || !emailShape.MatchString(row.Email) {
		return fieldProblem("email", "not an email address")
	}
	if err := checkReceiverEmail(row.ReceiverEmail); err != nil {
		return err
	}
	if row.Access != "gateway" && row.Access != "direct" {
		return fieldProblem("access", "must be gateway or direct")
	}
	if row.DeviceID != "" && !deviceIDShape.MatchString(row.DeviceID) {
		return fieldProblem("device_id", "not 64 lower-case hex digits")
	}
	if row.AccountUUID != "" && !accountUUIDShape.MatchString(row.AccountUUID) {
		return fieldProblem("account_uuid", "not a lower-case UUID")
	}
	return checkExit(row, options, checkLogin)
}

// checkReceiverEmail refuses a receiver email that is set and is not a bare address: a
// display name or a comment around it is refused.
func checkReceiverEmail(value string) error {
	if value == "" {
		return nil
	}
	addr, err := mail.ParseAddress(value)
	if len(value) > 254 || err != nil || addr.Address != value || !emailShape.MatchString(value) {
		return fieldProblem("receiver_email", "not an email address")
	}
	return nil
}

// checkExit is checkRow for the exit's fields.
func checkExit(row session.AccountRow, options []proxyRefOption, checkLogin bool) error {
	if row.ProxyProvider != "" && !kindShape.MatchString(row.ProxyProvider) {
		return fieldProblem("proxy.provider", "not a provider name")
	}
	if row.ProxyType != "" && !kindShape.MatchString(row.ProxyType) {
		return fieldProblem("proxy.type", "not a proxy type")
	}
	if ip, err := netip.ParseAddr(row.ExpectedIP); err != nil || ip.Zone() != "" {
		return fieldProblem("proxy.expected_egress_ip", "not an IP address")
	}
	if row.Direct {
		for _, field := range []struct {
			name    string
			present bool
		}{{"proxy.host", row.ProxyHost != ""}, {"proxy.port", row.ProxyPort != 0}, {"proxy.username_ref", row.UsernameRef != ""}, {"proxy.password_ref", row.PasswordRef != ""}} {
			if field.present {
				return fieldProblem(field.name, "not allowed with proxy.direct")
			}
		}
		return nil
	}
	if len(row.ProxyHost) > 253 || !hostShape.MatchString(row.ProxyHost) {
		return fieldProblem("proxy.host", "not a host name")
	}
	if row.ProxyPort < 1 || row.ProxyPort > 65535 {
		return fieldProblem("proxy.port", "not a port")
	}
	if checkLogin && !slices.Contains(options, proxyRefOption{UsernameRef: row.UsernameRef, PasswordRef: row.PasswordRef}) {
		return fieldProblem("proxy.username_ref", "not a proxy login that accounts use")
	}
	return nil
}

// served reports whether the gateway serves a row.
func served(row session.AccountRow) bool {
	return row.Status == "active" && row.Access == "gateway"
}

// servedSettings is a row without what no served set depends on: the emails, the proxy's provider and type, the
// revision, the times and the checks.
func servedSettings(row session.AccountRow) session.AccountRow {
	row.Email, row.ReceiverEmail, row.ProxyProvider, row.ProxyType = "", "", "", ""
	row.Rev, row.CreatedAt, row.UpdatedAt, row.Verify = 0, time.Time{}, time.Time{}, session.Verification{}
	return row
}

// reloadNeeded reports whether the served set can differ between two versions of a row.
func reloadNeeded(before, after session.AccountRow) bool {
	return (served(before) || served(after)) && servedSettings(before) != servedSettings(after)
}

// fieldsOf returns the names of the fields the fieldErrors in err report.
func fieldsOf(err error) []string {
	var fields []string
	var walk func(err error)
	walk = func(err error) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, inner := range joined.Unwrap() {
				walk(inner)
			}
			return
		}
		var fe fieldError
		if errors.As(err, &fe) {
			fields = append(fields, fe.field)
		}
	}
	walk(err)
	return fields
}

// dryLoad resolves the references of a row that would be served, as the reload after
// the write will, and reports the first field that cannot be loaded, never why.
func (w *AccountWriter) dryLoad(ctx context.Context, row session.AccountRow) error {
	file, data, err := rowDocument(row)
	if err != nil {
		return fieldProblem("file", "cannot be described")
	}
	entry, err := servedAccount(row.Alias, file, data, Claude)
	if err != nil {
		return fieldProblem(firstField(err), "not valid for a served account")
	}
	resolve, err := w.cfg.Resolve(ctx)
	if err != nil {
		return writeError{status: http.StatusBadGateway, message: "the credential resolver is not available", code: codeResolverUnavailable}
	}
	if _, err := loadAccount(ctx, entry, resolve, w.cfg.Accounts.set.Load().concurrency); err != nil {
		return writeError{status: http.StatusUnprocessableEntity, message: "a reference cannot be resolved", field: firstField(err), code: codeReferenceUnresolved}
	}
	return nil
}

// firstField returns the first field name in err, or "".
func firstField(err error) string {
	if fields := fieldsOf(err); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// logWrite logs a successful write: the action, the account and the names of the fields
// it changed, never a value.
func logWrite(entry session.AuditEntry, alias string, before, after session.AccountRow) {
	var names []string
	for name := range session.ChangedFields(before, after) {
		names = append(names, name)
	}
	slices.Sort(names)
	log.Printf("account write ip=%s action=%s alias=%s result=ok fields=%s", entry.SourceIP, entry.Action, alias, strings.Join(names, ","))
}

// afterWrite reloads when the write can change the served set and builds the answer.
func (w *AccountWriter) afterWrite(ctx context.Context, before, after session.AccountRow, verify *verifyReport) (writeResponse, error) {
	config := configOf(after)
	answer := writeResponse{Account: &config, Verify: verify}
	if reloadNeeded(before, after) {
		report, err := w.cfg.Reload(ctx)
		if err != nil {
			return writeResponse{}, writeError{status: http.StatusBadGateway, message: "the account was saved but the reload failed; the served set is unchanged until a reload succeeds", code: reasonReloadFailed}
		}
		answer.Reload = &report
	}
	return answer, nil
}
