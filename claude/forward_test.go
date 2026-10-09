package claude

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
)

// fakeExit is a stand-in for the account's proxy: it answers each CONNECT with status
// and, after a 200, echoes the tunnel bytes back. It remembers each request head.
type fakeExit struct {
	listener net.Listener
	status   int
	mu       sync.Mutex
	heads    []*http.Request
}

func startFakeExit(t *testing.T, status int) *fakeExit {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := &fakeExit{listener: listener, status: status}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go exit.serve(conn)
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error("fake exit close failed")
		}
	})
	return exit
}

func (e *fakeExit) serve(conn net.Conn) {
	defer closeQuietly(conn)
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	e.mu.Lock()
	e.heads = append(e.heads, request)
	e.mu.Unlock()
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n\r\n", e.status, http.StatusText(e.status)); err != nil || e.status != 200 {
		return
	}
	if _, err := io.Copy(conn, reader); err != nil {
		return
	}
}

// proxyAuthorization is the Proxy-Authorization value HTTPS_PROXY's userinfo
// session:<credential> produces.
func proxyAuthorization(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("session:"+token))
}

// connect opens a raw connection to the forward proxy at address and sends one request
// head: a CONNECT to target with authorization, or a GET when target is empty. It
// returns the connection, its reader and the response.
func connect(t *testing.T, address, target, authorization string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return connectOn(t, conn, target, authorization)
}

// connectOn is connect on a connection the caller opened.
func connectOn(t *testing.T, conn net.Conn, target, authorization string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	t.Cleanup(func() { closeQuietly(conn) })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	head := "GET http://api.example.test/ HTTP/1.1\r\nHost: api.example.test\r\n"
	if target != "" {
		head = "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	}
	if authorization != "" {
		head += "Proxy-Authorization: " + authorization + "\r\n"
	}
	if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return conn, reader, response
}

// TestForwardProxy checks every forward-proxy outcome against the saved capture record
// and the traffic row: a tunnel through the bound account's exit carries bytes both
// ways with the account's credentials, a rejecting or unreachable exit fails without
// any other connection, a missing or wrong session credential is refused with 407
// before any connection, and requests other than CONNECT are refused.
func TestForwardProxy(t *testing.T) {
	for _, test := range []struct {
		name       string
		exitStatus int  // 0 means no exit is listening
		connect    bool // false sends GET
		credential string
		status     int
		result     string
	}{
		{"ok", 200, true, "session", 200, "ok"},
		{"exit_rejects", 407, true, "session", 502, "egress_failed"},
		{"exit_unreachable", 0, true, "session", 502, "egress_failed"},
		{"not_connect", 200, false, "session", 405, "refused"},
		{"no_credential", 200, true, "", 407, "auth_failed"},
		{"unknown_credential", 200, true, "sk-ant-gws-" + strings.Repeat("C", 43), 407, "auth_failed"},
		{"not_basic", 200, true, "bearer", 407, "auth_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exit *fakeExit
			// A port that was just closed stands in for an exit that is down.
			closed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := closed.Addr().String()
			if err := closed.Close(); err != nil {
				t.Fatal(err)
			}
			if test.exitStatus != 0 {
				exit = startFakeExit(t, test.exitStatus)
				address = exit.listener.Addr().String()
			}
			h := newHarness(t)
			a := h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: address})
			a.proxyUsername, a.proxyPassword = "synthetic-user", "Synthetic-Password"
			h.token, h.sessionID = h.issue()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			store := newRecordStore(dir, h.accounts.knownValueReplacer())
			forward := newForwardProxy(h.store, h.accounts, store.writeConnect)
			var dialed []string
			dial := forward.dial
			forward.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
				dialed = append(dialed, target)
				return dial(ctx, network, target)
			}
			server := httptest.NewServer(forward)
			defer server.Close()
			authorization := ""
			switch test.credential {
			case "session":
				authorization = proxyAuthorization(h.token)
			case "bearer":
				authorization = "Bearer " + h.token
			case "":
			default:
				authorization = proxyAuthorization(test.credential)
			}
			target := "api.example.test:443"
			if !test.connect {
				target = ""
			}
			conn, reader, response := connect(t, server.Listener.Addr().String(), target, authorization)
			if response.StatusCode != test.status {
				t.Fatalf("forward proxy answered %d, want %d", response.StatusCode, test.status)
			}
			if test.status == 407 && !strings.HasPrefix(response.Header.Get("Proxy-Authenticate"), "Basic") {
				t.Fatal("407 without a Proxy-Authenticate challenge")
			}
			if test.result == "ok" {
				if _, err := io.WriteString(conn, "ping"); err != nil {
					t.Fatal(err)
				}
				echo := make([]byte, 4)
				if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
					t.Fatal("tunnel did not carry bytes both ways")
				}
			}
			closeQuietly(conn)
			if err := forward.wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			reaches := test.connect && test.credential == "session"
			if reaches && (len(dialed) != 1 || dialed[0] != address) {
				t.Fatalf("dialed %q, want only the account's exit %q", dialed, address)
			}
			if !reaches && len(dialed) != 0 {
				t.Fatal("a refused request opened a connection")
			}
			if exit != nil && reaches {
				exit.mu.Lock()
				heads := exit.heads
				exit.mu.Unlock()
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-user:Synthetic-Password"))
				if len(heads) != 1 || heads[0].Method != http.MethodConnect || heads[0].Host != "api.example.test:443" || heads[0].Header.Get("Proxy-Authorization") != want {
					t.Fatal("exit did not receive the CONNECT with the account's credentials")
				}
			}
			files, err := filepath.Glob(filepath.Join(dir, "connect-*.json"))
			if err != nil || len(files) != 1 {
				t.Fatal("forward request not recorded")
			}
			artifact, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var record connectRecord
			if err := json.Unmarshal(artifact, &record); err != nil {
				t.Fatal(err)
			}
			wantAccount := ""
			if reaches {
				wantAccount = "sample-test"
			}
			if record.Kind != "connect" || record.Account != wantAccount || record.Result != test.result {
				t.Fatalf("record %+v, want result %s for account %q", record, test.result, wantAccount)
			}
			if test.connect && record.Target != "api.example.test:443" {
				t.Fatal("CONNECT target not recorded")
			}
			if test.result == "ok" && (record.ExitStatus != 200 || record.BytesUp != 4 || record.BytesDown != 4) {
				t.Fatalf("tunnel record %+v, want exit 200 and 4 bytes each way", record)
			}
			if test.name == "exit_rejects" && record.ExitStatus != 407 {
				t.Fatal("exit status not recorded")
			}
			for _, secret := range []string{"Synthetic-Password", base64.StdEncoding.EncodeToString([]byte("synthetic-user:Synthetic-Password")), h.token} {
				if strings.Contains(string(artifact), secret) {
					t.Fatal("record kept a credential")
				}
			}
			rows := h.traffic()
			if len(rows) != 1 || rows[0].kind != "connect" || rows[0].result != test.result || rows[0].alias != wantAccount || rows[0].status != record.ExitStatus {
				t.Fatalf("traffic rows=%+v", rows)
			}
			if wantRoute := map[bool]string{true: "account", false: ""}[test.connect]; rows[0].route != wantRoute {
				t.Fatalf("traffic row route=%q, want %q", rows[0].route, wantRoute)
			}
			if reaches && rows[0].sessionID != h.sessionID || !reaches && rows[0].sessionID != "" {
				t.Fatalf("traffic row session=%q reaches=%v", rows[0].sessionID, reaches)
			}
		})
	}
}

// TestConnectRowsFollowTheBinding (E3.2, E3.3 CONNECT side, E3.12 CONNECT rows) checks
// two sessions on two accounts: each CONNECT, to an Anthropic host or any other, goes
// to the exit of the session's account, and its row names that session and account.
// Control arm: pausing account A moves the session's next tunnel to B, while the
// tunnel opened before the pause keeps flowing through A and is recorded on A.
func TestConnectRowsFollowTheBinding(t *testing.T) {
	exitA, exitB := startFakeExit(t, 200), startFakeExit(t, 200)
	h := newHarness(t)
	h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", &url.URL{Scheme: "http", Host: exitA.listener.Addr().String()})
	h.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", &url.URL{Scheme: "http", Host: exitB.listener.Addr().String()})
	h.token, h.sessionID = h.issue()
	token2, session2 := h.issue()
	forward := newForwardProxy(h.store, h.accounts, nil)
	server := httptest.NewServer(forward)
	defer server.Close()
	address := server.Listener.Addr().String()
	heads := func(exit *fakeExit) int {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return len(exit.heads)
	}
	// Session 1 (account A) tunnels to an Anthropic host and to another host; session 2
	// (account B) to another host. Every tunnel echoes four bytes.
	for _, tunnel := range []struct{ token, target string }{{h.token, "api.anthropic.com:443"}, {h.token, "example.invalid:8443"}, {token2, "example.invalid:8443"}} {
		conn, reader, response := connect(t, address, tunnel.target, proxyAuthorization(tunnel.token))
		if response.StatusCode != 200 {
			t.Fatalf("%s: status=%d", tunnel.target, response.StatusCode)
		}
		if _, err := io.WriteString(conn, "ping"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		closeQuietly(conn)
	}
	// Control arm: a tunnel opened on A stays on A after A is paused; the next one goes to B.
	held, heldReader, response := connect(t, address, "example.invalid:443", proxyAuthorization(h.token))
	if response.StatusCode != 200 {
		t.Fatalf("held tunnel: status=%d", response.StatusCode)
	}
	if err := h.store.Pause(context.Background(), "sample-a", indefinitePause, "test"); err != nil {
		t.Fatal(err)
	}
	moved, movedReader, response := connect(t, address, "example.invalid:443", proxyAuthorization(h.token))
	if response.StatusCode != 200 {
		t.Fatalf("tunnel after the pause: status=%d", response.StatusCode)
	}
	for _, tunnel := range []struct {
		conn   net.Conn
		reader *bufio.Reader
	}{{held, heldReader}, {moved, movedReader}} {
		if _, err := io.WriteString(tunnel.conn, "ping"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(tunnel.reader, make([]byte, 4)); err != nil {
			t.Fatal("tunnel did not carry bytes after the pause")
		}
		closeQuietly(tunnel.conn)
	}
	// Wrong credential: refused, no exit connection.
	if _, _, response := connect(t, address, "api.anthropic.com:443", proxyAuthorization("sk-ant-gws-"+strings.Repeat("D", 43))); response.StatusCode != 407 {
		t.Fatalf("wrong credential: status=%d", response.StatusCode)
	}
	if err := forward.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if heads(exitA) != 3 || heads(exitB) != 2 {
		t.Fatalf("exit A saw %d tunnels and exit B %d, want 3 and 2", heads(exitA), heads(exitB))
	}
	rows := h.traffic()
	if len(rows) != 6 {
		t.Fatalf("rows=%d", len(rows))
	}
	// Rows are compared as a multiset: the clients close in order, but the server-side
	// finish of each tunnel and its SQLite insert can still interleave, so the row order
	// proves nothing. Three tunnels on A for session 1 (two early, the held one); one on
	// B for session 2; the moved one on B for session 1; the refused request.
	type key struct{ session, alias, result string }
	got := map[key]int{}
	for _, row := range rows {
		got[key{row.sessionID, row.alias, row.result}]++
		if row.result == "ok" && (row.bytesUp != 4 || row.bytesDown != 4 || row.status != 200) {
			t.Fatalf("row bytes/status = %d/%d/%d", row.bytesUp, row.bytesDown, row.status)
		}
	}
	want := map[key]int{{h.sessionID, "sample-a", "ok"}: 3, {session2, "sample-b", "ok"}: 1, {h.sessionID, "sample-b", "ok"}: 1, {"", "", "auth_failed"}: 1}
	if !maps.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for _, row := range rows {
		if row.kind != "connect" {
			t.Fatalf("row kind %q", row.kind)
		}
	}
	var apiRows int
	if err := h.openFile().QueryRow("SELECT COUNT(*) FROM traffic WHERE host = 'api.anthropic.com' AND port = 443 AND session_id = ? AND alias = 'sample-a' AND result = 'ok'", h.sessionID).Scan(&apiRows); err != nil || apiRows != 1 {
		t.Fatalf("api.anthropic.com rows on session 1 / A = %d (%v), want 1", apiRows, err)
	}
}

// TestProxyCredential checks how the session credential is read from Proxy-Authorization.
func TestProxyCredential(t *testing.T) {
	encode := func(userinfo string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(userinfo)) }
	for _, test := range []struct {
		value, token string
		ok           bool
	}{
		{encode("session:tok"), "tok", true},
		{encode("tok"), "tok", true},
		{encode(":tok"), "tok", true},
		{encode("session:"), "", false},
		{encode(""), "", false},
		{"Basic not-base64!", "", false},
		{"Bearer tok", "", false},
		{"", "", false},
	} {
		token, ok := proxyCredential(test.value)
		if ok != test.ok || token != test.token {
			t.Fatalf("%q -> %q %v", test.value, token, ok)
		}
	}
}

// TestShutdownWaitsForOpenTunnel checks the shutdown path with a CONNECT tunnel still
// open: the server stops, and the drain returns only after the tunnel's record is
// saved. The record sink is held so that the wait is observable.
func TestShutdownWaitsForOpenTunnel(t *testing.T) {
	exit := startFakeExit(t, 200)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := &RecordStore{dir: dir}
	release := make(chan struct{})
	h := newHarness(t)
	h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
	h.token, h.sessionID = h.issue()
	forward := newForwardProxy(h.store, h.accounts, func(record connectRecord) error {
		<-release
		return store.writeConnect(record)
	})
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, free, forward) }()
	var conn net.Conn
	for deadline := time.Now().Add(3 * time.Second); conn == nil; {
		var err error
		if conn, err = net.Dial("tcp", address); err != nil {
			if time.Now().After(deadline) {
				t.Fatal("forward proxy did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	defer closeQuietly(conn)
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT api.example.test:443 HTTP/1.1\r\nHost: api.example.test:443\r\nProxy-Authorization: "+proxyAuthorization(h.token)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect}); err != nil || response.StatusCode != 200 {
		t.Fatal("tunnel not opened")
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 4)); err != nil {
		t.Fatal("tunnel not carrying bytes")
	}
	// Shut down with the tunnel still open.
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop with a tunnel open")
	}
	// The server no longer tracks the hijacked tunnel; Finish waits for it, then for the writes.
	finished := make(chan error, 1)
	go func() { finished <- (&Entrypoint{forward: forward, records: store}).Finish(ctx) }()
	select {
	case <-finished:
		t.Fatal("finish returned before the open tunnel was recorded")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("finish did not return after the tunnel ended")
	}
	files, err := filepath.Glob(filepath.Join(dir, "connect-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("open tunnel's record not saved before finish returned")
	}
	artifact, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var record connectRecord
	if err := json.Unmarshal(artifact, &record); err != nil || record.Result != "ok" || record.BytesUp != 4 || record.BytesDown != 4 {
		t.Fatalf("open tunnel recorded as %+v", record)
	}
}

// TestForwardRefusedRowsAreLimitedPerSource sends more CONNECTs with an unknown session
// credential from one address than a window records: only refusalRowsPerSource of them
// get a row, and a tunnel with a valid session still does. Control arm: once the
// loopback address is over its limit, the first refusal from another address is still
// recorded, and the loopback address's next one is not.
func TestForwardRefusedRowsAreLimitedPerSource(t *testing.T) {
	exit := startFakeExit(t, 200)
	h := newHarness(t)
	h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
	h.token, h.sessionID = h.issue()
	forward := newForwardProxy(h.store, h.accounts, nil)
	server := httptest.NewServer(forward)
	defer server.Close()
	address := server.Listener.Addr().String()
	unknown := proxyAuthorization("sk-ant-gws-" + strings.Repeat("B", 43))
	refuse := func() {
		t.Helper()
		if _, _, response := connect(t, address, "api.example.test:443", unknown); response.StatusCode != 407 {
			t.Fatalf("status=%d", response.StatusCode)
		}
		if err := forward.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for range refusalRowsPerSource + 2 {
		refuse()
	}
	conn, _, response := connect(t, address, "api.example.test:443", proxyAuthorization(h.token))
	if response.StatusCode != 200 {
		t.Fatalf("valid session: status=%d", response.StatusCode)
	}
	closeQuietly(conn)
	if err := forward.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := h.traffic()
	if len(rows) != refusalRowsPerSource+1 || rows[0].result != "auth_failed" || rows[refusalRowsPerSource].result != "ok" {
		t.Fatalf("rows=%+v", rows)
	}
	// Another address, served in place so that the row is written before ServeHTTP returns.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodConnect, "api.example.test:443", nil)
	req.RemoteAddr = "198.51.100.9:40000"
	req.Header.Set("Proxy-Authorization", unknown)
	recorder := httptest.NewRecorder()
	forward.ServeHTTP(recorder, req)
	if recorder.Code != 407 {
		t.Fatalf("other address: status=%d", recorder.Code)
	}
	refuse()
	rows = h.traffic()
	if len(rows) != refusalRowsPerSource+2 || rows[len(rows)-1].result != "auth_failed" {
		t.Fatalf("other address: rows=%+v", rows)
	}
}
