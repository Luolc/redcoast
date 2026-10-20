package claude

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAccountExit stands in for one account's Oxylabs port: an HTTP proxy that
// receives the gateway's absolute-URI requests and answers them itself. It remembers
// the bearer token and the proxy credentials of every request.
type fakeAccountExit struct {
	server *httptest.Server
	status int
	mu     sync.Mutex
	tokens []string
	proxy  []string
}

// startAccountExit starts a fake exit that answers every request with status and a
// JSON body naming the exit. A 401 body is in the API's error shape for a refused token.
func startAccountExit(t *testing.T, name string, status int) *fakeAccountExit {
	t.Helper()
	exit := &fakeAccountExit{status: status}
	exit.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		exit.mu.Lock()
		exit.tokens = append(exit.tokens, r.Header.Get("Authorization"))
		exit.proxy = append(exit.proxy, r.Header.Get("Proxy-Authorization"))
		exit.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := `{"exit":"` + name + `"}`
		if status == http.StatusUnauthorized {
			body = refusedTokenBody(name)
		}
		if _, err := io.WriteString(w, body); err != nil {
			return
		}
	}))
	t.Cleanup(exit.server.Close)
	return exit
}

// url returns the exit's address as the account's proxy URL.
func (e *fakeAccountExit) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(e.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// seen returns the bearer tokens the exit received.
func (e *fakeAccountExit) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.tokens...)
}

// twoAccounts returns a harness with accounts sample-a and sample-b behind two fake
// exits, in front of the fixed API origin.
func twoAccounts(t *testing.T, statusA, statusB int) (*harness, *fakeAccountExit, *fakeAccountExit) {
	t.Helper()
	h := newHarness(t)
	exitA, exitB := startAccountExit(t, "A", statusA), startAccountExit(t, "B", statusB)
	h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", exitA.url(t))
	h.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", exitB.url(t))
	h.token, h.sessionID = h.issue()
	return h, exitA, exitB
}

// post sends one inference request with the given session credential and Claude Code
// headers and returns the status and body.
func post(t *testing.T, h *harness, server *httptest.Server, token, body, claudeSession, claudeAgent string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,"+oauthBeta)
	if claudeSession != "" {
		req.Header.Set("X-Claude-Code-Session-Id", claudeSession)
	}
	if claudeAgent != "" {
		req.Header.Set("X-Claude-Code-Agent-Id", claudeAgent)
	}
	res, err := h.clientFor(token).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(res.Body)
	if err := res.Body.Close(); err != nil || readErr != nil {
		t.Fatal(err, readErr)
	}
	return res.StatusCode, string(data)
}

// TestSessionRouting (E3.3, E3.10 reverse-proxy side, E3.12 inference rows) checks the
// single entrypoint: two sessions bind to two accounts, each exit sees only its own
// account's token, every request has a traffic row with the binding's session and
// account, and unknown or revoked credentials are refused with no request upstream
// and no upstream status in their rows.
// Control arm: pausing one account moves its session to the other, and the exits'
// readings change with it.
func TestSessionRouting(t *testing.T) {
	h, exitA, exitB := twoAccounts(t, 200, 200)
	server := h.gateway("http://127.0.0.1:1")
	token2, session2 := h.issue()
	const claudeSession = "00000000-0000-4000-8000-000000000001"
	for i, request := range []struct{ token, agent, exit string }{{h.token, "", "A"}, {token2, "a0a0a0a0a0a0a0a0a", "B"}, {h.token, "", "A"}} {
		status, body := post(t, h, server, request.token, `{"messages":[]}`, claudeSession, request.agent)
		if status != 200 || body != `{"exit":"`+request.exit+`"}` {
			t.Fatalf("status=%d body=%q, want exit %s", status, body, request.exit)
		}
		// The row is written after the response; wait for it so the rows keep request order.
		h.trafficRows(i + 1)
	}
	tokenA, tokenB := "Bearer "+tokenOf(h.accounts.lookup("sample-a")), "Bearer "+tokenOf(h.accounts.lookup("sample-b"))
	if seen := exitA.seen(); len(seen) != 2 || seen[0] != tokenA || seen[1] != tokenA {
		t.Fatalf("exit A saw %q", seen)
	}
	if seen := exitB.seen(); len(seen) != 1 || seen[0] != tokenB {
		t.Fatalf("exit B saw %q", seen)
	}
	if alias, _, _ := h.binding(h.sessionID); alias != "sample-a" {
		t.Fatalf("session 1 bound to %s", alias)
	}
	if alias, _, _ := h.binding(session2); alias != "sample-b" {
		t.Fatalf("session 2 bound to %s", alias)
	}
	// Refused credentials: none, not of the session shape, never issued, revoked. No
	// exit sees them. Only the shape tells the second apart from the last two.
	if err := h.store.Revoke(context.Background(), token2, "client"); err != nil {
		t.Fatal(err)
	}
	refused := []struct{ token, body, result string }{
		{"", "session credential required\n", "auth_failed"},
		{"sk-ant-oat01-" + strings.Repeat("B", 43), notSessionCredential + "\n", "not_session"},
		{"sk-ant-gws-" + strings.Repeat("B", 43), "unknown session\n", "auth_failed"},
		{token2, "unknown session\n", "auth_failed"},
	}
	for _, c := range refused {
		if status, body := post(t, h, server, c.token, `{"messages":[]}`, "", ""); status != 401 || body != c.body {
			t.Fatalf("token %q: status=%d body=%q, want 401 %q", c.token, status, body, c.body)
		}
	}
	if len(exitA.seen()) != 2 || len(exitB.seen()) != 1 {
		t.Fatal("a refused credential reached an exit")
	}
	rows := h.traffic()
	if len(rows) != 7 {
		t.Fatalf("traffic rows=%d", len(rows))
	}
	for i, want := range []trafficRow{
		{sessionID: h.sessionID, alias: "sample-a", kind: "inference", result: "ok", claudeSession: claudeSession, status: 200},
		{sessionID: session2, alias: "sample-b", kind: "inference", result: "ok", claudeSession: claudeSession, claudeAgent: "a0a0a0a0a0a0a0a0a", status: 200},
		{sessionID: h.sessionID, alias: "sample-a", kind: "inference", result: "ok", claudeSession: claudeSession, status: 200},
	} {
		got := rows[i]
		got.bytesUp, got.bytesDown = 0, 0
		if got != want {
			t.Fatalf("row %d = %+v, want %+v", i, got, want)
		}
		if rows[i].bytesUp != int64(len(`{"messages":[]}`)) || rows[i].bytesDown != int64(len(`{"exit":"A"}`)) {
			t.Fatalf("row %d bytes = %d/%d", i, rows[i].bytesUp, rows[i].bytesDown)
		}
	}
	// A local refusal has no upstream status: the column is NULL, read back as 0.
	for i, row := range rows[3:] {
		if row.result != refused[i].result || row.status != 0 || row.sessionID != "" || row.alias != "" {
			t.Fatalf("refused request %d recorded as %+v, want result %s", i, row, refused[i].result)
		}
	}
	// Control arm: pause B; session 2 is revoked, so issue a third session, which would
	// have gone to B as the account with fewer active sessions.
	if err := h.store.Pause(context.Background(), "sample-b", indefinitePause, "test"); err != nil {
		t.Fatal(err)
	}
	token3, session3 := h.issue()
	if status, body := post(t, h, server, token3, `{"messages":[]}`, "", ""); status != 200 || body != `{"exit":"A"}` {
		t.Fatalf("paused account still chosen: status=%d body=%q", status, body)
	}
	if alias, _, _ := h.binding(session3); alias != "sample-a" {
		t.Fatalf("session 3 bound to %s", alias)
	}
	if len(exitA.seen()) != 3 || len(exitB.seen()) != 1 {
		t.Fatal("control arm did not move the request to exit A")
	}
}

// TestUpstreamUnauthorizedPausesAccount checks that a 401 from the upstream with the
// account's real token pauses that account: the response passes through unchanged and
// is not replayed, and the session's next request goes to the other account. Control
// arm: a 200 pauses nothing and the session stays.
func TestUpstreamUnauthorizedPausesAccount(t *testing.T) {
	for _, status := range []int{401, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, exitA, exitB := twoAccounts(t, status, 200)
			server := h.gateway("http://127.0.0.1:1")
			got, body := post(t, h, server, h.token, `{"messages":[]}`, "", "")
			if want := map[int]string{401: refusedTokenBody("A"), 200: `{"exit":"A"}`}[status]; got != status || body != want {
				t.Fatalf("first response status=%d body=%q", got, body)
			}
			// The row is written after the response; wait for it so the rows keep request order.
			h.trafficRows(1)
			got, body = post(t, h, server, h.token, `{"messages":[]}`, "", "")
			wantExit, wantAlias, wantB := "A", "sample-a", 0
			if status == 401 {
				wantExit, wantAlias, wantB = "B", "sample-b", 1
			}
			if got != 200 || body != `{"exit":"`+wantExit+`"}` {
				t.Fatalf("second response status=%d body=%q", got, body)
			}
			if len(exitA.seen()) != 2-wantB || len(exitB.seen()) != wantB {
				t.Fatalf("exit A saw %d requests and exit B %d", len(exitA.seen()), len(exitB.seen()))
			}
			paused := h.paused()
			if status == 401 && (len(paused) != 1 || paused["sample-a"] != "upstream_401") {
				t.Fatalf("paused=%v", paused)
			}
			if status == 200 && len(paused) != 0 {
				t.Fatalf("paused=%v on a 200", paused)
			}
			rows := h.trafficRows(2)
			if len(rows) != 2 || rows[0].status != status || rows[0].alias != "sample-a" || rows[1].alias != wantAlias {
				t.Fatalf("rows=%+v", rows)
			}
		})
	}
}

// TestInFlightStreamStaysOnAccount (E3.4) checks that pausing the bound account while a
// streamed response is in progress leaves that stream on the original account until it
// ends, while the session's next request goes to the other account.
func TestInFlightStreamStaysOnAccount(t *testing.T) {
	h := newHarness(t, "sample-a", "sample-b")
	release := make(chan struct{})
	var mu sync.Mutex
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		held := len(tokens) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if held {
			<-release
			if _, err := io.WriteString(w, "data: last\n\n"); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	server := h.gateway(upstream.URL)
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, first); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Pause(context.Background(), "sample-a", indefinitePause, "test"); err != nil {
		t.Fatal(err)
	}
	if status, _ := post(t, h, server, h.token, "", "", ""); status != 200 {
		t.Fatalf("request during the stream: status=%d", status)
	}
	close(release)
	rest, err := io.ReadAll(res.Body)
	if err != nil || string(rest) != "data: last\n\n" {
		t.Fatalf("stream after the pause: rest=%q err=%v", rest, err)
	}
	tokenA, tokenB := "Bearer "+tokenOf(h.accounts.lookup("sample-a")), "Bearer "+tokenOf(h.accounts.lookup("sample-b"))
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != tokenA || tokens[1] != tokenB {
		t.Fatalf("upstream saw %q", tokens)
	}
	if alias, _, _ := h.binding(h.sessionID); alias != "sample-b" {
		t.Fatalf("session bound to %s after the pause", alias)
	}
	// The stream's row is written when it ends, after the second request's row.
	rows := h.traffic()
	if len(rows) != 2 || rows[0].alias != "sample-b" || rows[1].alias != "sample-a" || rows[1].result != "ok" || rows[1].bytesDown != int64(len("data: first\n\ndata: last\n\n")) {
		t.Fatalf("rows=%+v", rows)
	}
}

// TestRestartKeepsSessionAndBinding (E3.9) checks that a session credential and its
// binding survive the gateway process: a new store on the same file accepts the
// credential and reports the same binding. Control arm: a store on another file
// refuses it.
func TestRestartKeepsSessionAndBinding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	h := newHarness(t, "sample-a", "sample-b")
	server := h.gateway(upstream.URL)
	if status, _ := post(t, h, server, h.token, "", "", ""); status != 204 {
		t.Fatalf("status=%d", status)
	}
	alias, boundAt, expires := h.binding(h.sessionID)
	server.Close()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(2 * time.Second)
	h.open()
	server = h.gateway(upstream.URL)
	if status, _ := post(t, h, server, h.token, "", "", ""); status != 204 {
		t.Fatalf("after restart: status=%d", status)
	}
	if again, boundAgain, expiresAgain := h.binding(h.sessionID); again != alias || boundAgain != boundAt || expiresAgain != expires {
		t.Fatalf("binding changed across restart: %s %d %d -> %s %d %d", alias, boundAt, expires, again, boundAgain, expiresAgain)
	}
	other := newHarness(t, "sample-a", "sample-b")
	if status, _ := post(t, other, other.gateway(upstream.URL), h.token, "", "", ""); status != 401 {
		t.Fatalf("another store accepted the credential: status=%d", status)
	}
}

// TestTrafficTableKeepsNoBody (E3.12) checks the whole SQLite file for a marker sent in
// a request body and in a response body: zero hits, while the session ID and the
// Claude Code session ID from the request header are found. A header value of another
// shape is dropped. The JSON and event-stream arms go through the usage meter: the row
// keeps the model and the token counts it reads and nothing of the text around them.
func TestTrafficTableKeepsNoBody(t *testing.T) {
	const marker = "synthetic-body-marker-0f9e8d7c"
	const usage = `"usage":{"input_tokens":11,"output_tokens":22}`
	arms := []struct {
		name, contentType, body string
		metered                 bool
	}{
		{"plain", "", `{"text":"` + marker + `"}`, false},
		{"json", "application/json", `{"type":"message","model":"claude-sample-1","content":[{"type":"text","text":"` + marker + `"}],` + usage + `}`, true},
		{"stream", "text/event-stream", `data: {"type":"message_start","message":{"model":"claude-sample-1","content":[],"usage":{"input_tokens":11,"output_tokens":1}}}` + "\n\n" +
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + marker + `"}}` + "\n\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":22}}` + "\n\n", true},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				if arm.contentType != "" {
					w.Header().Set("Content-Type", arm.contentType)
				}
				if _, err := io.WriteString(w, arm.body); err != nil {
					return
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			server := h.gateway(upstream.URL)
			const claudeSession = "abcdef01-2345-4678-89ab-cdef01234567"
			if status, _ := post(t, h, server, h.token, `{"messages":[{"role":"user","content":"`+marker+`"}]}`, claudeSession, "not a valid id!"); status != 200 {
				t.Fatalf("status=%d", status)
			}
			rows := h.trafficRows(1)
			if len(rows) != 1 || rows[0].claudeSession != claudeSession || rows[0].claudeAgent != "" {
				t.Fatalf("rows=%+v", rows)
			}
			if arm.metered {
				var model string
				var input, output int64
				if err := h.openFile().QueryRow("SELECT COALESCE(model, ''), COALESCE(input_tokens, -1), COALESCE(output_tokens, -1) FROM traffic").Scan(&model, &input, &output); err != nil {
					t.Fatal(err)
				}
				if model != "claude-sample-1" || input != 11 || output != 22 {
					t.Fatalf("usage row: model=%q input=%d output=%d", model, input, output)
				}
			}
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}
			var file []byte
			for _, suffix := range []string{"", "-wal"} {
				data, err := os.ReadFile(h.path + suffix)
				if err != nil && suffix == "" {
					t.Fatal(err)
				}
				file = append(file, data...)
			}
			if strings.Contains(string(file), marker) {
				t.Fatal("a body reached the SQLite file")
			}
			for _, kept := range []string{h.sessionID, claudeSession} {
				if !strings.Contains(string(file), kept) {
					t.Fatalf("control value %s not found in the file", kept)
				}
			}
			if strings.Contains(string(file), h.token) {
				t.Fatal("the session credential reached the SQLite file")
			}
			h.open()
		})
	}
}

// TestClaudeIDHeadersCannotCarryCredentials checks the boundary the two Claude Code
// ID headers cross: values of the observed shapes are stored and forwarded; a session
// credential or an account token placed in either header is neither stored, found
// anywhere in the SQLite file, nor forwarded upstream. The UUID and agent ID of the
// control request are found in the file.
func TestClaudeIDHeadersCannotCarryCredentials(t *testing.T) {
	type seen struct{ session, agent []string }
	received := make(chan seen, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- seen{r.Header.Values("X-Claude-Code-Session-Id"), r.Header.Values("X-Claude-Code-Agent-Id")}
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	server := h.gateway(upstream.URL)
	const uuid, agent = "abcdef01-2345-4678-89ab-cdef01234567", "a0a0a0a0a0a0a0a0a"
	accountToken := tokenOf(h.accounts.lookup("sample-a"))
	for i, request := range []struct{ session, agent string }{{uuid, agent}, {h.token, h.token}, {accountToken, uuid + " "}} {
		if status, _ := post(t, h, server, h.token, "", request.session, request.agent); status != 204 {
			t.Fatalf("status=%d", status)
		}
		// The row is written after the response; wait for it so the rows keep request order.
		h.trafficRows(i + 1)
	}
	first := <-received
	if len(first.session) != 1 || first.session[0] != uuid || len(first.agent) != 1 || first.agent[0] != agent {
		t.Fatalf("well-formed IDs not forwarded: %+v", first)
	}
	for range 2 {
		if got := <-received; len(got.session) != 0 || len(got.agent) != 0 {
			t.Fatalf("a credential-shaped ID header was forwarded: %+v", got)
		}
	}
	rows := h.traffic()
	if len(rows) != 3 || rows[0].claudeSession != uuid || rows[0].claudeAgent != agent {
		t.Fatalf("rows=%+v", rows)
	}
	for _, row := range rows[1:] {
		if row.claudeSession != "" || row.claudeAgent != "" {
			t.Fatalf("credential-shaped ID stored: %+v", row)
		}
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	var file []byte
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(h.path + suffix)
		if err != nil && suffix == "" {
			t.Fatal(err)
		}
		file = append(file, data...)
	}
	for _, kept := range []string{uuid, agent, h.sessionID} {
		if !strings.Contains(string(file), kept) {
			t.Fatalf("control value %s not found in the file", kept)
		}
	}
	for _, secret := range []string{h.token, accountToken} {
		if strings.Contains(string(file), secret) {
			t.Fatal("a credential reached the SQLite file through an ID header")
		}
	}
	h.open()
}

// TestClaudeIDShape checks which header values the traffic table keeps: the session
// ID's UUID and the agent ID's lowercase hex, nothing else.
func TestClaudeIDShape(t *testing.T) {
	for _, test := range []struct {
		name, value string
		kept        bool
	}{
		{"X-Claude-Code-Session-Id", "abcdef01-2345-4678-89ab-cdef01234567", true},
		{"X-Claude-Code-Session-Id", "ABCDEF01-2345-4678-89AB-CDEF01234567", true},
		{"X-Claude-Code-Session-Id", "a0a0a0a0a0a0a0a0a", false},
		{"X-Claude-Code-Session-Id", "sk-ant-gws-" + strings.Repeat("A", 43), false},
		{"X-Claude-Code-Session-Id", "", false},
		{"X-Claude-Code-Agent-Id", "a0a0a0a0a0a0a0a0a", true},
		{"X-Claude-Code-Agent-Id", "A0A0A0A0A0A0A0A0A", false},
		{"X-Claude-Code-Agent-Id", "abcdef01-2345-4678-89ab-cdef01234567", false},
		{"X-Claude-Code-Agent-Id", strings.Repeat("a", 33), false},
	} {
		header := http.Header{test.name: {test.value}}
		got := claudeID(header, test.name)
		if (got != "") != test.kept || (test.kept && got != test.value) || (!test.kept && header.Get(test.name) != "") {
			t.Fatalf("%s=%q: kept=%q header=%q", test.name, test.value, got, header.Get(test.name))
		}
	}
	// Two values are not one ID.
	header := http.Header{"X-Claude-Code-Agent-Id": {"a0a0a0a0a0a0a0a0a", "a0a0a0a0a0a0a0a0a"}}
	if claudeID(header, "X-Claude-Code-Agent-Id") != "" || len(header.Values("X-Claude-Code-Agent-Id")) != 0 {
		t.Fatal("repeated header kept")
	}
}

// TestTrafficStatusIsTheUpstreamStatus checks the status column's source: an upstream
// response's status, including a 101 the gateway then rejects, and NULL when the
// gateway answered without an upstream response (unreachable upstream).
func TestTrafficStatusIsTheUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.WriteHeader(http.StatusSwitchingProtocols)
		case "/v1/messages":
			w.WriteHeader(http.StatusTeapot)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	server := h.gateway(upstream.URL)
	for path, want := range map[string]int{"/v1/messages": http.StatusTeapot, "/v1/messages/count_tokens": http.StatusBadGateway} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := h.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil || res.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d", path, res.StatusCode, want)
		}
	}
	// Control arm: no upstream at all.
	gone := h.gateway("http://127.0.0.1:1")
	req, err := http.NewRequestWithContext(t.Context(), "POST", gone.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil || res.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable upstream: status=%d", res.StatusCode)
	}
	byResult := map[string]trafficRow{}
	for _, row := range h.traffic() {
		byResult[row.result+"/"+map[int]string{http.StatusTeapot: "teapot", http.StatusSwitchingProtocols: "upgrade", 0: "none"}[row.status]] = row
	}
	if len(byResult) != 3 {
		t.Fatalf("rows=%v", byResult)
	}
	if _, ok := byResult["ok/teapot"]; !ok {
		t.Fatal("relayed response did not record the upstream status")
	}
	if _, ok := byResult["upstream_failed/upgrade"]; !ok {
		t.Fatal("rejected upgrade did not record the upstream's 101")
	}
	if _, ok := byResult["upstream_failed/none"]; !ok {
		t.Fatal("unreachable upstream recorded a status")
	}
}

// refusedTokenBody is a 401 body in the API's error shape for a refused credential.
func refusedTokenBody(name string) string {
	return `{"type":"error","error":{"type":"authentication_error","message":"synthetic refusal ` + name + `"}}`
}

// send sends one request with the session credential and, when beta is set, the OAuth
// capability, and returns the status.
func send(t *testing.T, h *harness, server *httptest.Server, method, path string, beta bool) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if beta {
		req.Header.Set("Anthropic-Beta", oauthBeta)
	}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

// TestRefusedTokenPausesForAnHour checks which upstream 401s pause the account: only a
// refused token on an inference request that carried the OAuth capability, plain or
// gzip-encoded. That pause lasts credentialPause; once it ends, the account is chosen
// again. Control arms: a 401 without the capability, with another error type, or on
// the startup check pauses nothing, and the next request goes to the same account.
func TestRefusedTokenPausesForAnHour(t *testing.T) {
	gzipped := func(body string) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for _, arm := range []struct {
		name, method, path, encoding string
		beta                         bool
		body                         []byte
		pauses                       bool
	}{
		{"messages", "POST", "/v1/messages?beta=true", "", true, []byte(refusedTokenBody("A")), true},
		{"count_tokens", "POST", "/v1/messages/count_tokens?beta=true", "", true, []byte(refusedTokenBody("A")), true},
		{"gzip", "POST", "/v1/messages", "gzip", true, gzipped(refusedTokenBody("A")), true},
		{"no_oauth_beta", "POST", "/v1/messages", "", false, []byte(refusedTokenBody("A")), false},
		{"other_error_type", "POST", "/v1/messages", "", true, []byte(`{"type":"error","error":{"type":"permission_error","message":"x"}}`), false},
		{"not_json", "POST", "/v1/messages", "", true, []byte("unauthorized"), false},
		{"startup_check", "HEAD", "/api/hello", "", true, []byte(refusedTokenBody("A")), false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			h := newHarness(t)
			var refuse atomic.Bool
			refuse.Store(true)
			var seen atomic.Int32
			exit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Add(1)
				if !refuse.Load() {
					w.WriteHeader(http.StatusOK)
					return
				}
				if arm.encoding != "" {
					w.Header().Set("Content-Encoding", arm.encoding)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(arm.body)
			}))
			t.Cleanup(exit.Close)
			exitURL, err := url.Parse(exit.URL)
			if err != nil {
				t.Fatal(err)
			}
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", exitURL)
			server := h.gateway("http://127.0.0.1:1")
			if status := send(t, h, server, arm.method, arm.path, arm.beta); status != 401 {
				t.Fatalf("refusal: status=%d", status)
			}
			refuse.Store(false)
			paused := h.paused()
			if !arm.pauses {
				if len(paused) != 0 {
					t.Fatalf("paused=%v", paused)
				}
				if status := send(t, h, server, "POST", "/v1/messages", true); status != 200 || seen.Load() != 2 {
					t.Fatalf("next request: status=%d exit saw %d", status, seen.Load())
				}
				return
			}
			if paused["sample-a"] != "upstream_401" {
				t.Fatalf("paused=%v", paused)
			}
			if status := send(t, h, server, "POST", "/v1/messages", true); status != 503 || seen.Load() != 1 {
				t.Fatalf("during the pause: status=%d exit saw %d", status, seen.Load())
			}
			h.clock = h.clock.Add(credentialPause - time.Millisecond)
			if len(h.paused()) != 1 {
				t.Fatal("pause ended early")
			}
			h.clock = h.clock.Add(time.Millisecond)
			h.token, h.sessionID = h.issue()
			if status := send(t, h, server, "POST", "/v1/messages", true); status != 200 || seen.Load() != 2 {
				t.Fatalf("after the pause: status=%d exit saw %d", status, seen.Load())
			}
		})
	}
}

// TestRequestAllowlist checks that a request off the list is answered 403 without
// reaching the upstream and is recorded as forbidden with its method and path, while
// the listed requests are forwarded. Paths are compared escaped and exactly, so an
// encoded slash or a trailing one does not pass.
func TestRequestAllowlist(t *testing.T) {
	h, exit, _ := twoAccounts(t, 200, 200)
	server := h.gateway("http://127.0.0.1:1")
	for _, refused := range []struct{ method, path, row string }{
		{"GET", "/v1/messages", "GET /v1/messages"},
		{"POST", "/api/oauth/usage", "POST /api/oauth/usage"},
		{"POST", "/v1/messages/", "POST /v1/messages/"},
		{"POST", "/v1/messages%2Fcount_tokens", "POST /v1/messages%2Fcount_tokens"},
		{"POST", "/" + strings.Repeat("a", 300), "POST /" + strings.Repeat("a", maxRequestLine-len("POST /"))},
	} {
		if status := send(t, h, server, refused.method, refused.path, true); status != http.StatusForbidden {
			t.Fatalf("%s %s: status=%d", refused.method, refused.path, status)
		}
		rows := h.trafficRows(1)
		if row := rows[len(rows)-1]; row.result != "forbidden" || row.request != refused.row || row.status != 0 || row.alias != "" {
			t.Fatalf("%s %s recorded as %+v", refused.method, refused.path, row)
		}
	}
	if seen := exit.seen(); len(seen) != 0 {
		t.Fatalf("exit saw %d refused requests", len(seen))
	}
	// Control arm: the listed requests reach the exit and keep no request in their rows.
	for _, allowed := range []struct{ method, path string }{{"POST", "/v1/messages?beta=true"}, {"POST", "/v1/messages/count_tokens"}, {"HEAD", "/api/hello"}} {
		if status := send(t, h, server, allowed.method, allowed.path, true); status != 200 {
			t.Fatalf("%s %s: status=%d", allowed.method, allowed.path, status)
		}
	}
	if seen := exit.seen(); len(seen) != 3 {
		t.Fatalf("exit saw %d allowed requests, want 3", len(seen))
	}
	rows := h.trafficRows(8)
	for _, row := range rows[5:] {
		if row.result != "ok" || row.request != "" {
			t.Fatalf("allowed request recorded as %+v", row)
		}
	}
}

// TestRefusedRowsAreLimitedPerSource sends more refused requests from one address than
// a window records: only refusalRowsPerSource of them get a row, whether the
// credential is unknown or not of the session shape or the screen declines an upgrade
// with 426, a request with a valid session
// still does, and the next window records refusals again. Control arm: once
// the loopback address is over its limit, the first refusal from another address is
// still recorded, and the loopback address's next one is not.
func TestRefusedRowsAreLimitedPerSource(t *testing.T) {
	h, _, _ := twoAccounts(t, 200, 200)
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rt := h.router(target, accountTransport{})
	rt.provider.Screen = func(r *http.Request) (int, string) {
		if r.Header.Get("X-Synthetic-Upgrade") != "" {
			return http.StatusUpgradeRequired, "synthetic upgrade declined"
		}
		return 0, ""
	}
	server := httptest.NewServer(rt)
	t.Cleanup(server.Close)
	unknown := "sk-ant-gws-" + strings.Repeat("B", 43)
	// The last two are not of the session shape and count against the same limit.
	for i := range refusalRowsPerSource + 2 {
		token := unknown
		if i >= refusalRowsPerSource {
			token = "sk-ant-oat01-" + strings.Repeat("B", 43)
		}
		if status, _ := post(t, h, server, token, `{}`, "", ""); status != 401 {
			t.Fatalf("status=%d", status)
		}
	}
	// Declined upgrades count against the same limit too.
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Synthetic-Upgrade", "websocket")
		res, err := h.clientFor(unknown).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUpgradeRequired {
			t.Fatalf("upgrade: status=%d", res.StatusCode)
		}
	}
	if status, _ := post(t, h, server, h.token, `{}`, "", ""); status != 200 {
		t.Fatalf("valid session: status=%d", status)
	}
	rows := h.trafficRows(refusalRowsPerSource + 1)
	if len(rows) != refusalRowsPerSource+1 || rows[refusalRowsPerSource].result != "ok" {
		t.Fatalf("rows=%+v", rows)
	}
	// Another address, served in place so that the row is written before ServeHTTP returns.
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/messages", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.9:40000"
	req.Header.Set("Authorization", "Bearer "+unknown)
	recorder := httptest.NewRecorder()
	rt.ServeHTTP(recorder, req)
	if recorder.Code != 401 {
		t.Fatalf("other address: status=%d", recorder.Code)
	}
	if status, _ := post(t, h, server, unknown, `{}`, "", ""); status != 401 {
		t.Fatalf("status=%d", status)
	}
	rows = h.trafficRows(refusalRowsPerSource + 3)
	if len(rows) != refusalRowsPerSource+2 || rows[len(rows)-1].result != "auth_failed" {
		t.Fatalf("other address: rows=%+v", rows)
	}
	h.clock = h.clock.Add(refusalWindow)
	if status, _ := post(t, h, server, "", `{}`, "", ""); status != 401 {
		t.Fatalf("status=%d", status)
	}
	if rows := h.trafficRows(refusalRowsPerSource + 3); len(rows) != refusalRowsPerSource+3 || rows[len(rows)-1].result != "auth_failed" {
		t.Fatalf("next window: rows=%+v", rows)
	}
}

// flakyExit stands in for an account's exit whose mode a test switches. In "down" no
// connection to it can be made, and in "wait" getting one waits until the gateway
// gives up the request. Once connected, "ok" answers 200 with a body naming the exit,
// "close" drops the connection without an answer, "silent" sends no answer until the
// gateway gives up waiting for its head, and "upgrade" answers 101.
type flakyExit struct {
	server  *httptest.Server
	mode    atomic.Value // string
	waiting chan struct{}
}

// startFlakyExit starts a flaky exit in mode "ok".
func startFlakyExit(t *testing.T) *flakyExit {
	t.Helper()
	e := &flakyExit{waiting: make(chan struct{}, 1)}
	e.mode.Store("ok")
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		switch e.mode.Load().(string) {
		case "close":
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case "silent":
			<-r.Context().Done()
		case "upgrade":
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.WriteHeader(http.StatusSwitchingProtocols)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"exit":"A"}`)
		}
	}))
	t.Cleanup(e.server.Close)
	return e
}

// attach adds account alias to h behind e, whose mode then decides how the account's
// connections go. The account waits headTimeout for a response head instead of the
// production 30 s.
func (e *flakyExit) attach(t *testing.T, h *harness, alias, token string) {
	t.Helper()
	exitURL, err := url.Parse(e.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := h.addWithExit(alias, token, exitURL)
	a.transport.ResponseHeaderTimeout = headTimeout
	through, closed := a.transport.Proxy, &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	a.transport.Proxy = func(r *http.Request) (*url.URL, error) {
		switch e.mode.Load().(string) {
		case "down":
			return closed, nil
		case "wait":
			select {
			case e.waiting <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return through(r)
	}
}

// headTimeout is how long a flaky exit's account waits for a response head.
const headTimeout = 200 * time.Millisecond

// TestUpstreamFailuresPauseAccount checks that upstreamFailureRun requests in a row that
// do not get through an account's exit pause the account for upstreamFailurePause: no
// connection to the exit can be made, or once the exit has the whole request, with a
// declared length or chunked, it drops the connection or sends no response head in
// time. The session's next request rebinds
// to the other account, and once the pause ends the account is chosen again. Control
// arms pause nothing, and the session's next request stays on the account: one failure
// fewer; requests the client left while the gateway waited for a connection; a run
// broken by an upstream response (a 101 the gateway rejects); a malformed client body.
func TestUpstreamFailuresPauseAccount(t *testing.T) {
	type step struct {
		mode  string
		count int
	}
	for _, arm := range []struct {
		name   string
		steps  []step
		pauses bool
	}{
		{"run", []step{{"down", upstreamFailureRun}}, true},
		{"one_short", []step{{"down", upstreamFailureRun - 1}}, false},
		{"client_left", []step{{"wait", upstreamFailureRun}}, false},
		{"answered_between", []step{{"down", upstreamFailureRun - 1}, {"upgrade", 1}, {"down", upstreamFailureRun - 1}}, false},
		{"dropped", []step{{"close", upstreamFailureRun}}, true},
		{"dropped_chunked", []step{{"close_chunked", upstreamFailureRun}}, true},
		{"no_head", []step{{"silent", upstreamFailureRun}}, true},
		{"malformed_body", []step{{"malformed", upstreamFailureRun}}, false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			h := newHarness(t)
			exitA, exitB := startFlakyExit(t), startAccountExit(t, "B", 200)
			exitA.attach(t, h, "sample-a", "sk-ant-oat01-synthetic-token-a")
			h.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", exitB.url(t))
			h.token, h.sessionID = h.issue()
			server := h.gateway("http://127.0.0.1:1")
			sent := 0
			for _, s := range arm.steps {
				mode := s.mode
				switch mode {
				case "malformed":
					mode = "ok"
				case "close_chunked":
					mode = "close"
				}
				exitA.mode.Store(mode)
				for range s.count {
					switch s.mode {
					case "wait":
						leave(t, h, server, exitA)
					case "malformed":
						sendMalformed(t, h, server)
					case "close_chunked":
						if got := postChunked(t, h, server); got != http.StatusBadGateway {
							t.Fatalf("%s: status=%d", s.mode, got)
						}
					default:
						if got, _ := post(t, h, server, h.token, `{"messages":[]}`, "", ""); got != http.StatusBadGateway {
							t.Fatalf("%s: status=%d", s.mode, got)
						}
					}
					sent++
					// The pause is written before the row; wait for the row so the next
					// request sees it.
					if rows := h.trafficRows(sent); len(rows) != sent || rows[sent-1].result != "upstream_failed" || rows[sent-1].alias != "sample-a" {
						t.Fatalf("%s: rows=%+v", s.mode, rows)
					}
				}
			}
			exitA.mode.Store("ok")
			paused := h.paused()
			got, body := post(t, h, server, h.token, `{"messages":[]}`, "", "")
			if !arm.pauses {
				if len(paused) != 0 {
					t.Fatalf("paused=%v", paused)
				}
				if alias, _, _ := h.binding(h.sessionID); got != 200 || body != `{"exit":"A"}` || alias != "sample-a" || len(exitB.seen()) != 0 {
					t.Fatalf("next request: status=%d body=%q bound to %s, exit B saw %d", got, body, alias, len(exitB.seen()))
				}
				return
			}
			if len(paused) != 1 || paused["sample-a"] != upstreamFailedReason {
				t.Fatalf("paused=%v", paused)
			}
			if alias, _, _ := h.binding(h.sessionID); got != 200 || body != `{"exit":"B"}` || alias != "sample-b" {
				t.Fatalf("next request: status=%d body=%q bound to %s", got, body, alias)
			}
			h.clock = h.clock.Add(upstreamFailurePause - time.Millisecond)
			if len(h.paused()) != 1 {
				t.Fatal("pause ended early")
			}
			h.clock = h.clock.Add(time.Millisecond)
			if len(h.paused()) != 0 {
				t.Fatal("pause did not end")
			}
			h.token, h.sessionID = h.issue()
			if got, body := post(t, h, server, h.token, `{"messages":[]}`, "", ""); got != 200 || body != `{"exit":"A"}` {
				t.Fatalf("after the pause: status=%d body=%q", got, body)
			}
		})
	}
}

// leave sends one inference request without a body and gives it up once the gateway
// waits for a connection through exit, as a client that leaves does. Without a body
// the gateway notices at once that the client has gone.
func leave(t *testing.T, h *harness, server *httptest.Server, exit *flakyExit) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-exit.waiting:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := h.client().Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatalf("a request the client left got status %d", res.StatusCode)
	}
}

// postChunked sends one inference request with the session credential and a whole
// chunked body, and returns the status.
func postChunked(t *testing.T, h *harness, server *httptest.Server) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength, req.TransferEncoding = -1, []string{"chunked"}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

// sendMalformed sends one inference request whose chunked body is malformed, on a
// connection it keeps open until the answer arrives.
func sendMalformed(t *testing.T, h *harness, server *httptest.Server) {
	t.Helper()
	addr := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	request := "POST /v1/messages HTTP/1.1\r\nHost: " + addr + "\r\nAuthorization: Bearer " + h.token +
		"\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n{\"a\":\r\nzz\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
}

// TestGuardRefusalsPauseNothing checks that requests a provider's guard refuses, which
// the gateway answers 502 without reaching the upstream, never count against the
// account, however many arrive in a row; the next request reaches the upstream through
// the same account.
func TestGuardRefusalsPauseNothing(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	guarded := Claude
	guarded.Guard = func(out *http.Request) error {
		if out.Header.Get("X-Refuse") != "" {
			return errors.New("refused by the guard")
		}
		return nil
	}
	h := newHarness(t, "sample-a", "sample-b")
	rt := newRouter(h.store, h.accounts, target, guarded, accountTransport{})
	rt.now = func() time.Time { return h.clock }
	server := httptest.NewServer(rt)
	t.Cleanup(server.Close)
	for i := range upstreamFailureRun {
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Refuse", "1")
		res, err := h.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("refused request: status=%d", res.StatusCode)
		}
		if rows := h.trafficRows(i + 1); len(rows) != i+1 || rows[i].result != "upstream_failed" {
			t.Fatalf("rows=%+v", rows)
		}
	}
	if paused := h.paused(); len(paused) != 0 {
		t.Fatalf("paused=%v", paused)
	}
	if status := send(t, h, server, "POST", "/v1/messages", true); status != 200 || requests.Load() != 1 {
		t.Fatalf("next request: status=%d upstream saw %d", status, requests.Load())
	}
	if alias, _, _ := h.binding(h.sessionID); alias != "sample-a" {
		t.Fatalf("bound to %s", alias)
	}
}

// TestFailureRunWindow checks that a run counts only failures within
// upstreamFailureWindow: a failure that old drops out, so one more failure completes
// no run, while the same failures a millisecond closer together complete one.
func TestFailureRunWindow(t *testing.T) {
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, arm := range []struct {
		name  string
		last  time.Duration
		takes bool
	}{
		{"in_window", upstreamFailureWindow - time.Millisecond, true},
		{"out_of_window", upstreamFailureWindow, false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			var runs failureRuns
			for range upstreamFailureRun - 1 {
				if runs.fail("sample-a", start) {
					t.Fatal("a run completed early")
				}
			}
			if got := runs.fail("sample-a", start.Add(arm.last)); got != arm.takes {
				t.Fatalf("failure after %v completed a run: %v", arm.last, got)
			}
		})
	}
}

// TestLimitCutsPauseNothing checks that requests the gateway's own limits end while it
// waits for a connection never count against the account, however many arrive in a
// row; the next request goes through the same account.
func TestLimitCutsPauseNothing(t *testing.T) {
	h := newHarness(t)
	exit := startFlakyExit(t)
	exit.attach(t, h, "sample-a", "sk-ant-oat01-synthetic-token-a")
	h.add("sample-b", "sk-ant-oat01-synthetic-token-b")
	h.token, h.sessionID = h.issue()
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rt := h.router(target, accountTransport{})
	rt.limits.Idle = 50 * time.Millisecond
	server := httptest.NewServer(rt)
	t.Cleanup(server.Close)
	exit.mode.Store("wait")
	for i := range upstreamFailureRun {
		if got, _ := post(t, h, server, h.token, `{"messages":[]}`, "", ""); got != http.StatusBadGateway {
			t.Fatalf("cut request: status=%d", got)
		}
		if rows := h.trafficRows(i + 1); len(rows) != i+1 || rows[i].result != "idle_timeout" {
			t.Fatalf("rows=%+v", rows)
		}
	}
	if paused := h.paused(); len(paused) != 0 {
		t.Fatalf("paused=%v", paused)
	}
	exit.mode.Store("ok")
	if got, body := post(t, h, server, h.token, `{"messages":[]}`, "", ""); got != 200 || body != `{"exit":"A"}` {
		t.Fatalf("next request: status=%d body=%q", got, body)
	}
}
