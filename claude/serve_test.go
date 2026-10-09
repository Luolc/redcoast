package claude

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunServersStopsTogether checks that one server's failure stops the others and
// that the failure is returned; control arm: with no failure, canceling ctx stops all
// of them with no error.
func TestRunServersStopsTogether(t *testing.T) {
	failure := errors.New("listener failed")
	waiting := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	err := RunServers(t.Context(), waiting, func(context.Context) error { return failure }, waiting)
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- RunServers(ctx, waiting, waiting) }()
	select {
	case err := <-done:
		t.Fatalf("servers stopped on their own: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("servers did not stop with ctx")
	}
}

// TestRunFinishesAfterServerFailure is the regression for the failure path: a server
// fails while a CONNECT tunnel is open and its record is being written; Run returns
// the failure only after the tunnel has ended and its capture record and traffic row
// are saved. Control arm: RunServers alone returns before the row exists.
func TestRunFinishesAfterServerFailure(t *testing.T) {
	for _, finishes := range []bool{false, true} {
		t.Run(map[bool]string{false: "run_servers_only", true: "run"}[finishes], func(t *testing.T) {
			exit := startFakeExit(t, 200)
			h := newHarness(t)
			h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			h.token, h.sessionID = h.issue()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			records := &RecordStore{dir: dir}
			release := make(chan struct{})
			forward := newForwardProxy(h.store, h.accounts, func(record connectRecord) error {
				<-release
				return records.writeConnect(record)
			})
			entrypoint := &Entrypoint{Forward: forward, forward: forward, records: records}
			free, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := free.Addr().String()
			fail := make(chan struct{})
			failure := errors.New("session socket failed")
			failing := func(ctx context.Context) error {
				select {
				case <-fail:
					return failure
				case <-ctx.Done():
					return nil
				}
			}
			serveForward := func(ctx context.Context) error { return Serve(ctx, free, forward) }
			done := make(chan error, 1)
			go func() {
				if finishes {
					done <- entrypoint.Run(t.Context(), serveForward, failing)
				} else {
					done <- RunServers(t.Context(), serveForward, failing)
				}
			}()
			var conn net.Conn
			for deadline := time.Now().Add(3 * time.Second); conn == nil; {
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
			// The other server fails with the tunnel open; the cancellation ends the tunnel
			// and its record write blocks on release.
			close(fail)
			if finishes {
				select {
				case err := <-done:
					t.Fatalf("Run returned before the tunnel was recorded: %v", err)
				case <-time.After(300 * time.Millisecond):
				}
				close(release)
			}
			var result error
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("servers did not stop")
			}
			if !errors.Is(result, failure) {
				t.Fatalf("err=%v", result)
			}
			files, _ := filepath.Glob(filepath.Join(dir, "connect-*.json"))
			rows := h.traffic()
			if finishes && (len(files) != 1 || len(rows) != 1 || rows[0].result != "ok") {
				t.Fatalf("Run returned without the tunnel's record: files=%d rows=%+v", len(files), rows)
			}
			if !finishes {
				if len(files) != 0 || len(rows) != 0 {
					t.Fatal("control arm: the record was saved before the sink was released")
				}
				close(release)
				if err := entrypoint.Finish(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
