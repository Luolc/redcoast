package claude

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// harness is a session store with accounts and one issued session, the common ground
// of the entrypoint tests.
type harness struct {
	t         *testing.T
	store     *session.Store
	path      string // the SQLite file
	accounts  *Accounts
	token     string // the issued session's credential
	sessionID string
	clock     time.Time
}

// newHarness opens a fresh store with a 10 s lease at a fixed clock (advance moves it)
// and adds a direct account for each alias; it then issues one session.
func newHarness(t *testing.T, aliases ...string) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "gateway.sqlite"), clock: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	h.open()
	h.accounts = NewAccounts(&AccountSet{byAlias: map[string]*account{}}, h.store)
	for _, alias := range aliases {
		h.add(alias, "sk-ant-oat01-synthetic-"+alias+"-token")
	}
	h.token, h.sessionID = h.issue()
	return h
}

// open opens the store at h.path.
func (h *harness) open() {
	h.t.Helper()
	store, err := session.Open(context.Background(), h.path, session.Config{Lease: 10 * time.Second, IdleExpiry: 30 * time.Second, Now: func() time.Time { return h.clock }})
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = store
	h.t.Cleanup(func() { _ = store.Close() })
	// A reopened store, as after a restart, is the one the accounts migrate through.
	if h.accounts != nil {
		h.accounts.store, h.accounts.migrate = store, store.MigrateBindings
	}
}

// add adds an account whose outbound side connects directly, for tests of forwarding
// that do not involve the exit, and puts it on the pro plan. It returns the account.
func (h *harness) add(alias, token string) *account {
	h.t.Helper()
	if err := h.store.SetPlanSchedule(context.Background(), session.PlanPeriod{Alias: alias, Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
		h.t.Fatal(err)
	}
	a := &account{alias: alias, token: token, proxyUsername: "synthetic-user-" + alias, proxyPassword: "Synthetic-Password-" + alias, exit: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, transport: newTransport(DefaultAccountConcurrency), slots: make(chan struct{}, DefaultAccountConcurrency)}
	h.t.Cleanup(a.transport.CloseIdleConnections)
	h.accounts.set.Load().byAlias[alias] = a
	return a
}

// addWithExit adds an account whose every connection goes through the HTTP proxy at exit.
func (h *harness) addWithExit(alias, token string, exit *url.URL) *account {
	a := h.add(alias, token)
	a.exit = exit
	authenticated := *exit
	authenticated.User = url.UserPassword(a.proxyUsername, a.proxyPassword)
	a.transport.Proxy = http.ProxyURL(&authenticated)
	return a
}

// issue issues a session and returns its credential and ID.
func (h *harness) issue() (token, id string) {
	h.t.Helper()
	issued, err := h.store.Issue(context.Background(), "test-machine", session.LocalSource, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return issued.Token, issued.ID
}

// gateway starts the entrypoint in front of upstream without capture.
func (h *harness) gateway(upstream string) *httptest.Server {
	h.t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		h.t.Fatal(err)
	}
	server := httptest.NewServer(h.router(target, accountTransport{}))
	h.t.Cleanup(server.Close)
	return server
}

// router returns a router on the harness clock, the clock the store runs on, so that
// Retry-After and pauses are measured where the readings are dated.
func (h *harness) router(target *url.URL, transport http.RoundTripper) *router {
	rt := newRouter(h.store, h.accounts, target, transport)
	rt.now = func() time.Time { return h.clock }
	return rt
}

// captured starts the entrypoint in front of upstream with capture into sink.
func (h *harness) captured(upstream string, sink func(captureRecord) error) *httptest.Server {
	h.t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		h.t.Fatal(err)
	}
	server := httptest.NewServer(newCapturedProxy(h.router(target, captureTransport{accountTransport{}}), sink))
	h.t.Cleanup(server.Close)
	return server
}

// authorizing adds the session credential to requests that carry no Authorization.
type authorizing struct {
	token string
	base  http.RoundTripper
}

// RoundTrip sets the credential and sends the request.
func (a authorizing) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+a.token)
	}
	return a.base.RoundTrip(r)
}

// client returns a client that sends the harness's session credential and does not
// ask for compression.
func (h *harness) client() *http.Client {
	return h.clientFor(h.token)
}

// clientFor returns a client that sends token as the session credential.
func (h *harness) clientFor(token string) *http.Client {
	base := &http.Transport{DisableCompression: true}
	h.t.Cleanup(base.CloseIdleConnections)
	return &http.Client{Timeout: 5 * time.Second, Transport: authorizing{token, base}}
}

// trafficRow is one traffic row as read back from the file.
type trafficRow struct {
	sessionID, alias, kind, result, claudeSession, claudeAgent, route, request string
	status                                                                     int
	bytesUp, bytesDown                                                         int64
}

// trafficRows reads the traffic table once it holds at least n rows or two seconds
// have passed. A row is recorded in the handler's deferred block, after the response
// reached the client, so a read right after the response can run ahead of it.
func (h *harness) trafficRows(n int) []trafficRow {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := h.traffic()
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// traffic reads the traffic table from the file in insertion order.
func (h *harness) traffic() []trafficRow {
	h.t.Helper()
	db := h.openFile()
	rows, err := db.Query("SELECT COALESCE(session_id, ''), COALESCE(alias, ''), kind, result, COALESCE(claude_session_id, ''), COALESCE(claude_agent_id, ''), COALESCE(route, ''), COALESCE(request, ''), COALESCE(status, 0), bytes_up, bytes_down FROM traffic ORDER BY id")
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []trafficRow
	for rows.Next() {
		var row trafficRow
		if err := rows.Scan(&row.sessionID, &row.alias, &row.kind, &row.result, &row.claudeSession, &row.claudeAgent, &row.route, &row.request, &row.status, &row.bytesUp, &row.bytesDown); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// binding reads session id's current binding from the file.
func (h *harness) binding(id string) (alias string, boundAt, expires int64) {
	h.t.Helper()
	db := h.openFile()
	if err := db.QueryRow("SELECT alias, bound_at, lease_expires_at FROM bindings WHERE session_id = ?", id).Scan(&alias, &boundAt, &expires); err != nil {
		h.t.Fatal(err)
	}
	return alias, boundAt, expires
}

// paused reads the aliases paused at the harness clock, with their reasons.
func (h *harness) paused() map[string]string {
	h.t.Helper()
	db := h.openFile()
	rows, err := db.Query("SELECT alias, pause_reason FROM account_state WHERE paused_until > ?", h.clock.UnixMilli())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var alias, reason string
		if err := rows.Scan(&alias, &reason); err != nil {
			h.t.Fatal(err)
		}
		out[alias] = reason
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// openFile opens a second connection to the SQLite file, independent of the store.
func (h *harness) openFile() *sql.DB {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.path+"?_busy_timeout=10000")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = db.Close() })
	return db
}

// lookup returns the current set's account alias names, or nil.
func (c *Accounts) lookup(alias string) *account {
	return c.set.Load().lookup(alias)
}

// knownValueReplacer returns the current set's replacer.
func (c *Accounts) knownValueReplacer() *strings.Replacer {
	return c.set.Load().knownValueReplacer()
}

// newRecordStore returns a record store in dir that removes known's values; known may be nil.
func newRecordStore(dir string, known *strings.Replacer) *RecordStore {
	s := &RecordStore{dir: dir}
	if known != nil {
		s.known.Store(known)
	}
	return s
}
