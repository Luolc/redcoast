package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startUnix serves the session interface on a socket in a fresh directory and returns
// the socket path, the cancel function and the channel that the server's result lands
// on. It listens before it returns, so the socket already has its final mode: the file
// appears at bind, before ListenUnixSocket restricts it.
func startUnix(t *testing.T, server *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock") // short: unix socket paths are limited to 108 bytes
	listener, err := ListenUnixSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- ServeUnixListener(ctx, listener, server.Handler()) }()
	return socket, cancel, done
}

// tcpListeners returns the LISTEN rows of /proc/net/tcp and tcp6 whose socket this
// process holds. /proc/self/net/tcp lists the whole network namespace, so another
// process's listener on a shared machine would otherwise count.
func tcpListeners(t *testing.T) map[string]bool {
	t.Helper()
	owned := make(map[string]bool)
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if target, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.HasPrefix(target, "socket:[") {
			owned[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	rows := make(map[string]bool)
	for _, name := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) > 9 && fields[3] == "0A" && owned[fields[9]] {
				rows[fields[1]] = true
			}
		}
	}
	return rows
}

// TestServeUnixOnly checks the session interface over its unix socket: issue returns a
// credential and both entrypoints, revoke with the credential ends the session and a
// second revoke is refused; the socket is owner-only and the process gained no TCP
// listener, while the unix socket path does appear in the process's socket table.
func TestServeUnixOnly(t *testing.T) {
	store, _ := open(t, newClock())
	entrypoints := Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}
	before := tcpListeners(t)
	socket, cancel, done := startUnix(t, NewServer(store, entrypoints, func() []string { return accounts }))
	ctx := context.Background()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()

	resp, err := client.Post("http://session/sessions", "application/json", strings.NewReader(`{"client_machine":"machine-a","launch_meta":{"pid":42}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: status %d body %s (%v)", resp.StatusCode, body, err)
	}
	var issued Grant
	if err := json.Unmarshal(body, &issued); err != nil || !strings.HasPrefix(issued.Token, TokenPrefix) || issued.SessionID == "" || issued.Entrypoints != entrypoints {
		t.Fatalf("issue response %s (%v)", body, err)
	}
	if count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ? AND client_machine = 'machine-a' AND launch_meta = '{\"pid\":42}'", issued.SessionID) != 1 {
		t.Fatal("session row missing or launch metadata not saved")
	}
	if _, err := store.Bind(ctx, issued.Token, accounts); err != nil {
		t.Fatalf("issued credential refused by Bind: %v", err)
	}

	for i, test := range []struct {
		authorization string
		status        int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer " + issued.Token, http.StatusNoContent},
		{"Bearer " + issued.Token, http.StatusUnauthorized},
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://session/sessions/current", nil)
		if err != nil {
			t.Fatal(err)
		}
		if test.authorization != "" {
			req.Header.Set("Authorization", test.authorization)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatalf("revoke %d: status %d, want %d", i, resp.StatusCode, test.status)
		}
	}
	if _, err := store.Bind(ctx, issued.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("got %v after revoke over the socket, want ErrUnknownSession", err)
	}
	for _, body := range []string{`{}`, `{"client_machine":"machine-a","extra":1}`, `{"client_machine":"machine-a","launch_meta":[1]}`, `not json`} {
		resp, err := client.Post("http://session/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: status %d, want 400", body, resp.StatusCode)
		}
	}

	after := tcpListeners(t)
	for address := range after {
		if !before[address] {
			t.Fatalf("a TCP listener appeared at %s while the session interface was up", address)
		}
	}
	unixTable, err := os.ReadFile("/proc/self/net/unix")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unixTable, []byte(socket)) {
		t.Fatal("control arm: the unix socket path is not in the process's socket table, so the TCP check could not have seen a listener either")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file still present after shutdown: %v", err)
	}
}

// TestShutdownClosesRequestsInProgress is the regression for a request that outlived
// the server: a POST whose body is still incomplete when the server stops must not
// issue a session after ServeUnix returns, and its connection is closed. The control
// arm completes a request before the stop; ServeUnix then returns at once with nil.
func TestShutdownClosesRequestsInProgress(t *testing.T) {
	for _, partial := range []bool{false, true} {
		store, _ := open(t, newClock())
		server := NewServer(store, Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return accounts })
		started := make(chan struct{}, 2)
		server.issuing = func() { started <- struct{}{} }
		socket, cancel, done := startUnix(t, server)
		conn, err := net.DialTimeout("unix", socket, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		body := `{"client_machine":"machine-a"}`
		head := "POST /sessions HTTP/1.1\r\nHost: session\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
		sent := body
		if partial {
			sent = body[:1]
		}
		if _, err := io.WriteString(conn, head+sent); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started: // the handler is reading the body
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not start")
		}
		if !partial {
			// Read the complete response so the connection is idle when the server stops.
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil || resp.StatusCode != http.StatusCreated {
				t.Fatalf("control arm: status %v (%v), want 201", resp, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		stopped := time.Now()
		cancel()
		err = <-done
		elapsed := time.Since(stopped)
		sessions := count(t, store, "SELECT COUNT(*) FROM sessions")
		if !partial {
			if err != nil || elapsed > 2*time.Second || sessions != 1 {
				t.Fatalf("control arm: ServeUnix returned %v after %v with %d sessions, want nil, at once, 1", err, elapsed, sessions)
			}
			continue
		}
		if err == nil || elapsed < shutdownGrace {
			t.Fatalf("partial arm: ServeUnix returned %v after %v, want an error after the grace period", err, elapsed)
		}
		// Finishing the body now must not reach a handler: the connection is closed.
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(conn, body[1:]); err == nil {
			if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
				t.Fatal("partial arm: the request was answered after ServeUnix returned")
			}
		}
		if got := count(t, store, "SELECT COUNT(*) FROM sessions"); got != sessions || got != 0 {
			t.Fatalf("partial arm: %d sessions after shutdown, want 0", got)
		}
	}
}

// TestServeTCP checks the session interface over TCP: a request without a machine
// credential is refused with unknown_machine before anything else; a registered
// machine's credential gets a session that revokes over the same address; the wrong
// credential is refused with unknown_machine and a machine at its limit with
// machine_limit (no session issued either time, by the sessions count).
func TestServeTCP(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	schedule(t, store, "sample-a", "pro")
	if err := store.AddMachine(ctx, "client-a", hashOf("machine-a-synthetic-credential"), 1); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return accounts })
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.ServeTCP(serveCtx, address) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if conn, err := net.Dial("tcp", address); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not come up")
		}
	}
	// With no account able to serve, the wrong credential is still unknown_machine (the
	// machine is authenticated before the accounts are looked at) and the right one
	// gets the accounts' refusal.
	for _, alias := range accounts {
		if err := store.Pause(ctx, alias, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "smoke"); err != nil {
			t.Fatal(err)
		}
	}
	var refused *Refusal
	if _, err := (&Client{Address: address, Credential: "wrong"}).Issue(ctx, "", nil); !errors.As(err, &refused) || refused.Code != "unknown_machine" {
		t.Fatalf("wrong credential with no account available: %v", err)
	}
	client := &Client{Address: address, Credential: "machine-a-synthetic-credential"}
	if _, err := client.Issue(ctx, "", nil); !errors.As(err, &refused) || refused.Code != "no_account" {
		t.Fatalf("right credential with no account available: %v", err)
	}
	for _, alias := range accounts {
		if err := store.Resume(ctx, alias); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := client.Issue(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Issue(ctx, "", nil); !errors.As(err, &refused) || refused.Code != "machine_limit" {
		t.Fatalf("second session over the limit of 1: %v", err)
	}
	if _, err := (&Client{Address: address, Credential: "wrong"}).Issue(ctx, "", nil); !errors.As(err, &refused) || refused.Code != "unknown_machine" {
		t.Fatalf("wrong credential: %v", err)
	}
	// No credential at all: refused by the listener, before the handler.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/sessions", strings.NewReader(`{"client_machine":"client-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body Refusal
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || res.StatusCode != http.StatusUnauthorized || body.Code != "unknown_machine" {
		t.Fatalf("no credential: status=%d body=%+v err=%v", res.StatusCode, body, err)
	}
	_ = res.Body.Close()
	var sessions int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions issued: %d %v", sessions, err)
	}
	// The session was issued from loopback, so it is local, and it revokes over TCP.
	var source string
	if err := store.db.QueryRow("SELECT source_addr FROM sessions WHERE id = ?", grant.SessionID).Scan(&source); err != nil || source != LocalSource {
		t.Fatalf("source: %q %v", source, err)
	}
	if err := client.Revoke(ctx, grant.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, grant.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("after revocation: %v", err)
	}
}

// TestRevokeSource checks DELETE /sessions/current against the issuing address: from
// another address it is 401 and the session goes on; from the issuing address it is
// 204 and the session has ended (control arm). The handler is driven directly so that
// the peer address can be set.
func TestRevokeSource(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	schedule(t, store, "sample-a", "pro")
	issued, err := store.Issue(ctx, "client-a", "198.51.100.7", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return accounts }).Handler()
	revoke := func(remote string) int {
		req := httptest.NewRequestWithContext(ctx, http.MethodDelete, "/sessions/current", nil)
		req.Header.Set("Authorization", "Bearer "+issued.Token)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, remote := range []string{"198.51.100.8:4000", "127.0.0.1:4000"} {
		if status := revoke(remote); status != http.StatusUnauthorized {
			t.Fatalf("revoke from %s: %d", remote, status)
		}
		if _, err := store.BindFrom(ctx, issued.Token, "198.51.100.7", accounts); err != nil {
			t.Fatalf("session after a wrong-source revoke from %s: %v", remote, err)
		}
	}
	if status := revoke("198.51.100.7:4000"); status != http.StatusNoContent {
		t.Fatalf("revoke from the issuing address: %d", status)
	}
	if _, err := store.BindFrom(ctx, issued.Token, "198.51.100.7", accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("session after revocation: %v", err)
	}
}

// TestSourceOf checks the classification of peer addresses.
func TestSourceOf(t *testing.T) {
	for remote, want := range map[string]string{"": LocalSource, "@": LocalSource, "127.0.0.1:4000": LocalSource, "[::1]:4000": LocalSource, "198.51.100.7:4000": "198.51.100.7", "[2001:db8::7]:4000": "2001:db8::7"} {
		if got := SourceOf(remote); got != want {
			t.Errorf("SourceOf(%q) = %q, want %q", remote, got, want)
		}
	}
}
