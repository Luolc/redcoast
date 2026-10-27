package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
)

// Synthetic values of the renewal tests. None of them is a real login.
const (
	syntheticRefreshToken = "synthetic-refresh-token-0"
	syntheticEmail        = "sample-a@example.test"
)

// unsignedJWT is a JWT with claims and a placeholder signature, which the gateway
// never verifies.
func unsignedJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".synthetic-signature"
}

// idToken is an id_token with email and the ChatGPT account ID accountID.
func idToken(email, accountID string) string {
	return unsignedJWT(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
}

// loginJSON is a login in auth.json's format, with a field the gateway does not use.
func loginJSON(access, refresh, id, accountID string) string {
	data, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens":         map[string]string{"id_token": id, "access_token": access, "refresh_token": refresh, "account_id": accountID},
		"last_refresh":   fixtureLastRefresh,
	})
	return string(data)
}

// tokenEndpoint answers refreshes at the fake exit's /oauth/token: each request is
// recorded and answered by answer, and every other request gets 200 with an empty body.
type tokenEndpoint struct {
	mu       sync.Mutex
	requests []map[string]string
	answer   func(n int, request map[string]string) (int, string)
}

// install makes e the fake exit's token endpoint and has the upstream answer
// inference with status.
func (e *tokenEndpoint) install(exit *fakeExit, upstream func() int) {
	exit.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			w.WriteHeader(upstream())
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]string
		_ = json.Unmarshal(body, &request)
		e.mu.Lock()
		e.requests = append(e.requests, request)
		n := len(e.requests)
		e.mu.Unlock()
		status, answer := e.answer(n, request)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	})
}

// seen returns the refreshes the endpoint received.
func (e *tokenEndpoint) seen() []map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]string(nil), e.requests...)
}

// renewedAnswer is the token endpoint's answer to the n-th refresh: access-n and
// refresh-n, with an id_token of sample-a's account.
func renewedAnswer(n int) string {
	data, _ := json.Marshal(map[string]string{
		"access_token":  "synthetic-access-" + string(rune('0'+n)),
		"refresh_token": "synthetic-refresh-token-" + string(rune('0'+n)),
		"id_token":      idToken(syntheticEmail, syntheticAccountID),
	})
	return string(data)
}

// eventually waits up to five seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pauseReason returns sample-a's pause reason.
func (g *gateway) pauseReason() string {
	g.t.Helper()
	reason, err := g.store.PauseReason(context.Background(), "sample-a")
	if err != nil {
		g.t.Fatal(err)
	}
	return reason
}

// runRenewals runs the gateway's renewals until the test ends.
func (g *gateway) runRenewals() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.entry.RunRenewals(ctx, time.Hour)
	}()
	g.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestImport checks the import through the management interface. An account without a
// login loads, its requests get 502 with nothing sent upstream, and the first sweep
// pauses it for a new import. A login of another email, or whose account ID is not
// its id_token's, is refused (400) with nothing written or sent. A valid login is
// written (0600 in a 0700 directory) and renewed at once with the Codex CLI's refresh
// request; the renewed login replaces it on disk, keeping the fields the gateway does
// not use, ends the pause and is the one inference carries. A login of another
// ChatGPT account is then refused, and a login whose renewal is refused gets 409 and
// pauses the account for a new import. No answer carries a token.
func TestImport(t *testing.T) {
	g := newGatewayWith(t, "")
	endpoint := &tokenEndpoint{answer: func(n int, _ map[string]string) (int, string) { return http.StatusOK, renewedAnswer(n) }}
	endpoint.install(g.exit, func() int { return http.StatusOK })
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader()); status != http.StatusBadGateway || len(g.exit.exchanges()) != 0 {
		t.Fatalf("without a login: status %d, %d requests upstream; want 502 and none", status, len(g.exit.exchanges()))
	}
	g.runRenewals()
	eventually(t, "the pause for a new import", func() bool { return g.pauseReason() == claude.NeedsImportReason })
	admin := claude.NewAdmin(g.accounts, g.store, nil)
	admin.Import = g.entry.Import
	server := httptest.NewServer(admin.Handler())
	t.Cleanup(server.Close)
	importLogin := func(login string) (int, string) {
		t.Helper()
		res, err := server.Client().Post(server.URL+"/accounts/sample-a/import", "application/json", strings.NewReader(login))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		for _, value := range []string{syntheticAccessToken, syntheticRefreshToken, "synthetic-access-", "synthetic-refresh-token-"} {
			if strings.Contains(string(data), value) {
				t.Fatalf("the answer %q carries a token", data)
			}
		}
		return res.StatusCode, string(data)
	}
	path := filepath.Join(g.dir, "accounts", "sample-a", authFile)
	for _, test := range []struct{ name, login, problem string }{
		{"other_email", loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken("sample-b@example.test", syntheticAccountID), syntheticAccountID), "its email is not the account's"},
		{"account_id_not_the_id_token's", loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, "synthetic-account-id-other"), syntheticAccountID), "not the id_token's chatgpt_account_id"},
		{"id_token_not_a_jwt", loginJSON(syntheticAccessToken, syntheticRefreshToken, "synthetic-id-token", syntheticAccountID), "tokens.id_token: not a JWT"},
	} {
		if status, body := importLogin(test.login); status != http.StatusBadRequest || !strings.Contains(body, test.problem) {
			t.Fatalf("%s: %d %s", test.name, status, body)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || len(endpoint.seen()) != 0 {
			t.Fatalf("%s: a refused login was written or renewed", test.name)
		}
	}
	if status, body := importLogin(loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(strings.ToUpper(syntheticEmail), syntheticAccountID), syntheticAccountID)); status != http.StatusOK {
		t.Fatalf("valid login: %d %s", status, body)
	}
	want := map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": syntheticRefreshToken, "refresh_reason": "unauthorized_recovery"}
	if seen := endpoint.seen(); len(seen) != 1 || len(seen[0]) != len(want) || seen[0]["client_id"] != want["client_id"] || seen[0]["grant_type"] != want["grant_type"] ||
		seen[0]["refresh_token"] != want["refresh_token"] || seen[0]["refresh_reason"] != want["refresh_reason"] {
		t.Fatalf("token endpoint got %v, want one %v", seen, want)
	}
	for file, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want mode %o", file, info, err, mode)
		}
	}
	doc, err := readAuth(filepath.Dir(path))
	if err != nil || doc.Tokens.AccessToken != "synthetic-access-1" || doc.Tokens.RefreshToken != "synthetic-refresh-token-1" || doc.Tokens.AccountID != syntheticAccountID ||
		string(doc.raw["auth_mode"]) != `"chatgpt"` || doc.LastRefresh == fixtureLastRefresh {
		t.Fatalf("stored login after the renewal: %v", err)
	}
	if reason := g.pauseReason(); reason != "" {
		t.Fatalf("paused for %q after the import", reason)
	}
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader()); status != http.StatusOK {
		t.Fatalf("inference after the import: status %d", status)
	}
	if last := g.exit.exchanges()[len(g.exit.exchanges())-1]; last.header.Get("Authorization") != "Bearer synthetic-access-1" {
		t.Fatal("inference does not carry the renewed token")
	}
	other := loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, "synthetic-account-id-other"), "synthetic-account-id-other")
	if status, body := importLogin(other); status != http.StatusBadRequest || !strings.Contains(body, "not the stored login's account") {
		t.Fatalf("another account's login: %d %s", status, body)
	}
	endpoint.mu.Lock()
	endpoint.answer = func(int, map[string]string) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant","error_description":"` + syntheticRefreshToken + `"}`
	}
	endpoint.mu.Unlock()
	if status, body := importLogin(loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID)); status != http.StatusConflict || !strings.Contains(body, "400 invalid_grant") {
		t.Fatalf("login whose renewal is refused: %d %s", status, body)
	}
	if reason := g.pauseReason(); reason != claude.NeedsImportReason {
		t.Fatalf("paused for %q after a refused renewal, want %s", reason, claude.NeedsImportReason)
	}
}

// TestUnauthorizedRenewal checks what an upstream 401 does at the Codex process: the
// account is paused (upstream_401) and one renewal queued. Three requests refused
// together queue one renewal, which ends the pause, even though the third 401 arrives
// only after that renewal settled; the control arm, a 401 to a request sent after that
// pause ended, queues a second. A refused renewal pauses the account for a new import,
// which the health endpoint names; a renewal that fails otherwise keeps the 401 pause.
func TestUnauthorizedRenewal(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer func(n int, request map[string]string) (int, string)
		reason string // sample-a's pause reason after the renewal
	}{
		{"renewed", func(n int, _ map[string]string) (int, string) { return http.StatusOK, renewedAnswer(n) }, ""},
		{"refused", func(int, map[string]string) (int, string) {
			return http.StatusUnauthorized, `{"error":{"code":"refresh_token_reused"}}`
		}, claude.NeedsImportReason},
		{"failed", func(int, map[string]string) (int, string) { return http.StatusServiceUnavailable, "" }, "upstream_401"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := newGateway(t)
			g.runRenewals()
			endpoint := &tokenEndpoint{answer: test.answer}
			var held sync.WaitGroup
			held.Add(3)
			release, late := make(chan struct{}), make(chan struct{})
			var arrive sync.Once
			t.Cleanup(func() { arrive.Do(func() { close(late) }) })
			var unauthorized atomic.Int32
			endpoint.install(g.exit, func() int {
				n := unauthorized.Add(1)
				if n <= 3 {
					held.Done()
					<-release
				}
				if n == 3 {
					<-late
				}
				return http.StatusUnauthorized
			})
			var clients sync.WaitGroup
			for range 3 {
				clients.Add(1)
				go func() {
					defer clients.Done()
					_, _ = g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader())
				}()
			}
			held.Wait()
			close(release)
			eventually(t, "the renewal", func() bool { return len(endpoint.seen()) >= 1 && g.pauseReason() == test.reason })
			arrive.Do(func() { close(late) })
			clients.Wait()
			eventually(t, "the renewal after the late 401", func() bool { return g.pauseReason() == test.reason })
			time.Sleep(100 * time.Millisecond)
			if seen := endpoint.seen(); len(seen) != 1 || seen[0]["refresh_token"] != "synthetic-refresh-token" || seen[0]["refresh_reason"] != "unauthorized_recovery" {
				t.Fatalf("token endpoint got %v, want one refresh of the stored token", seen)
			}
			if test.reason == "upstream_401" {
				return
			}
			if test.reason == claude.NeedsImportReason {
				res := httptest.NewRecorder()
				claude.NewEgress(g.accounts, g.store, codexUpstream).HealthHandler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/health", nil))
				if !strings.Contains(res.Body.String(), "sample-a: paused (needs_reimport)") {
					t.Fatalf("health %s", res.Body)
				}
				return
			}
			if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader()); status != http.StatusUnauthorized {
				t.Fatalf("control arm: status %d", status)
			}
			eventually(t, "the second renewal", func() bool { return len(endpoint.seen()) == 2 && g.pauseReason() == "" })
		})
	}
}

// processRenewer is one gateway process's renewer of sample-a, whose login is in
// accountsDir, renewing at tokenURL.
func processRenewer(t *testing.T, accountsDir, tokenURL string) renewer {
	t.Helper()
	l := &logins{accountsDir: accountsDir}
	source, err := l.account("sample-a", []byte("email: "+syntheticEmail+"\ninstallation_id: "+accountInstallation+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return renewer{logins: l, tokenURL: tokenURL, now: time.Now}
}

// rotatingEndpoint is a token endpoint that, like the real one, takes each refresh
// token once: the n-th refresh must present refresh token n-1 and gets token n, and a
// spent one gets 400 refresh_token_reused. Each refresh takes delay. It returns the
// count of renewals and of requests.
func rotatingEndpoint(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	var count, calls atomic.Int32
	current := syntheticRefreshToken
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		time.Sleep(delay)
		mu.Lock()
		defer mu.Unlock()
		if request["refresh_token"] != current {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"refresh_token_reused"}}`)
			return
		}
		n := int(count.Add(1))
		current = "synthetic-refresh-token-" + string(rune('0'+n))
		_, _ = io.WriteString(w, renewedAnswer(n))
	}))
	t.Cleanup(server.Close)
	return server, &count, &calls
}

// TestRenewSerialized checks that the renewals of one account are serialized across
// processes: two processes renew after the same 401 at once, and one refreshes while
// the other, holding the lock after it, reads the renewed login and sends nothing; both
// then serve the renewed token. The control arm, one process renewing twice, refreshes
// twice, the second time with the rotated refresh token.
func TestRenewSerialized(t *testing.T) {
	accountsDir := t.TempDir()
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID), 0o700, 0o600)
	server, count, _ := rotatingEndpoint(t, 50*time.Millisecond)
	first, second := processRenewer(t, accountsDir, server.URL+"/oauth/token"), processRenewer(t, accountsDir, server.URL+"/oauth/token")
	errs := make(chan error, 2)
	for _, r := range []renewer{first, second} {
		go func() { errs <- r.renew(t.Context(), "sample-a", server.Client(), time.Now()) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	for i, r := range []renewer{first, second} {
		if got := r.logins.lookup("sample-a").tokens.Load(); got == nil || got.access != "synthetic-access-1" {
			t.Fatalf("process %d serves %v, want the renewed token", i, got)
		}
	}
	if err := first.renew(t.Context(), "sample-a", server.Client(), time.Now()); err != nil || count.Load() != 2 {
		t.Fatalf("control arm: %v, %d refreshes, want 2", err, count.Load())
	}
}

// TestRenewFailures checks how a failed refresh is classified, as the Codex CLI does:
// a 401, a 400 with invalid_grant and the three refresh_token_* codes (in error,
// error.code or code) refuse the login, anything else is a failure to retry. Neither
// kind of error carries the answer's text, which can repeat a token.
func TestRenewFailures(t *testing.T) {
	echo := `"error_description":"` + syntheticRefreshToken + `"`
	for _, test := range []struct {
		status  int
		body    string
		refused bool
	}{
		{http.StatusUnauthorized, `{` + echo + `}`, true},
		{http.StatusBadRequest, `{"error":"invalid_grant",` + echo + `}`, true},
		{http.StatusBadRequest, `{"error":{"code":"refresh_token_expired"},` + echo + `}`, true},
		{http.StatusForbidden, `{"code":"refresh_token_invalidated",` + echo + `}`, true},
		{http.StatusBadRequest, `{"error":"REFRESH_TOKEN_REUSED"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_request",` + echo + `}`, false},
		{http.StatusForbidden, `{"error":"invalid_grant"}`, false},
		{http.StatusInternalServerError, syntheticRefreshToken, false},
		{http.StatusTooManyRequests, "", false},
	} {
		err := refreshFailure(test.status, []byte(test.body))
		if errors.Is(err, claude.ErrRenewalRefused) != test.refused || strings.Contains(err.Error(), syntheticRefreshToken) {
			t.Fatalf("%d %s: %v, want refused %v", test.status, test.body, err, test.refused)
		}
	}
}

// TestRenewNotSaved checks a renewal whose renewed login cannot be written: the
// refresh token on disk is spent, so the renewal succeeds with the renewed token served
// from memory and the account named as a problem, and each later renewal, without
// refreshing, only retries the write until it is saved. The control arm is the write
// once the directory is writable again.
func TestRenewNotSaved(t *testing.T) {
	accountsDir := t.TempDir()
	stored := loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID)
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), stored, 0o700, 0o600)
	server, count, _ := rotatingEndpoint(t, 0)
	r := processRenewer(t, accountsDir, server.URL+"/oauth/token")
	dir := filepath.Join(accountsDir, "sample-a")
	if _, err := os.Create(filepath.Join(dir, lockFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	for i, want := range []string{"", "still not saved"} {
		err := r.renew(t.Context(), "sample-a", server.Client(), time.Now())
		if want == "" && err != nil || want != "" && (err == nil || errors.Is(err, claude.ErrRenewalRefused) || !strings.Contains(err.Error(), want)) || count.Load() != 1 {
			t.Fatalf("try %d: %v after %d refreshes, want %q after one", i+1, err, count.Load(), want)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, authFile)); string(data) != stored {
			t.Fatal("the stored login changed")
		}
		if got := r.logins.lookup("sample-a").tokens.Load(); got.access != "synthetic-access-1" {
			t.Fatal("the process does not serve the renewed token")
		}
		if problems := r.logins.problems(); len(problems) != 1 || !strings.HasPrefix(problems[0], "sample-a: renewed login not saved") {
			t.Fatalf("problems %q", problems)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.renew(t.Context(), "sample-a", server.Client(), time.Time{}); err != nil || count.Load() != 1 {
		t.Fatalf("control arm: %v after %d refreshes", err, count.Load())
	}
	doc, err := readAuth(dir)
	if err != nil || doc.Tokens.AccessToken != "synthetic-access-1" || doc.Tokens.RefreshToken != "synthetic-refresh-token-1" || len(r.logins.problems()) != 0 {
		t.Fatalf("after the write: %v, problems %q", err, r.logins.problems())
	}
}

// TestRenewLocked checks that a renewal waits for another process's lock only as long
// as its context allows, sending nothing, and that another account's renewal goes on;
// the control arm renews once the lock is released.
func TestRenewLocked(t *testing.T) {
	accountsDir := t.TempDir()
	login := loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID)
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), login, 0o700, 0o600)
	writeFile(t, filepath.Join(accountsDir, "sample-b", authFile), login, 0o700, 0o600)
	server, count, _ := rotatingEndpoint(t, 0)
	r := processRenewer(t, accountsDir, server.URL+"/oauth/token")
	source, err := r.logins.account("sample-b", []byte("email: "+syntheticEmail+"\ninstallation_id: "+accountInstallation+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	release, err := lockAccount(t.Context(), filepath.Join(accountsDir, "sample-a"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.renew(ctx, "sample-a", server.Client(), time.Now()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "another process holds") || count.Load() != 0 {
			t.Fatalf("locked account: %v after %d refreshes", err, count.Load())
		}
	case <-time.After(5 * time.Second):
		// The renewal is left waiting on the lock, which release below frees.
		release()
		t.Fatal("the renewal of a locked account did not return within 5 s")
	}
	if err := r.renew(t.Context(), "sample-b", server.Client(), time.Now()); err != nil || count.Load() != 1 {
		t.Fatalf("other account: %v, %d refreshes", err, count.Load())
	}
	release()
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), loginJSON(syntheticAccessToken, "synthetic-refresh-token-1", idToken(syntheticEmail, syntheticAccountID), syntheticAccountID), 0o700, 0o600)
	if err := r.renew(t.Context(), "sample-a", server.Client(), time.Now()); err != nil || count.Load() != 2 {
		t.Fatalf("control arm: %v, %d refreshes", err, count.Load())
	}
}

// TestRenewIdentity checks the identity check of a renewed login: an id_token of
// another email or another ChatGPT account refuses the renewal, and neither the stored
// login nor the token served changes; the control arm renews.
func TestRenewIdentity(t *testing.T) {
	for _, test := range []struct {
		name, id string
		refused  bool
	}{
		{"other_email", idToken("sample-b@example.test", syntheticAccountID), true},
		{"other_account", idToken(syntheticEmail, "synthetic-account-id-other"), true},
		{"control", idToken(syntheticEmail, syntheticAccountID), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountsDir := t.TempDir()
			stored := loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID)
			writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), stored, 0o700, 0o600)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "synthetic-access-1", "refresh_token": "synthetic-refresh-token-1", "id_token": test.id})
			}))
			t.Cleanup(server.Close)
			r := processRenewer(t, accountsDir, server.URL+"/oauth/token")
			err := r.renew(t.Context(), "sample-a", server.Client(), time.Now())
			if errors.Is(err, claude.ErrRenewalRefused) != test.refused || (!test.refused && err != nil) {
				t.Fatalf("got %v, want refused %v", err, test.refused)
			}
			data, _ := os.ReadFile(filepath.Join(accountsDir, "sample-a", authFile))
			served := r.logins.lookup("sample-a").tokens.Load().access
			if test.refused && (string(data) != stored || served != syntheticAccessToken) || !test.refused && served != "synthetic-access-1" {
				t.Fatalf("stored login changed %v, serving the renewed token %v", string(data) != stored, served == "synthetic-access-1")
			}
		})
	}
}

// TestRenewNoRedirect checks that a renewal follows no redirect of the token endpoint,
// so the refresh token goes nowhere else: the import's renewal fails (502) and the exit
// sees no request to where the redirect points.
func TestRenewNoRedirect(t *testing.T) {
	g := newGatewayWith(t, "")
	g.exit.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			w.Header().Set("Location", "http://127.0.0.1:9/elsewhere")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	})
	err := g.entry.Import(t.Context(), "sample-a", []byte(loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID)))
	if err == nil || errors.Is(err, claude.ErrRenewalRefused) || !strings.Contains(err.Error(), "307") {
		t.Fatalf("got %v, want a failure on the 307", err)
	}
	for _, e := range g.exit.exchanges() {
		if e.path != "/oauth/token" {
			t.Fatalf("the exit saw a request to %s", e.path)
		}
	}
}

// TestRenewalsAfterRestart checks that a process starting its renewals takes up an
// account another process left paused for upstream_401, which no sweep would renew,
// and ends the pause with a renewal; the control arm, an operator's pause, stays and
// sends nothing.
func TestRenewalsAfterRestart(t *testing.T) {
	for _, test := range []struct {
		reason, after string
		refreshes     int
	}{
		{"upstream_401", "", 1},
		{"maintenance", "maintenance", 0},
	} {
		t.Run(test.reason, func(t *testing.T) {
			g := newGateway(t)
			endpoint := &tokenEndpoint{answer: func(n int, _ map[string]string) (int, string) { return http.StatusOK, renewedAnswer(n) }}
			endpoint.install(g.exit, func() int { return http.StatusOK })
			if err := g.store.Pause(t.Context(), "sample-a", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), test.reason); err != nil {
				t.Fatal(err)
			}
			g.runRenewals()
			eventually(t, "the renewals", func() bool { return len(endpoint.seen()) == test.refreshes && g.pauseReason() == test.after })
			time.Sleep(100 * time.Millisecond)
			if len(endpoint.seen()) != test.refreshes || g.pauseReason() != test.after {
				t.Fatalf("%d refreshes, paused for %q", len(endpoint.seen()), g.pauseReason())
			}
		})
	}
}

// TestRenewDue checks when a sweep renews, as the Codex CLI does: an access token that
// expires within five minutes, or, when its expiry cannot be read, a login last
// renewed more than eight days ago.
func TestRenewDue(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := renewer{now: func() time.Time { return now }}
	expiring := func(at time.Time) string { return unsignedJWT(map[string]any{"exp": at.Unix()}) }
	for _, test := range []struct {
		name, access, lastRefresh, want string
	}{
		{"expires_soon", expiring(now.Add(4 * time.Minute)), now.Format(time.RFC3339), "proactive_expiry"},
		{"expired", expiring(now.Add(-time.Hour)), now.Format(time.RFC3339), "proactive_expiry"},
		{"valid", expiring(now.Add(6 * time.Minute)), now.Add(-30 * 24 * time.Hour).Format(time.RFC3339), ""},
		{"old_opaque", syntheticAccessToken, now.Add(-9 * 24 * time.Hour).Format(time.RFC3339Nano), "scheduled"},
		{"recent_opaque", syntheticAccessToken, now.Add(-7 * 24 * time.Hour).Format(time.RFC3339), ""},
		{"opaque_without_time", syntheticAccessToken, "", ""},
	} {
		var doc authDoc
		doc.Tokens.AccessToken, doc.LastRefresh = test.access, test.lastRefresh
		if got := r.due(doc); got != test.want {
			t.Fatalf("%s: %q, want %q", test.name, got, test.want)
		}
	}
}

// TestRenewSettled checks a 401 that arrives after a renewal it should not repeat: a
// renewal queued for a refusal before the last renewal settled takes that renewal's
// outcome and sends nothing, the renewed token or the refusal alike. The control arm, a
// refusal after it, refreshes again.
func TestRenewSettled(t *testing.T) {
	accountsDir := t.TempDir()
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), loginJSON(syntheticAccessToken, syntheticRefreshToken, idToken(syntheticEmail, syntheticAccountID), syntheticAccountID), 0o700, 0o600)
	server, count, calls := rotatingEndpoint(t, 0)
	r := processRenewer(t, accountsDir, server.URL+"/oauth/token")
	refused := time.Now()
	if err := r.renew(t.Context(), "sample-a", server.Client(), refused); err != nil || count.Load() != 1 {
		t.Fatalf("first renewal: %v, %d refreshes", err, count.Load())
	}
	if err := r.renew(t.Context(), "sample-a", server.Client(), refused); err != nil || count.Load() != 1 {
		t.Fatalf("a refusal before the renewal: %v, %d refreshes, want the renewal's outcome and 1", err, count.Load())
	}
	if err := r.renew(t.Context(), "sample-a", server.Client(), time.Now()); err != nil || count.Load() != 2 {
		t.Fatalf("control arm: %v, %d refreshes, want 2", err, count.Load())
	}
	writeFile(t, filepath.Join(accountsDir, "sample-a", authFile), loginJSON("synthetic-access-2", "synthetic-refresh-token-spent", idToken(syntheticEmail, syntheticAccountID), syntheticAccountID), 0o700, 0o600)
	refused = time.Now()
	for i := range 2 {
		if err := r.renew(t.Context(), "sample-a", server.Client(), refused); !errors.Is(err, claude.ErrRenewalRefused) || calls.Load() != 3 {
			t.Fatalf("spent refresh token, try %d: %v, %d requests, want refused and 3", i+1, err, calls.Load())
		}
	}
}

// TestDashboardShowsCodexState checks what the dashboard shows of a Codex account: its
// 5h and 7d readings from the x-codex-* headers, a renewal after a 401 whose login
// could not be written (the account serves again, and the health names the unsaved
// login), and a pause for a new import. The control arm is the dashboard before them:
// healthy, no reading and no pause.
func TestDashboardShowsCodexState(t *testing.T) {
	g := newGateway(t)
	reader, err := session.OpenReadOnly(filepath.Join(g.dir, "codex.sqlite"), session.Config{}, claude.DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	egress := claude.NewEgress(g.accounts, g.store, codexUpstream)
	egress.Problems = g.provider.Problems
	dashboard := claude.NewDashboard(egress, claude.DashboardConfig{Reader: reader, Store: g.store, DBPath: filepath.Join(g.dir, "codex.sqlite")})
	type view struct {
		Health struct {
			Status  string   `json:"status"`
			Reasons []string `json:"reasons"`
		} `json:"health"`
		Accounts []struct {
			PauseReason string                   `json:"pause_reason"`
			Quota       map[string]session.Quota `json:"quota"`
		} `json:"accounts"`
	}
	read := func() view {
		t.Helper()
		res := httptest.NewRecorder()
		dashboard.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/dashboard.json", nil))
		var v view
		if err := json.Unmarshal(res.Body.Bytes(), &v); err != nil || len(v.Accounts) != 1 {
			t.Fatalf("dashboard %s: %v", res.Body, err)
		}
		return v
	}
	if v := read(); v.Health.Status != "ok" || v.Accounts[0].PauseReason != "" || len(v.Accounts[0].Quota) != 0 {
		t.Fatalf("control arm: %+v", v)
	}
	var upstream atomic.Int32
	upstream.Store(http.StatusOK)
	endpoint := &tokenEndpoint{answer: func(n int, _ map[string]string) (int, string) { return http.StatusOK, renewedAnswer(n) }}
	endpoint.install(g.exit, func() int { return int(upstream.Load()) })
	inner := g.exit.answer
	g.exit.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		for name, value := range map[string]string{"Primary-Used-Percent": "3", "Primary-Window-Minutes": "300", "Primary-Reset-At": "1700000000", "Secondary-Used-Percent": "40", "Secondary-Window-Minutes": "10080"} {
			w.Header().Set("X-Codex-"+name, value)
		}
		inner(w, r)
	})
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader()); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	dir := filepath.Join(g.dir, "accounts", "sample-a")
	if _, err := os.Create(filepath.Join(dir, lockFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	g.runRenewals()
	upstream.Store(http.StatusUnauthorized)
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, codexHeader()); status != http.StatusUnauthorized {
		t.Fatalf("status %d", status)
	}
	eventually(t, "the renewal", func() bool { return len(endpoint.seen()) == 1 && g.pauseReason() == "" })
	v := read()
	if v.Health.Status != "degraded" || !slices.ContainsFunc(v.Health.Reasons, func(r string) bool { return strings.HasPrefix(r, "sample-a: renewed login not saved") }) {
		t.Fatalf("health after an unsaved renewal %+v", v.Health)
	}
	if q := v.Accounts[0].Quota; q[session.Window5h].Utilization != 0.03 || q[session.Window7d].Utilization != 0.4 {
		t.Fatalf("quota %+v", q)
	}
	if err := g.store.Pause(t.Context(), "sample-a", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), claude.NeedsImportReason); err != nil {
		t.Fatal(err)
	}
	if v := read(); v.Accounts[0].PauseReason != claude.NeedsImportReason || !slices.Contains(v.Health.Reasons, "sample-a: paused (needs_reimport)") {
		t.Fatalf("after a pause for a new import %+v", v)
	}
}
