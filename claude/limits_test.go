package claude

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// waitTraffic waits until the traffic table has n rows and returns them; rows are
// written when a request's handler returns, after the client may already have its answer.
func waitTraffic(t *testing.T, h *harness, n int) []trafficRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := h.traffic()
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// streamingUpstream answers every request with one byte after each delay in schedule,
// flushing each one, so a test controls when bytes move.
func streamingUpstream(t *testing.T, schedule []time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, delay := range schedule {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// every returns n delays of d.
func every(n int, d time.Duration) []time.Duration {
	schedule := make([]time.Duration, n)
	for i := range schedule {
		schedule[i] = d
	}
	return schedule
}

// TestInferenceLimits checks the reverse proxy's limits with a 150 ms idle limit: a
// stream that keeps sending runs well past it and completes; the control arm with one
// gap longer than the idle limit is cut as idle_timeout; a stream that keeps sending
// past the total limit is cut as max_duration.
func TestInferenceLimits(t *testing.T) {
	for _, test := range []struct {
		name     string
		schedule []time.Duration
		max      time.Duration
		bytes    int
		result   string
	}{
		{"busy_past_idle_limit", every(12, 50*time.Millisecond), 2 * time.Second, 12, "ok"},
		{"gap_past_idle_limit", []time.Duration{50 * time.Millisecond, 400 * time.Millisecond, 50 * time.Millisecond}, 2 * time.Second, 1, "idle_timeout"},
		{"busy_past_total_limit", every(12, 50*time.Millisecond), 300 * time.Millisecond, 5, "max_duration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, "sample-a")
			target, _ := url.Parse(streamingUpstream(t, test.schedule).URL)
			rt := h.router(target, accountTransport{})
			rt.limits = Limits{Idle: 150 * time.Millisecond, MaxRequest: test.max, MaxTunnel: time.Hour}
			server := httptest.NewServer(rt)
			defer server.Close()
			response, err := h.client().Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			rows := waitTraffic(t, h, 1)
			// The total-limit arm's byte count depends on scheduling; it must be cut short.
			countOK := len(body) == test.bytes || (test.result == "max_duration" && len(body) > 0 && len(body) < 12)
			if len(rows) != 1 || rows[0].result != test.result || !countOK {
				t.Fatalf("body %d bytes, rows %+v; want %d bytes and result %s", len(body), rows, test.bytes, test.result)
			}
		})
	}
}

// TestInferenceIdleClientStopsReading checks that a client that stops reading a large
// response is cut by the idle limit, well before the total limit, although the upstream
// still has bytes to send: the cut moves the blocked write's deadline.
func TestInferenceIdleClientStopsReading(t *testing.T) {
	h := newHarness(t, "sample-a")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 64<<10)
		for range 1024 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	rt := h.router(target, accountTransport{})
	rt.limits = Limits{Idle: 150 * time.Millisecond, MaxRequest: 30 * time.Second, MaxTunnel: time.Hour}
	server := httptest.NewServer(rt)
	defer server.Close()
	response, err := h.client().Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	started := time.Now()
	rows := waitTraffic(t, h, 1)
	if len(rows) != 1 || rows[0].result != "idle_timeout" || time.Since(started) > 3*time.Second {
		t.Fatalf("rows %+v after %v; want idle_timeout well before the total limit", rows, time.Since(started))
	}
}

// TestInferenceIdleClientStopsSending checks that a client that stops in the middle
// of its request body is cut by the idle limit, well before the total limit, while the
// upstream waits for the rest of the body: the cut moves the blocked read's deadline.
func TestInferenceIdleClientStopsSending(t *testing.T) {
	h := newHarness(t, "sample-a")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	rt := h.router(target, accountTransport{})
	rt.limits = Limits{Idle: 150 * time.Millisecond, MaxRequest: 30 * time.Second, MaxTunnel: time.Hour}
	server := httptest.NewServer(rt)
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)
	// Sixteen bytes announced, one sent.
	if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: gateway.test\r\nAuthorization: Bearer "+h.token+"\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rows := waitTraffic(t, h, 1)
	if len(rows) != 1 || rows[0].result != "idle_timeout" || time.Since(started) > 3*time.Second {
		t.Fatalf("rows %+v after %v; want idle_timeout well before the total limit", rows, time.Since(started))
	}
}

// TestTunnelLimits checks the forward proxy's limits with a 150 ms idle limit through
// an echoing exit: a tunnel that keeps exchanging bytes runs well past it and ends ok
// when the client closes; the control arm that goes quiet is closed by the gateway as
// idle_timeout; a busy tunnel past the total limit is closed as max_duration.
func TestTunnelLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		pings  int
		quiet  time.Duration
		max    time.Duration
		result string
	}{
		{"busy_past_idle_limit", 12, 0, 2 * time.Second, "ok"},
		{"quiet_past_idle_limit", 1, 400 * time.Millisecond, 2 * time.Second, "idle_timeout"},
		{"busy_past_total_limit", 12, 0, 300 * time.Millisecond, "max_duration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exit := startFakeExit(t, 200)
			h := newHarness(t)
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			h.token, h.sessionID = h.issue()
			forward := newForwardProxy(h.store, h.accounts, nil)
			forward.limits = Limits{Idle: 150 * time.Millisecond, MaxRequest: time.Hour, MaxTunnel: test.max}
			server := httptest.NewServer(forward)
			defer server.Close()
			conn, reader, response := connect(t, server.Listener.Addr().String(), "example.invalid:443", proxyAuthorization(h.token))
			if response.StatusCode != 200 {
				t.Fatalf("status=%d", response.StatusCode)
			}
			echoed := 0
			for range test.pings {
				time.Sleep(50 * time.Millisecond)
				if _, err := io.WriteString(conn, "ping"); err != nil {
					break
				}
				if _, err := io.ReadFull(reader, make([]byte, 4)); err != nil {
					break
				}
				echoed++
			}
			time.Sleep(test.quiet)
			closeQuietly(conn)
			rows := waitTraffic(t, h, 1)
			if len(rows) != 1 || rows[0].result != test.result || rows[0].sessionID != h.sessionID {
				t.Fatalf("echoed %d, rows %+v; want result %s", echoed, rows, test.result)
			}
			if test.result == "ok" && echoed != test.pings {
				t.Fatalf("echoed %d of %d pings", echoed, test.pings)
			}
		})
	}
}

// TestNoAccountRowsCarryTheSession checks that a refusal for want of an account is
// recorded with the session it refused, on both entrypoints; the control arm with an
// unknown credential is recorded without one.
func TestNoAccountRowsCarryTheSession(t *testing.T) {
	for _, entry := range []string{"inference", "connect"} {
		t.Run(entry, func(t *testing.T) {
			exit := startFakeExit(t, 200)
			h := newHarness(t)
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			h.token, h.sessionID = h.issue()
			if err := h.store.Pause(context.Background(), "sample-a", indefinitePause, "upstream_401"); err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{h.token, "sk-ant-gws-unknown"} {
				switch entry {
				case "inference":
					server := h.gateway("http://127.0.0.1:1")
					response, err := h.clientFor(token).Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
				case "connect":
					server := httptest.NewServer(newForwardProxy(h.store, h.accounts, nil))
					defer server.Close()
					connect(t, server.Listener.Addr().String(), "example.invalid:443", proxyAuthorization(token))
				}
			}
			rows := waitTraffic(t, h, 2)
			if len(rows) != 2 || rows[0].result != "no_account" || rows[0].sessionID != h.sessionID || rows[1].result != "auth_failed" || rows[1].sessionID != "" {
				t.Fatalf("rows %+v; want no_account with session %s, then auth_failed without one", rows, h.sessionID)
			}
		})
	}
}
