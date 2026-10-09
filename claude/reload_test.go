package claude

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// exit is a fake account exit: an HTTP proxy that answers every proxied request itself
// and records the account token it carried, so which token reached the upstream is
// observable without an upstream. hold, when set, makes requests wait until it closes.
type exit struct {
	server    *httptest.Server
	mu        sync.Mutex
	tokens    []string
	passwords []string // the proxy passwords the requests carried
	hold      chan struct{}
	body      string // the response body; empty answers {}
}

// newExit starts the fake exit.
func newExit(t *testing.T) *exit {
	t.Helper()
	e := &exit{}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := basicUser(r.Header.Get("Proxy-Authorization"))
		if !ok || user != "synthetic-value-proxy-user" {
			http.Error(w, "proxy credentials", http.StatusProxyAuthRequired)
			return
		}
		e.mu.Lock()
		e.tokens = append(e.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		e.passwords = append(e.passwords, password)
		hold, body := e.hold, e.body
		e.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if body == "" {
			body = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(e.server.Close)
	return e
}

// basicUser decodes a Basic authorization value.
func basicUser(value string) (user, password string, ok bool) {
	encoded, found := strings.CutPrefix(value, "Basic ")
	if !found {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	user, password, ok = strings.Cut(string(decoded), ":")
	return user, password, ok
}

// seen returns the tokens the exit saw so far.
func (e *exit) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.tokens)
}

// reloadValues are the credential values behind the references the reload tests use.
var reloadValues = map[string]string{
	"op://example-vault/token-a/credential":       "sk-ant-oat01-synthetic-a-old",
	"op://example-vault/token-a-new/credential":   "sk-ant-oat01-synthetic-a-new",
	"op://example-vault/token-b/credential":       "sk-ant-oat01-synthetic-b",
	"op://example-vault/token-c/credential":       "sk-ant-oat01-synthetic-c",
	"op://example-proxies/proxy-account/username": "synthetic-value-proxy-user",
	"op://example-proxies/proxy-account/password": "synthetic-value-proxy-password",
	"op://example-proxies/proxy-account/rotated":  "synthetic-value-proxy-rotated",
}

// reloadHarness is a harness whose accounts come from an inventory directory on disk,
// so a test can rewrite the directory and reload, with every account's exit at the
// fake exit.
type reloadHarness struct {
	*harness
	exit        *exit
	dir         string
	gateway     *httptest.Server
	passwordRef string // the proxy password reference rewrite uses; empty is the default one
}

// newReloadHarness writes the account files (alias to token reference), loads them,
// starts the gateway and issues one session; every alias is on the pro plan.
func newReloadHarness(t *testing.T, accounts map[string]string) *reloadHarness {
	t.Helper()
	h := &reloadHarness{harness: newHarness(t), exit: newExit(t), dir: t.TempDir()}
	for _, alias := range []string{"sample-a", "sample-b", "sample-c"} {
		if err := h.store.SetPlanSchedule(t.Context(), session.PlanPeriod{Alias: alias, Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	h.rewrite(accounts)
	set, err := LoadAccounts(t.Context(), h.dir, h.resolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	h.accounts.set.Store(set)
	t.Cleanup(h.accounts.CloseIdleConnections)
	// The upstream is never reached: the fake exit answers in its place.
	h.gateway = h.harness.gateway("http://127.0.0.1:1")
	return h
}

// resolve is the test resolver over reloadValues.
func (h *reloadHarness) resolve(_ context.Context, reference string) (string, error) {
	value, known := reloadValues[reference]
	if !known {
		return "", errors.New("synthetic resolver: unknown reference")
	}
	return value, nil
}

// rewrite replaces the inventory directory's files with one active gateway account per
// entry, all behind the fake exit.
func (h *reloadHarness) rewrite(accounts map[string]string) {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(h.dir, entry.Name())); err != nil {
			h.t.Fatal(err)
		}
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(h.exit.server.URL, "http://"))
	for alias, tokenRef := range accounts {
		content := strings.Replace(inventoryYAML(alias, "active", "gateway", tokenRef, port), "host: proxy.example.test", "host: "+host, 1)
		if h.passwordRef != "" {
			content = strings.Replace(content, "op://example-proxies/proxy-account/password", h.passwordRef, 1)
		}
		if err := os.WriteFile(filepath.Join(h.dir, alias+".yaml"), []byte(content), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

// reload reloads from the directory with verify and returns the report.
func (h *reloadHarness) reload(verify EgressCheck) (Reload, error) {
	return h.accounts.Reload(h.t.Context(), h.dir, h.resolve, verify)
}

// send sends one inference request with the session credential and returns the status.
func (h *reloadHarness) send(token string) int {
	h.t.Helper()
	res, err := h.clientFor(token).Post(h.gateway.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

// history returns the reasons of session id's history rows in order, with a marker for
// an open row.
func (h *reloadHarness) history(id string) []string {
	h.t.Helper()
	rows, err := h.openFile().Query("SELECT reason, to_ts IS NULL FROM binding_history WHERE session_id = ? ORDER BY id", id)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var reason string
		var open bool
		if err := rows.Scan(&reason, &open); err != nil {
			h.t.Fatal(err)
		}
		if open {
			reason += "(open)"
		}
		out = append(out, reason)
	}
	return out
}

// bound returns the alias session id is bound to, or "" without a binding.
func (h *reloadHarness) bound(id string) string {
	h.t.Helper()
	var alias string
	err := h.openFile().QueryRow("SELECT alias FROM bindings WHERE session_id = ?", id).Scan(&alias)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		h.t.Fatal(err)
	}
	return alias
}

var (
	onlyA = map[string]string{"sample-a": "op://example-vault/token-a/credential"}
	aAndB = map[string]string{"sample-a": "op://example-vault/token-a/credential", "sample-b": "op://example-vault/token-b/credential"}
	aNew  = map[string]string{"sample-a": "op://example-vault/token-a-new/credential"}
)

// TestReloadAddsAccount checks that a new session can land on an added account only
// after the reload: before it (the control arm) the same request lands on the old one.
func TestReloadAddsAccount(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatal("first session not on sample-a")
	}
	before, beforeID := h.issue()
	if h.send(before) != 200 || h.bound(beforeID) != "sample-a" {
		t.Fatal("control arm: a second session before the reload did not land on sample-a")
	}
	if r, err := h.reload(nil); err != nil || r.Added != nil || r.Removed != nil || r.Changed != nil || r.Rejected != nil || r.Moved != 0 || r.Unplaced != 0 {
		t.Fatalf("reload of an unchanged inventory: %+v %v", r, err)
	}
	h.rewrite(aAndB)
	if r, err := h.reload(nil); err != nil || !slices.Equal(r.Added, []string{"sample-b"}) || r.Moved != 0 {
		t.Fatalf("reload adding sample-b: %+v %v", r, err)
	}
	after, afterID := h.issue()
	if h.send(after) != 200 || h.bound(afterID) != "sample-b" {
		t.Fatalf("a session after the reload landed on %q, want sample-b", h.bound(afterID))
	}
	if seen := h.exit.seen(); seen[len(seen)-1] != reloadValues["op://example-vault/token-b/credential"] {
		t.Fatalf("exit saw %v, want sample-b's token last", seen)
	}
}

// TestReloadRemovesAccount checks that a session bound to a removed account is moved by
// the reload while a request already forwarding on it finishes there, and that the
// session's next request goes to the other account. Control arm: a reload that keeps
// the account leaves the session on it.
func TestReloadRemovesAccount(t *testing.T) {
	for _, remove := range []bool{true, false} {
		t.Run(map[bool]string{true: "removed", false: "kept"}[remove], func(t *testing.T) {
			h := newReloadHarness(t, aAndB)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			// A second request is held at the exit while the reload runs.
			hold := make(chan struct{})
			h.exit.mu.Lock()
			h.exit.hold = hold
			h.exit.mu.Unlock()
			inFlight := make(chan int, 1)
			go func() { inFlight <- h.send(h.token) }()
			deadline := time.Now().Add(5 * time.Second)
			for len(h.exit.seen()) < 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if remove {
				h.rewrite(map[string]string{"sample-b": "op://example-vault/token-b/credential"})
			}
			r, err := h.reload(nil)
			if err != nil {
				t.Fatal(err)
			}
			if remove && (!slices.Equal(r.Removed, []string{"sample-a"}) || r.Moved != 1) {
				t.Fatalf("reload removing sample-a: %+v", r)
			}
			if !remove && (r.Removed != nil || r.Moved != 0) {
				t.Fatalf("control arm reload: %+v", r)
			}
			h.exit.mu.Lock()
			h.exit.hold = nil
			h.exit.mu.Unlock()
			close(hold)
			if status := <-inFlight; status != 200 {
				t.Fatalf("request in flight across the reload: status=%d", status)
			}
			if h.send(h.token) != 200 {
				t.Fatal("request after the reload failed")
			}
			want := map[bool]string{true: "sample-b", false: "sample-a"}[remove]
			seen := h.exit.seen()
			if h.bound(h.sessionID) != want || seen[1] != reloadValues["op://example-vault/token-a/credential"] || seen[2] != reloadValues[aAndB[want]] {
				t.Fatalf("bound to %s, exit saw %v, want the held request on sample-a and the next on %s", h.bound(h.sessionID), seen, want)
			}
		})
	}
}

// TestReloadChangesToken checks that after a token change the exit sees the new token,
// that the sessions bound to the account were moved first, and (control arm) that a
// reload without the change leaves the old token in use.
func TestReloadChangesToken(t *testing.T) {
	for _, change := range []bool{true, false} {
		t.Run(map[bool]string{true: "changed", false: "unchanged"}[change], func(t *testing.T) {
			h := newReloadHarness(t, aAndB)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			if change {
				h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
			}
			r, err := h.reload(nil)
			if err != nil {
				t.Fatal(err)
			}
			if change && (!slices.Equal(r.Changed, []string{"sample-a"}) || r.Moved != 1 || h.bound(h.sessionID) != "sample-b") {
				t.Fatalf("reload changing sample-a's token: %+v, session on %s", r, h.bound(h.sessionID))
			}
			if !change && (r.Changed != nil || r.Moved != 0 || h.bound(h.sessionID) != "sample-a") {
				t.Fatalf("control arm: %+v, session on %s", r, h.bound(h.sessionID))
			}
			// After the change the first session is active on sample-b, so a fresh session
			// lands on sample-a and goes out with the new token. Without the change the
			// first session's own next request still goes out with the old token.
			if change {
				token, id := h.issue()
				if h.send(token) != 200 || h.bound(id) != "sample-a" {
					t.Fatalf("fresh session landed on %q, want sample-a", h.bound(id))
				}
				if seen := h.exit.seen(); seen[len(seen)-1] != reloadValues["op://example-vault/token-a-new/credential"] {
					t.Fatalf("exit saw %v, want the new token last", seen)
				}
				return
			}
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("control arm: session left sample-a")
			}
			if seen := h.exit.seen(); seen[len(seen)-1] != reloadValues["op://example-vault/token-a/credential"] {
				t.Fatalf("control arm: exit saw %v, want the old token last", seen)
			}
		})
	}
}

// TestReloadMigratesIdleSession checks the migration of a session that sends nothing
// during the reload: its binding moves with a history row whose reason is reload and
// its first request afterwards lands on the other account. Control arm: a reload that
// only adds an account leaves the binding and its lease untouched.
func TestReloadMigratesIdleSession(t *testing.T) {
	for _, change := range []bool{true, false} {
		t.Run(map[bool]string{true: "token_changed", false: "account_added"}[change], func(t *testing.T) {
			h := newReloadHarness(t, aAndB)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			_, _, expires := h.binding(h.sessionID)
			files := map[string]string{"sample-a": "op://example-vault/token-a/credential", "sample-b": "op://example-vault/token-b/credential", "sample-c": "op://example-vault/token-c/credential"}
			if change {
				files["sample-a"] = "op://example-vault/token-a-new/credential"
			}
			h.rewrite(files)
			if _, err := h.reload(nil); err != nil {
				t.Fatal(err)
			}
			alias, _, expiresAfter := h.binding(h.sessionID)
			if change {
				if alias == "sample-a" || !slices.Equal(h.history(h.sessionID), []string{"new", "reload(open)"}) {
					t.Fatalf("idle session on %s with history %v, want moved with a reload row", alias, h.history(h.sessionID))
				}
				if h.send(h.token) != 200 || h.bound(h.sessionID) != alias || slices.Contains(h.exit.seen(), reloadValues["op://example-vault/token-a-new/credential"]) {
					t.Fatalf("first request after the reload: on %s, exit saw %v", h.bound(h.sessionID), h.exit.seen())
				}
				return
			}
			if alias != "sample-a" || expiresAfter != expires || !slices.Equal(h.history(h.sessionID), []string{"new(open)"}) {
				t.Fatalf("control arm: session on %s, lease %d -> %d, history %v", alias, expires, expiresAfter, h.history(h.sessionID))
			}
		})
	}
}

// TestReloadWithoutOtherCandidate checks a token change on the only account: the
// session loses its binding and is counted, and its next request selects the account
// anew with the new token.
func TestReloadWithoutOtherCandidate(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatal("session not on sample-a")
	}
	h.rewrite(aNew)
	r, err := h.reload(nil)
	if err != nil || !slices.Equal(r.Changed, []string{"sample-a"}) || r.Moved != 0 || r.Unplaced != 1 {
		t.Fatalf("got %+v %v, want one unplaced session", r, err)
	}
	if h.bound(h.sessionID) != "" || !slices.Equal(h.history(h.sessionID), []string{"new"}) {
		t.Fatalf("unplaced session: bound to %q, history %v", h.bound(h.sessionID), h.history(h.sessionID))
	}
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatalf("next request: status or binding wrong, bound to %q", h.bound(h.sessionID))
	}
	if seen := h.exit.seen(); seen[len(seen)-1] != reloadValues["op://example-vault/token-a-new/credential"] {
		t.Fatalf("exit saw %v, want the new token last", seen)
	}
}

// TestReloadWaitsForBindingSection checks the exclusive region: a request held between
// taking the candidates and Bind makes the reload wait, finishes on the old account
// object, and is then moved so that it never uses the new token. Control arm: with the
// region off, the same timing binds the session back to the old alias and the request
// goes out with the new token, the error the region prevents.
func TestReloadWaitsForBindingSection(t *testing.T) {
	for _, exclusion := range []bool{true, false} {
		t.Run(map[bool]string{true: "exclusive", false: "control_no_exclusion"}[exclusion], func(t *testing.T) {
			h := newReloadHarness(t, onlyA)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			entered, release := make(chan struct{}), make(chan struct{})
			h.accounts.noExclusion = !exclusion
			h.accounts.beforeBind = func() {
				entered <- struct{}{}
				<-release
			}
			requested := make(chan int, 1)
			go func() { requested <- h.send(h.token) }()
			<-entered
			h.accounts.beforeBind = nil
			h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
			reloaded := make(chan Reload, 1)
			go func() {
				r, err := h.reload(nil)
				if err != nil {
					t.Error(err)
				}
				reloaded <- r
			}()
			if exclusion {
				select {
				case <-reloaded:
					t.Fatal("reload finished while a binding section was in progress")
				case <-time.After(200 * time.Millisecond):
				}
			} else {
				<-reloaded
			}
			close(release)
			if status := <-requested; status != 200 {
				t.Fatalf("held request: status=%d", status)
			}
			if exclusion {
				<-reloaded
			}
			if h.send(h.token) != 200 {
				t.Fatal("request after the reload failed")
			}
			seen := h.exit.seen()
			newToken := reloadValues["op://example-vault/token-a-new/credential"]
			if exclusion && (seen[1] != reloadValues["op://example-vault/token-a/credential"] || h.bound(h.sessionID) != "sample-b" || slices.Contains(seen, newToken)) {
				t.Fatalf("exclusive: exit saw %v, session on %s; want the held request on the old token, then sample-b", seen, h.bound(h.sessionID))
			}
			if !exclusion && (seen[1] != newToken || h.bound(h.sessionID) != "sample-a") {
				t.Fatalf("control: exit saw %v, session on %s; want the held request to go out with the new token on sample-a", seen, h.bound(h.sessionID))
			}
		})
	}
}

// TestReloadMigrationFailure checks that a failed migration transaction leaves the set
// and the bindings as they were and reports the failure: the next request still goes
// out with the old token. Control arm: without the fault the session is moved.
func TestReloadMigrationFailure(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit_fails", false: "commit_succeeds"}[fail], func(t *testing.T) {
			h := newReloadHarness(t, aAndB)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			if fail {
				h.accounts.migrate = func(context.Context, []string, []string, string) (session.Migration, error) {
					return session.Migration{}, errors.New("synthetic: disk full")
				}
			}
			h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
			r, err := h.reload(nil)
			if fail {
				if err == nil || !strings.Contains(err.Error(), "accounts and bindings unchanged") || h.bound(h.sessionID) != "sample-a" {
					t.Fatalf("got %v, session on %s; want a reported failure with the binding kept", err, h.bound(h.sessionID))
				}
				if h.send(h.token) != 200 {
					t.Fatal("request after the failed reload")
				}
				if seen := h.exit.seen(); seen[1] != reloadValues["op://example-vault/token-a/credential"] || !slices.Equal(h.accounts.Candidates(), []string{"sample-a", "sample-b"}) {
					t.Fatalf("exit saw %v, candidates %v; want the old token and the old set", seen, h.accounts.Candidates())
				}
				return
			}
			if err != nil || r.Moved != 1 || h.bound(h.sessionID) != "sample-b" {
				t.Fatalf("control arm: %+v %v, session on %s", r, err, h.bound(h.sessionID))
			}
		})
	}
}

// TestReloadValidationFailure checks that an inventory with an unresolvable reference
// is refused with the alias and field named, the old set keeps serving and the binding
// stays; the control arm with the reference fixed reloads.
func TestReloadValidationFailure(t *testing.T) {
	h := newReloadHarness(t, aAndB)
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatal("session not on sample-a")
	}
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-missing/credential", "sample-b": "op://example-vault/token-b/credential"})
	_, err := h.reload(nil)
	if err == nil || !strings.Contains(err.Error(), "sample-a: oauth_token: cannot resolve") || !strings.Contains(err.Error(), "accounts unchanged") {
		t.Fatalf("got %v, want a refusal naming sample-a's oauth_token", err)
	}
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" || !slices.Equal(h.accounts.Candidates(), []string{"sample-a", "sample-b"}) {
		t.Fatal("old set or binding changed after a refused reload")
	}
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
	if r, err := h.reload(nil); err != nil || !slices.Equal(r.Changed, []string{"sample-a"}) {
		t.Fatalf("control arm with the reference fixed: %+v %v", r, err)
	}
}

// TestReloadEgressCheck checks the hook's semantics: an account the check refuses is
// left out of the new set and reported while the rest of the reload goes on; the
// control arm with the check accepting admits it.
func TestReloadEgressCheck(t *testing.T) {
	for _, refuse := range []bool{true, false} {
		t.Run(map[bool]string{true: "refused", false: "accepted"}[refuse], func(t *testing.T) {
			h := newReloadHarness(t, onlyA)
			if h.send(h.token) != 200 {
				t.Fatal("request before the reload failed")
			}
			h.rewrite(aAndB)
			var checked []string
			verify := func(_ context.Context, a *account) error {
				checked = append(checked, a.alias+"@"+a.exit.Host)
				if refuse && a.alias == "sample-b" {
					return errors.New("synthetic: exit IP differs from expected_egress_ip")
				}
				return nil
			}
			r, err := h.reload(verify)
			if err != nil {
				t.Fatal(err)
			}
			host := strings.TrimPrefix(h.exit.server.URL, "http://")
			if !slices.Equal(checked, []string{"sample-b@" + host}) {
				t.Fatalf("checked %v, want only the added account at its exit", checked)
			}
			if refuse && (!slices.Equal(r.Rejected, []string{"sample-b"}) || r.Added != nil || !slices.Equal(h.accounts.Candidates(), []string{"sample-a"})) {
				t.Fatalf("refused: %+v, candidates %v", r, h.accounts.Candidates())
			}
			if !refuse && (r.Rejected != nil || !slices.Equal(r.Added, []string{"sample-b"}) || !slices.Equal(h.accounts.Candidates(), []string{"sample-a", "sample-b"})) {
				t.Fatalf("accepted: %+v, candidates %v", r, h.accounts.Candidates())
			}
			if h.send(h.token) != 200 {
				t.Fatal("request after the reload failed")
			}
		})
	}
}

// TestReloadKeepsOldRedaction checks that capture redaction keeps the credentials of
// the set a reload replaced: a request forwarding on the old account across the reload
// still gets its response saved with the old proxy password removed, while the field
// that carried it is kept and the exit received the real old password. Control arm: no
// reload, same request, same redaction.
func TestReloadKeepsOldRedaction(t *testing.T) {
	for _, rotate := range []bool{true, false} {
		t.Run(map[bool]string{true: "password_rotated", false: "no_reload"}[rotate], func(t *testing.T) {
			h := newReloadHarness(t, onlyA)
			captureDir := filepath.Join(t.TempDir(), "capture")
			if err := os.Mkdir(captureDir, 0o700); err != nil {
				t.Fatal(err)
			}
			target, _ := url.Parse("http://127.0.0.1:1")
			entry, err := NewHandler(target, captureDir, h.store, h.accounts, DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			captured := httptest.NewServer(entry.Reverse)
			defer captured.Close()
			oldPassword := reloadValues["op://example-proxies/proxy-account/password"]
			hold := make(chan struct{})
			h.exit.mu.Lock()
			h.exit.hold, h.exit.body = hold, `{"model":"claude-`+oldPassword+`"}`
			h.exit.mu.Unlock()
			done := make(chan int, 1)
			go func() {
				res, err := h.clientFor(h.token).Post(captured.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
				if err != nil {
					done <- 0
					return
				}
				_ = res.Body.Close()
				done <- res.StatusCode
			}()
			deadline := time.Now().Add(5 * time.Second)
			for len(h.exit.seen()) < 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if rotate {
				h.passwordRef = "op://example-proxies/proxy-account/rotated"
				h.rewrite(onlyA)
				if r, err := h.reload(nil); err != nil || !slices.Equal(r.Changed, []string{"sample-a"}) {
					t.Fatalf("reload rotating the password: %+v %v", r, err)
				}
			}
			h.exit.mu.Lock()
			h.exit.hold = nil
			h.exit.mu.Unlock()
			close(hold)
			if status := <-done; status != 200 {
				t.Fatalf("held request: status=%d", status)
			}
			// The record is saved after the response went out; wait for the file.
			var files []string
			for deadline := time.Now().Add(5 * time.Second); len(files) == 0 && time.Now().Before(deadline); {
				files, _ = filepath.Glob(filepath.Join(captureDir, "*.json"))
				time.Sleep(5 * time.Millisecond)
			}
			if len(files) != 1 {
				t.Fatalf("capture files %v, want one", files)
			}
			if err := entry.Finish(t.Context()); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			h.exit.mu.Lock()
			passwords := slices.Clone(h.exit.passwords)
			h.exit.mu.Unlock()
			if !slices.Equal(passwords, []string{oldPassword}) {
				t.Fatalf("exit received proxy passwords %v, want the old one", passwords)
			}
			if strings.Contains(string(saved), oldPassword) || !strings.Contains(string(saved), `claude-`+knownCredential) {
				t.Fatalf("saved record: old password present=%t, redacted model field present=%t", strings.Contains(string(saved), oldPassword), strings.Contains(string(saved), `claude-`+knownCredential))
			}
		})
	}
}

// TestReloadSerializesReloads checks that a reload started while another is in progress
// waits for it and diffs against the set the first one published: the second one
// removes the first's new account, migrates the session and publishes a set that holds
// the session's account. A reload with a context that ends while waiting is abandoned.
func TestReloadSerializesReloads(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatal("session not on sample-a")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	slow := func(_ context.Context, a *account) error {
		if a.alias == "sample-c" {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a/credential", "sample-c": "op://example-vault/token-c/credential"})
	first := make(chan Reload, 1)
	go func() {
		r, err := h.reload(slow)
		if err != nil {
			t.Error(err)
		}
		first <- r
	}()
	<-entered
	// The inventory changes under the first reload; the second one must see it only
	// after the first published.
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
	second := make(chan Reload, 1)
	go func() {
		r, err := h.reload(nil)
		if err != nil {
			t.Error(err)
		}
		second <- r
	}()
	select {
	case <-second:
		t.Fatal("second reload finished while the first was in progress")
	case <-time.After(200 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.accounts.Reload(ctx, h.dir, h.resolve, nil); err == nil || !strings.Contains(err.Error(), "another reload in progress") {
		t.Fatalf("third reload with an ending context: %v, want abandoned", err)
	}
	close(release)
	r1, r2 := <-first, <-second
	if !slices.Equal(r1.Added, []string{"sample-c"}) || r1.Moved != 0 {
		t.Fatalf("first reload: %+v", r1)
	}
	if !slices.Equal(r2.Removed, []string{"sample-c"}) || !slices.Equal(r2.Changed, []string{"sample-a"}) || !slices.Equal(r2.Added, []string{"sample-b"}) || r2.Moved != 1 {
		t.Fatalf("second reload: %+v, want it to diff against the first's set and move the session", r2)
	}
	if bound := h.bound(h.sessionID); bound != "sample-b" || !slices.Contains(h.accounts.Candidates(), bound) {
		t.Fatalf("session on %q, candidates %v; want the session on an account of the published set", bound, h.accounts.Candidates())
	}
	if h.send(h.token) != 200 {
		t.Fatal("request after both reloads failed")
	}
}

// TestReloadAbandonedWait checks the region's waits: a reload that gives up while a
// binding section holds the region leaves nothing behind, so a new request completes
// while that section is still held; and a binding section queued behind a waiting
// reload leaves when its own context ends, as the queue error, instead of waiting out
// the reload.
func TestReloadAbandonedWait(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	if h.send(h.token) != 200 {
		t.Fatal("first request failed")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	h.accounts.beforeBind = func() {
		entered <- struct{}{}
		<-release
	}
	held := make(chan int, 1)
	go func() { held <- h.send(h.token) }()
	<-entered
	h.accounts.beforeBind = nil
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.accounts.Reload(ctx, h.dir, h.resolve, nil); err == nil || !strings.Contains(err.Error(), "did not finish in time") {
		t.Fatalf("reload against a held binding section: %v, want it to give up", err)
	}
	completed := make(chan int, 1)
	go func() { completed <- h.send(h.token) }()
	select {
	case status := <-completed:
		if status != 200 {
			t.Fatalf("request after the abandoned reload: status=%d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request after the abandoned reload still queued: the abandoned writer is still pending")
	}
	// Second property: while a reload is waiting, a new binding section queues and
	// leaves when its own context ends. Control arm: with no reload waiting, the same
	// section completes.
	waiting := make(chan error, 1)
	go func() {
		_, err := h.reload(nil)
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)
	queuedCtx, cancelQueued := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelQueued()
	started := time.Now()
	if _, _, err := h.accounts.bind(queuedCtx, h.token, session.LocalSource); !errors.Is(err, errBindingQueue) || time.Since(started) > time.Second {
		t.Fatalf("queued binding section: %v after %v, want the queue error at its own deadline", err, time.Since(started))
	}
	released.Do(func() { close(release) })
	if status := <-held; status != 200 {
		t.Fatalf("held request: status=%d", status)
	}
	if err := <-waiting; err != nil {
		t.Fatalf("reload after the section released: %v", err)
	}
	controlCtx, cancelControl := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelControl()
	if _, a, err := h.accounts.bind(controlCtx, h.token, session.LocalSource); err != nil || a == nil {
		t.Fatalf("control arm with no reload waiting: %v", err)
	}
}

// TestReloadCancelledBeforePublish checks that a reload whose context is already over
// when it reaches the exclusive region publishes nothing and reports the abandonment,
// even when there is no session to migrate; the control arm with a live context
// publishes.
func TestReloadCancelledBeforePublish(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancelled", false: "live"}[cancelled], func(t *testing.T) {
			h := newReloadHarness(t, onlyA)
			h.rewrite(aAndB)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// The egress check runs after the inventory loaded and before the region; the
			// context ends there, with nothing to migrate afterwards.
			verify := func(context.Context, *account) error {
				if cancelled {
					cancel()
				}
				return nil
			}
			r, err := h.accounts.Reload(ctx, h.dir, h.resolve, verify)
			published := slices.Contains(h.accounts.Candidates(), "sample-b")
			if cancelled && (err == nil || !strings.Contains(err.Error(), "accounts unchanged") || published) {
				t.Fatalf("cancelled: %+v %v, published=%t; want an abandonment with nothing published", r, err, published)
			}
			if !cancelled && (err != nil || !published) {
				t.Fatalf("live: %+v %v, published=%t", r, err, published)
			}
		})
	}
}
