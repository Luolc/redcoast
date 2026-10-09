package claude

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// sharedListener opens a listener and a second one on the same socket, as the new
// process of a handoff holds it.
func sharedListener(t *testing.T) (old, inherited net.Listener) {
	t.Helper()
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	file, err := old.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if inherited, err = net.FileListener(file); err != nil {
		t.Fatal(err)
	}
	return old, inherited
}

// stopFor stops the old server's context the way each arm does: a handoff whose drain
// lasts until the test ends it, or a plain stop. It returns the drain's end.
func stopFor(t *testing.T, arm string, stop context.CancelCauseFunc) context.CancelFunc {
	requests, endDrain := context.WithCancel(t.Context())
	if arm == "stop" {
		stop(nil)
	} else {
		stop(Handoff{Requests: requests})
	}
	if arm == "drain_ends" {
		endDrain()
	}
	return endDrain
}

// TestHandoffDrainsInferenceStreams checks that an inference stream open at a handoff
// completes through the old server while a new request to the same address is still
// answered (by the new server once the old one stops accepting). Control arms: a plain
// stop and the end of the drain both cut the stream.
func TestHandoffDrainsInferenceStreams(t *testing.T) {
	for _, arm := range []string{"handoff", "drain_ends", "stop"} {
		t.Run(arm, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					return
				}
				_, _ = io.WriteString(w, "first ")
				_ = http.NewResponseController(w).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "second")
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(upstream.Close)
			h := newHarness(t, "sample-test")
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			old, inherited := sharedListener(t)
			gateway := "http://" + old.Addr().String()
			ctx, stop := context.WithCancelCause(t.Context())
			served := make(chan error, 1)
			go func() { served <- Serve(ctx, old, h.router(target, accountTransport{})) }()
			response, err := h.client().Post(gateway+"/v1/messages", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body := bufio.NewReader(response.Body)
			if first, err := body.ReadString(' '); err != nil || first != "first " {
				t.Fatalf("stream start %q, %v", first, err)
			}
			newCtx, stopNew := context.WithCancel(t.Context())
			defer stopNew()
			go func() { _ = Serve(newCtx, inherited, h.router(target, accountTransport{})) }()
			defer stopFor(t, arm, stop)()
			if arm == "handoff" {
				fresh, err := h.client().Post(gateway+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{}`))
				if err != nil || fresh.StatusCode != http.StatusOK {
					t.Fatalf("new request during the drain: %v", err)
				}
				_ = fresh.Body.Close()
				select {
				case err := <-served:
					t.Fatalf("old server stopped with a stream open: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
			}
			// In the other arms the upstream request is canceled and never released.
			if arm == "handoff" {
				close(release)
			}
			rest, err := io.ReadAll(body)
			if completed := err == nil && string(rest) == "second"; completed != (arm == "handoff") {
				t.Fatalf("rest of the stream %q, %v", rest, err)
			}
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("old server did not stop")
			}
		})
	}
}

// TestHandoffDrainsTunnels checks that a CONNECT tunnel open at a handoff keeps carrying
// bytes and that Run returns only after it ends. Control arms: a plain stop and the end
// of the drain both cut the tunnel.
func TestHandoffDrainsTunnels(t *testing.T) {
	for _, arm := range []string{"handoff", "drain_ends", "stop"} {
		t.Run(arm, func(t *testing.T) {
			exit := startFakeExit(t, 200)
			h := newHarness(t)
			h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			h.token, h.sessionID = h.issue()
			forward := newForwardProxy(h.store, h.accounts, nil)
			entrypoint := &Entrypoint{Forward: forward, forward: forward}
			old, inherited := sharedListener(t)
			_ = inherited.Close()
			ctx, stop := context.WithCancelCause(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- entrypoint.Run(ctx, func(ctx context.Context) error { return Serve(ctx, old, forward) })
			}()
			conn, reader, response := connect(t, old.Addr().String(), "api.example.test:443", proxyAuthorization(h.token))
			if response.StatusCode != http.StatusOK {
				t.Fatalf("tunnel status %d", response.StatusCode)
			}
			defer stopFor(t, arm, stop)()
			if arm == "handoff" {
				select {
				case err := <-done:
					t.Fatalf("Run returned with a tunnel open: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
			}
			if arm == "handoff" {
				echo := make([]byte, 4)
				if _, err := io.WriteString(conn, "ping"); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
					t.Fatalf("tunnel after the handoff: %q, %v", echo, err)
				}
			} else if _, err := io.Copy(io.Discard, reader); err != nil {
				// The cut is asynchronous: wait, within connect's deadline, for the gateway to
				// close the tunnel; a timeout means it stayed open.
				t.Fatalf("tunnel not cut: %v", err)
			}
			closeQuietly(conn)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after the tunnel ended")
			}
		})
	}
}

// watchedListener reports its first Accept and its Close.
type watchedListener struct {
	net.Listener
	accepted, closed chan struct{}
	closeOnce        sync.Once
}

func (l *watchedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.accepted <- struct{}{}:
		default:
		}
	}
	return conn, err
}

func (l *watchedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// TestHandoffAnswersAcceptedConnection is the regression for a request dropped at a
// handoff: the old server had accepted the connection, but the request arrived only
// after it stopped accepting, and it closed the connection without an answer. Control
// arm: after a plain stop the server still drops it.
func TestHandoffAnswersAcceptedConnection(t *testing.T) {
	for _, arm := range []string{"handoff", "stop"} {
		t.Run(arm, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			watched := &watchedListener{Listener: listener, accepted: make(chan struct{}, 1), closed: make(chan struct{})}
			ctx, stop := context.WithCancelCause(t.Context())
			served := make(chan error, 1)
			go func() {
				served <- Serve(ctx, watched, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
			}()
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(conn)
			select {
			case <-watched.accepted:
			case <-time.After(5 * time.Second):
				t.Fatal("the connection was not accepted")
			}
			defer stopFor(t, arm, stop)()
			select {
			case <-watched.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("the server did not stop accepting")
			}
			if arm == "stop" {
				// Requests end asynchronously after a stop; give the server time to shut down.
				time.Sleep(100 * time.Millisecond)
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: gateway\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if answered := err == nil && response.StatusCode == http.StatusOK; answered != (arm == "handoff") {
				t.Fatalf("answered=%v: %v", answered, err)
			}
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("old server did not stop")
			}
		})
	}
}
