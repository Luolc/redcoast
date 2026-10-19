package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/claude"
)

// installationIDShape is the shape of the inventory's installation_id: a lower-case UUID
// v4, as Codex writes to $CODEX_HOME/installation_id.
var installationIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// authFile is the login in an account's directory, in $CODEX_HOME's format.
const authFile = "auth.json"

// lockFile is the file beside authFile whose lock serializes the renewals and imports
// of the account across processes, the two of a handoff included.
const lockFile = "lock"

// maxAuthFile bounds auth.json; a real one is a few kilobytes.
const maxAuthFile = 64 << 10

// errNoLogin is an account whose directory or auth.json does not exist yet.
var errNoLogin = errors.New("no login; import one")

// logins are the accounts' login states by alias, in accountsDir. A state outlives
// reloads, so the credential a new account set serves sees the renewals that finish
// after the reload.
type logins struct {
	accountsDir string
	mu          sync.Mutex
	byAlias     map[string]*loginState
}

// loginState is one account's login as this process serves it.
type loginState struct {
	dir   string
	email atomic.Pointer[string] // the inventory's email, which a login must match
	// mu serializes this process's renewals and imports of the account; the lock file
	// serializes them with other processes.
	mu     sync.Mutex
	tokens atomic.Pointer[tokens] // nil while the account has no login
	// settled is when this process's last renewal that was forced, failed or changed
	// the token ended, and outcome its result; mu guards both.
	settled time.Time
	outcome error
	unsaved atomic.Pointer[unsavedLogin] // a renewed login served but not written yet
}

// tokens are what a forwarded request needs from the login.
type tokens struct {
	access    string
	accountID string
}

// state returns alias's state, creating it, with email as the inventory's.
func (l *logins) state(alias, email string) *loginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byAlias == nil {
		l.byAlias = map[string]*loginState{}
	}
	st := l.byAlias[alias]
	if st == nil {
		st = &loginState{dir: filepath.Join(l.accountsDir, alias)}
		l.byAlias[alias] = st
	}
	st.email.Store(&email)
	return st
}

// lookup returns alias's state, or nil when no account set has loaded alias.
func (l *logins) lookup(alias string) *loginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byAlias[alias]
}

// refused reports whether an upstream 401 refused a login an account still serves. The
// client cannot change how a request authenticates upstream, as the gateway sets both
// headers, so the 401 refused the token the request carried; a 401 to a token renewed
// since then refuses nothing.
func (l *logins) refused(res *http.Response) bool {
	if res.Request == nil {
		return true
	}
	used := strings.TrimPrefix(res.Request.Header.Get("Authorization"), "Bearer ")
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, st := range l.byAlias {
		if t := st.tokens.Load(); t != nil && t.access == used {
			return true
		}
	}
	return false
}

// account reads installation_id and email from a served account's inventory file and
// returns the source of its credential: the login in its directory, read again at
// every load, with that installation ID. An account without a login yet loads, and
// its requests are refused until one is imported. The email is required because a
// login is checked against it.
func (l *logins) account(alias string, document []byte) (claude.CredentialSource, error) {
	var file struct {
		Email          string  `yaml:"email"`
		InstallationID *string `yaml:"installation_id"`
	}
	switch {
	case yaml.NewDecoder(bytes.NewReader(document)).Decode(&file) != nil:
		return nil, fmt.Errorf("%s: file: not a Codex account document", alias)
	case file.Email == "":
		return nil, fmt.Errorf("%s: email: missing", alias)
	case file.InstallationID == nil:
		return nil, fmt.Errorf("%s: installation_id: missing", alias)
	case !installationIDShape.MatchString(*file.InstallationID):
		return nil, fmt.Errorf("%s: installation_id: not a lower-case UUID v4", alias)
	}
	installationID := *file.InstallationID
	return func(context.Context, claude.Resolver) (claude.Credential, error) {
		st := l.state(alias, file.Email)
		doc, err := readAuth(st.dir)
		switch {
		case errors.Is(err, errNoLogin):
			st.tokens.Store(nil)
		case err != nil:
			return nil, fmt.Errorf("%s: %s: %v", alias, authFile, err)
		default:
			st.tokens.Store(doc.served())
		}
		return login{state: st, installationID: installationID}, nil
	}, nil
}

// authDoc is an auth.json as read: the fields the gateway uses, and the whole document
// so that a renewal keeps the fields it does not know.
type authDoc struct {
	raw    map[string]json.RawMessage
	Tokens struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

// served returns the tokens a forwarded request needs.
func (d authDoc) served() *tokens {
	return &tokens{access: d.Tokens.AccessToken, accountID: d.Tokens.AccountID}
}

// readAuth reads the login in dir; errNoLogin when dir or its auth.json does not
// exist. The directory and the file must be private to this user, and the file a
// regular one. Errors never carry a value from the file.
func readAuth(dir string) (authDoc, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return authDoc{}, errNoLogin
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return authDoc{}, errors.New("the account directory must be a directory with no access for group and others")
	}
	path := filepath.Join(dir, authFile)
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return authDoc{}, errNoLogin
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return authDoc{}, errors.New("must be a regular file with no access for group and others")
	}
	f, err := os.Open(path)
	if err != nil {
		return authDoc{}, errors.New("cannot open")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxAuthFile+1))
	if err != nil || len(data) > maxAuthFile {
		return authDoc{}, errors.New("cannot read, or too large")
	}
	return parseAuth(data)
}

// parseAuth parses a login in $CODEX_HOME's auth.json format. Its access token and
// account ID must be header values; errors never carry a value.
func parseAuth(data []byte) (authDoc, error) {
	var doc authDoc
	if json.Unmarshal(data, &doc.raw) != nil || doc.raw == nil || json.Unmarshal(data, &doc) != nil {
		return authDoc{}, errors.New("not a Codex login")
	}
	for _, value := range []struct{ field, value string }{{"tokens.access_token", doc.Tokens.AccessToken}, {"tokens.account_id", doc.Tokens.AccountID}} {
		if !headerValue(value.value) {
			return authDoc{}, errors.New(value.field + ": empty or not a header value")
		}
	}
	return doc, nil
}

// headerValue reports whether value is non-empty printable ASCII without spaces, so
// that it cannot break or inject a header.
func headerValue(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	return true
}

// lockPoll is how often lockAccount tries a lock another process holds.
const lockPoll = 50 * time.Millisecond

// lockAccount takes the lock of the account in dir, waiting while another process
// holds it until ctx ends, and returns its release.
func lockAccount(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("cannot open the lock file")
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, errors.New("cannot lock the lock file")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, errors.New("another process holds the account's lock")
		case <-time.After(lockPoll):
		}
	}
}

// writeAuth replaces the auth.json in dir with data, whole or not at all, and returns
// once the replacement is on disk.
func writeAuth(dir string, data []byte) error {
	f, err := os.CreateTemp(dir, authFile+".*")
	if err != nil {
		return errors.New("cannot create the new login file")
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return errors.New("cannot write the new login file")
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, authFile)); err != nil {
		return errors.New("cannot replace the login file")
	}
	d, err := os.Open(dir)
	if err == nil {
		err = errors.Join(d.Sync(), d.Close())
	}
	if err != nil {
		return errors.New("cannot sync the account directory")
	}
	return nil
}

// login is a Codex account's credential: its login as this process serves it, and
// the installation ID the account's requests carry.
type login struct {
	state          *loginState
	installationID string
}

// Authorize removes every credential the client sent, sets the account's token and
// account ID, and puts the account's installation ID in place of the client's. An
// account without a login marks the request for the guard to refuse.
func (l login) Authorize(out *http.Request) {
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Chatgpt-Account-Id"} {
		out.Header.Del(name)
	}
	t := l.state.tokens.Load()
	if t == nil {
		out.Header.Set(refusedMark, errNoLogin.Error())
		return
	}
	out.Header.Set("Authorization", "Bearer "+t.access)
	out.Header.Set("Chatgpt-Account-Id", t.accountID)
	replaceInstallationID(out, l.installationID)
}

// Same reports whether other is the same account's login with the same installation
// ID; a renewed token is the same login.
func (l login) Same(other claude.Credential) bool {
	o, ok := other.(login)
	return ok && o == l
}
