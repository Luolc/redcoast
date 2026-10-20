package claude

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockedLog collects what the standard logger writes during one test.
type lockedLog struct {
	mu  sync.Mutex
	out strings.Builder
}

// Write appends p.
func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out.Write(p)
}

// lines returns the logged lines that contain marker.
func (l *lockedLog) lines(marker string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for line := range strings.Lines(l.out.String()) {
		if strings.Contains(line, marker) {
			out = append(out, line)
		}
	}
	return out
}

// captureLog sends the standard logger to a lockedLog until the test ends.
func captureLog(t *testing.T) *lockedLog {
	logged := &lockedLog{}
	previous := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logged
}

// closedAddress returns a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// silentExit starts an exit that reads each request head and closes the connection
// without answering, and returns its address.
func silentExit(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			closeQuietly(conn)
		}
	}()
	return listener.Addr().String()
}

// blockingUpstream starts an upstream that never answers: each request waits until its
// connection goes or the test ends. received gets a value for each request that arrived.
func blockingUpstream(t *testing.T) (upstream *httptest.Server, received chan struct{}) {
	t.Helper()
	received, release := make(chan struct{}, 4), make(chan struct{})
	upstream = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })
	return upstream, received
}

// hijackingUpstream starts an upstream that takes each connection from net/http and
// closes it without a response; with reset it first sets the linger to zero, so the
// close sends a TCP reset instead of a FIN.
func hijackingUpstream(t *testing.T, reset bool) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A close with unread bytes would send a reset whatever the linger.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		if reset {
			_ = conn.(*net.TCPConn).SetLinger(0)
		}
		closeQuietly(conn)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// failureMarkers are a request's parts that must not reach the log: its path, query and
// a header value.
const (
	failurePath        = "/v1/messages"
	failureQueryMarker = "synthetic-query-marker"
	failureHeaderValue = "synthetic-header-marker"
)

// sendFailing sends one inference request through server with ctx, with the markers in
// its query and a header, and returns the status, or 0 when no response came.
func sendFailing(ctx context.Context, t *testing.T, h *harness, server *httptest.Server) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+failurePath+"?probe="+failureQueryMarker, strings.NewReader("synthetic prompt"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Synthetic-Marker", failureHeaderValue)
	res, err := h.client().Do(req)
	if err != nil {
		return 0
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// sendRaw writes head, with the session credential in place of {token}, on a new
// connection to server, closes the connection's write side and returns the status of
// the response, or 0 when none came.
func sendRaw(t *testing.T, h *harness, server *httptest.Server, head string) int {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, strings.ReplaceAll(head, "{token}", h.token)); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// expectFailureLog checks that logged holds exactly one upstream-failure line, naming
// a's alias and exit and failure, and that none of the request's parts, the upstream's
// address or any credential is in the log.
func expectFailureLog(t *testing.T, logged *lockedLog, h *harness, a *account, target *url.URL, failure string) {
	t.Helper()
	lines := logged.lines("upstream unavailable")
	if len(lines) != 1 {
		t.Fatalf("upstream-failure log lines = %q, want one", lines)
	}
	for _, want := range []string{"alias=" + a.alias + " ", "exit=" + a.exitName() + " ", "failure=" + failure + " ", "elapsed_ms="} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("log line %q lacks %q", lines[0], want)
		}
	}
	all := strings.Join(logged.lines(""), "")
	for _, secret := range []string{failurePath, failureQueryMarker, failureHeaderValue, target.Host, h.token, tokenOf(a), a.proxyUsername, a.proxyPassword} {
		if strings.Contains(all, secret) {
			t.Fatalf("log %q contains %q", all, secret)
		}
	}
}

// failureSetup is what an arm of TestUpstreamFailureIsNamed set up: the upstream's URL,
// a change to the router before it serves, the client's context and a raw request;
// zero values change nothing, use the test's context and send with sendFailing.
type failureSetup struct {
	upstream string
	adjust   func(*router)
	ctx      context.Context
	head     string // when set, sent as the raw request in place of sendFailing's
}

// TestUpstreamFailureIsNamed makes each kind of upstream failure happen against a fake
// upstream or a fake exit and checks the name the traffic row's failure column and the
// one log line give it, and that the log holds no URL, header or credential. Control
// arm: a request the upstream answers has no failure and logs nothing.
func TestUpstreamFailureIsNamed(t *testing.T) {
	const token = "sk-ant-oat01-synthetic-failure-token"
	for _, arm := range []struct {
		name    string
		failure string // empty: the control arm, which logs nothing
		result  string
		status  int
		setup   func(t *testing.T, h *harness) failureSetup // adds the account sample-a
	}{
		{name: "ok", result: "ok", status: http.StatusOK, setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(upstream.Close)
			return failureSetup{upstream: upstream.URL}
		}},
		{name: "exit_refuses_connect", failure: "egress_connect_407", setup: func(t *testing.T, h *harness) failureSetup {
			exit := startFakeExit(t, http.StatusProxyAuthRequired)
			h.addWithExit("sample-a", token, &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			return failureSetup{upstream: "https://api.example.test"}
		}},
		{name: "exit_unreachable", failure: "egress_dial", setup: func(t *testing.T, h *harness) failureSetup {
			h.addWithExit("sample-a", token, &url.URL{Scheme: "http", Host: closedAddress(t)})
			return failureSetup{upstream: "https://api.example.test"}
		}},
		{name: "exit_unanswered", failure: "egress_unanswered", setup: func(t *testing.T, h *harness) failureSetup {
			h.addWithExit("sample-a", token, &url.URL{Scheme: "http", Host: silentExit(t)})
			return failureSetup{upstream: "https://api.example.test"}
		}},
		{name: "upstream_unreachable", failure: "dial", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			return failureSetup{upstream: "http://" + closedAddress(t)}
		}},
		{name: "tls_handshake", failure: "tls", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(plain.Close)
			return failureSetup{upstream: "https://" + plain.Listener.Addr().String()}
		}},
		{name: "response_header_timeout", failure: "response_header_timeout", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token).transport.ResponseHeaderTimeout = 50 * time.Millisecond
			upstream, _ := blockingUpstream(t)
			return failureSetup{upstream: upstream.URL}
		}},
		{name: "peer_closes", failure: "reset", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			return failureSetup{upstream: hijackingUpstream(t, false).URL}
		}},
		{name: "peer_resets", failure: "reset", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			return failureSetup{upstream: hijackingUpstream(t, true).URL}
		}},
		{name: "http2_stream_reset", failure: "reset", setup: func(t *testing.T, h *harness) failureSetup {
			var proto atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				proto.Store(int32(r.ProtoMajor))
				panic(http.ErrAbortHandler)
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			t.Cleanup(func() {
				if proto.Load() != 2 {
					t.Errorf("upstream got HTTP/%d, want HTTP/2", proto.Load())
				}
			})
			roots := x509.NewCertPool()
			roots.AddCert(upstream.Certificate())
			h.add("sample-a", token).transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			return failureSetup{upstream: upstream.URL}
		}},
		{name: "upgrade", failure: "upgrade", status: http.StatusSwitchingProtocols, setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Connection", "Upgrade")
				w.Header().Set("Upgrade", "websocket")
				w.WriteHeader(http.StatusSwitchingProtocols)
			}))
			t.Cleanup(upstream.Close)
			return failureSetup{upstream: upstream.URL}
		}},
		{name: "guard", failure: "guard", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("a refused request reached the upstream")
			}))
			t.Cleanup(upstream.Close)
			return failureSetup{upstream: upstream.URL, adjust: func(rt *router) {
				provider := rt.provider
				provider.Guard = func(*http.Request) error { return errors.New("synthetic refusal") }
				target, _ := url.Parse(upstream.URL)
				*rt = *newRouter(rt.store, rt.accounts, target, provider, accountTransport{})
				rt.now = h.router(target, accountTransport{}).now
			}}
		}},
		{name: "idle_limit", failure: "limit", result: "idle_timeout", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream, _ := blockingUpstream(t)
			return failureSetup{upstream: upstream.URL, adjust: func(rt *router) { rt.limits = Limits{Idle: 100 * time.Millisecond, MaxRequest: time.Minute} }}
		}},
		{name: "malformed_chunked_body", failure: "client_body", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream, _ := blockingUpstream(t)
			return failureSetup{upstream: upstream.URL, head: "POST " + failurePath + " HTTP/1.1\r\nHost: gateway.example.test\r\nAuthorization: Bearer {token}\r\n" +
				"X-Synthetic-Marker: " + failureHeaderValue + "\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n"}
		}},
		// The client's half-close ends its connection's context before the body read
		// fails, so the client counts as gone.
		{name: "truncated_body", failure: "client_canceled", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream, _ := blockingUpstream(t)
			return failureSetup{upstream: upstream.URL, head: "POST " + failurePath + " HTTP/1.1\r\nHost: gateway.example.test\r\nAuthorization: Bearer {token}\r\n" +
				"X-Synthetic-Marker: " + failureHeaderValue + "\r\nContent-Length: 1000\r\n\r\n{\"synthetic\": \"prompt"}
		}},
		{name: "client_cancels", failure: "client_canceled", setup: func(t *testing.T, h *harness) failureSetup {
			h.add("sample-a", token)
			upstream, received := blockingUpstream(t)
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-received
				cancel()
			}()
			return failureSetup{upstream: upstream.URL, ctx: ctx}
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			h := newHarness(t)
			setup := arm.setup(t, h)
			h.token, h.sessionID = h.issue()
			a := h.accounts.lookup("sample-a")
			target, err := url.Parse(setup.upstream)
			if err != nil {
				t.Fatal(err)
			}
			rt := h.router(target, accountTransport{})
			if setup.adjust != nil {
				setup.adjust(rt)
			}
			ownContext := setup.ctx != nil
			if !ownContext {
				setup.ctx = t.Context()
			}
			server := httptest.NewServer(rt)
			t.Cleanup(server.Close)
			logged := captureLog(t)
			var status int
			if setup.head != "" {
				status = sendRaw(t, h, server, setup.head)
			} else {
				status = sendFailing(setup.ctx, t, h, server)
			}
			result := arm.result
			if result == "" {
				result = "upstream_failed"
			}
			wantStatus := http.StatusBadGateway
			switch {
			case arm.failure == "":
				wantStatus = http.StatusOK
			case ownContext:
				// The client canceled before any response.
				wantStatus = 0
			}
			if status != wantStatus {
				t.Fatalf("client got %d, want %d", status, wantStatus)
			}
			rows := h.trafficRows(1)
			expectTraffic(t, rows, 0, trafficRow{sessionID: h.sessionID, alias: "sample-a", kind: "inference", result: result, status: arm.status})
			if rows[0].failure != arm.failure {
				t.Fatalf("failure = %q, want %q", rows[0].failure, arm.failure)
			}
			if arm.failure == "" {
				if lines := logged.lines("upstream unavailable"); len(lines) != 0 {
					t.Fatalf("answered request logged %q", lines)
				}
				return
			}
			expectFailureLog(t, logged, h, a, target, arm.failure)
		})
	}
}

// TestShutdownFailureIsNamed stops the gateway while a request waits for the upstream:
// the request is named shutdown, not a client's cancellation.
func TestShutdownFailureIsNamed(t *testing.T) {
	h := newHarness(t, "sample-a")
	upstream, received := blockingUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, listener, h.router(target, accountTransport{})) }()
	logged := captureLog(t)
	go func() {
		<-received
		stop()
	}()
	_ = sendFailing(t.Context(), t, h, &httptest.Server{URL: "http://" + listener.Addr().String()})
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	rows := h.trafficRows(1)
	expectTraffic(t, rows, 0, trafficRow{sessionID: h.sessionID, alias: "sample-a", kind: "inference", result: "upstream_failed"})
	if rows[0].failure != "shutdown" {
		t.Fatalf("failure = %q, want shutdown", rows[0].failure)
	}
	expectFailureLog(t, logged, h, h.accounts.lookup("sample-a"), target, "shutdown")
}
