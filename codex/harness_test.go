package codex

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
)

// Synthetic values of the tests. None of them is a real login, ID or key.
const (
	syntheticAccessToken = "synthetic-access-token-sample-a"
	syntheticAccountID   = "synthetic-account-id-sample-a"
	accountInstallation  = "0b8c7e2a-3f41-4d2e-9a6b-5c1d2e3f4a5b"
	clientInstallation   = "7d6c5b4a-3e2f-4a1b-8c9d-0e1f2a3b4c5d"
	syntheticSessionKey  = "synthetic-session-key-of-at-least-32-bytes"
	syntheticProxyUser   = "synthetic-proxy-user"
	syntheticProxyPass   = "synthetic-proxy-password"
	syntheticClaudeToken = "sk-ant-oat01-synthetic-claude-token"
)

// testTokenURL is the token endpoint of the tests. It is never dialed: the account's
// exit, the fake exit, answers every request sent through it.
const testTokenURL = "http://127.0.0.1:9/oauth/token"

// exchange is one request the fake exit received.
type exchange struct {
	path   string
	header http.Header
	body   string
}

// fakeExit stands in for an account's exit proxy and the upstream behind it: it
// receives the gateway's absolute-URI requests and answers them itself through answer.
type fakeExit struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []exchange
	answer func(w http.ResponseWriter, r *http.Request)
}

// startExit starts a fake exit that answers 200 with an empty stream unless answer is
// replaced.
func startExit(t *testing.T) *fakeExit {
	t.Helper()
	e := &fakeExit{answer: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.seen = append(e.seen, exchange{path: r.URL.Path, header: r.Header.Clone(), body: string(body)})
		answer := e.answer
		e.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		answer(w, r)
	}))
	t.Cleanup(e.server.Close)
	return e
}

// exchanges returns the requests the exit received.
func (e *fakeExit) exchanges() []exchange {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]exchange(nil), e.seen...)
}

// setAnswer replaces how the exit answers.
func (e *fakeExit) setAnswer(answer func(w http.ResponseWriter, r *http.Request)) {
	e.mu.Lock()
	e.answer = answer
	e.mu.Unlock()
}

// port returns the exit's port.
func (e *fakeExit) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(e.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// resolve resolves the tests' env:// references to synthetic values.
func resolve(_ context.Context, reference string) (string, error) {
	values := map[string]string{
		"env://SYNTHETIC_PROXY_USER": syntheticProxyUser,
		"env://SYNTHETIC_PROXY_PASS": syntheticProxyPass,
		"env://SYNTHETIC_CLAUDE":     syntheticClaudeToken,
	}
	if value, ok := values[reference]; ok {
		return value, nil
	}
	return "", errors.New("unknown reference")
}

// inventoryYAML is one account file with the exit at port and extra top-level lines.
func inventoryYAML(port, extra string) string {
	return "email: sample-a@example.test\naccess: gateway\nstatus: active\n" + extra +
		"proxy:\n  host: 127.0.0.1\n  port: " + port + "\n  expected_egress_ip: 127.0.0.1\n  username_ref: env://SYNTHETIC_PROXY_USER\n  password_ref: env://SYNTHETIC_PROXY_PASS\n"
}

// writeFile writes data to path with mode, creating its directory with dirMode.
func writeFile(t *testing.T, path, data string, dirMode, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// authJSON is a login in $CODEX_HOME's format with the synthetic token and account ID.
func authJSON() string {
	data, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens":         map[string]string{"id_token": "synthetic-id-token", "access_token": syntheticAccessToken, "refresh_token": "synthetic-refresh-token", "account_id": syntheticAccountID},
		"last_refresh":   "2026-10-19T00:00:00Z",
	})
	return string(data)
}

// gateway is a Codex gateway over one account, sample-a, behind a fake exit.
type gateway struct {
	t        *testing.T
	exit     *fakeExit
	store    *session.Store
	accounts *claude.Accounts
	entry    *claude.Entrypoint
	dir      string           // the temporary directory with inventory/ and accounts/
	server   *httptest.Server // the reverse proxy
	forward  *httptest.Server // the forward proxy
	token    string           // a session credential of this gateway
}

// newGateway starts a Codex gateway with sample-a, whose login and inventory file are
// written to a temporary directory, on the pro plan, and issues one session.
func newGateway(t *testing.T) *gateway {
	t.Helper()
	return newGatewayWith(t, authJSON())
}

// newGatewayWith starts newGateway's gateway with login as sample-a's auth.json, or
// without an account directory when login is empty.
func newGatewayWith(t *testing.T, login string) *gateway {
	t.Helper()
	dir := t.TempDir()
	exit := startExit(t)
	writeFile(t, filepath.Join(dir, "inventory", "sample-a.yaml"), inventoryYAML(exit.port(t), "installation_id: "+accountInstallation+"\n"), 0o700, 0o600)
	if err := os.Mkdir(filepath.Join(dir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if login != "" {
		writeFile(t, filepath.Join(dir, "accounts", "sample-a", authFile), login, 0o700, 0o600)
	}
	provider := New(filepath.Join(dir, "accounts"), testTokenURL)
	set, err := claude.LoadAccounts(t.Context(), filepath.Join(dir, "inventory"), provider, resolve, 4)
	if err != nil {
		t.Fatal(err)
	}
	form, err := NewSessionForm([]byte(syntheticSessionKey))
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(t.Context(), filepath.Join(dir, "codex.sqlite"), session.Config{Form: form})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SetPlanSchedule(t.Context(), session.PlanPeriod{Alias: "sample-a", Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	accounts := claude.NewAccounts(set, store)
	t.Cleanup(accounts.CloseIdleConnections)
	// The upstream is never dialed: the account's transport sends every request to the
	// exit, which answers it.
	upstream, err := url.Parse("http://127.0.0.1:9/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint, err := claude.NewHandler(upstream, provider, "", store, accounts, claude.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	server, forward := httptest.NewServer(entrypoint.Reverse), httptest.NewServer(entrypoint.Forward)
	t.Cleanup(server.Close)
	t.Cleanup(forward.Close)
	issued, err := store.Issue(t.Context(), "test-machine", session.LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{t: t, exit: exit, store: store, accounts: accounts, entry: entrypoint, dir: dir, server: server, forward: forward, token: issued.Token}
}

// connect sends CONNECT target to the forward proxy at address with token as the proxy
// password and returns the status.
func connect(t *testing.T, address, target, token string) int {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	authorization := base64.StdEncoding.EncodeToString([]byte("session:" + token))
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\nProxy-Authorization: Basic "+authorization+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// do sends a request to the gateway with the session credential, unless header sets
// Authorization, and returns the status and body.
func (g *gateway) do(method, path, body string, header http.Header) (int, string) {
	g.t.Helper()
	req, err := http.NewRequestWithContext(g.t.Context(), method, g.server.URL+path, strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	for name, values := range header {
		req.Header[name] = values
	}
	res, err := g.server.Client().Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	data, err := io.ReadAll(res.Body)
	if closeErr := res.Body.Close(); err != nil || closeErr != nil {
		g.t.Fatal(err, closeErr)
	}
	return res.StatusCode, string(data)
}
