package claude

import (
	"errors"
	"io"
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

// TestProviderHooks checks the hooks a provider adds to the reverse proxy: the prefix
// its upstream URL stands for, the screen that answers before authentication, whose
// 426 is recorded as upgrade_declined and any other status as refused, and the guard
// on outgoing requests. The control arm runs the same requests through a provider
// without the screen and the guard, and they reach the upstream.
func TestProviderHooks(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	plain := Claude
	plain.Requests = map[string]bool{"POST /api/x/v1/messages": true}
	plain.Prefix = "/api/x"
	hooked := plain
	hooked.Screen = func(r *http.Request) (int, string) {
		if r.Header.Get("Upgrade") != "" {
			return http.StatusUpgradeRequired, "upgrade refused"
		}
		if r.Header.Get("X-Screen") != "" {
			return http.StatusBadRequest, "screened"
		}
		return 0, ""
	}
	hooked.Guard = func(out *http.Request) error {
		if out.Header.Get("X-Refuse") != "" {
			return errors.New("refused by the guard")
		}
		return nil
	}
	for _, test := range []struct {
		name     string
		provider Provider
		header   string
		status   int
		path     string // the path the upstream saw; empty when it saw nothing
	}{
		{"prefix", hooked, "", 200, "/base/v1/messages"},
		{"screened", hooked, "Upgrade", 426, ""},
		{"screened_other", hooked, "X-Screen", 400, ""},
		{"guarded", hooked, "X-Refuse", 502, ""},
		{"no_screen", plain, "Upgrade", 200, "/base/v1/messages"},
		{"no_guard", plain, "X-Refuse", 200, "/base/v1/messages"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			paths = nil
			mu.Unlock()
			h := newHarness(t, "sample-a")
			server := httptest.NewServer(newRouter(h.store, h.accounts, target, test.provider, accountTransport{}))
			t.Cleanup(server.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/x/v1/messages", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			if test.header != "" {
				req.Header.Set(test.header, "websocket")
			}
			res, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if res.StatusCode != test.status || test.path == "" && len(paths) != 0 || test.path != "" && (len(paths) != 1 || paths[0] != test.path) {
				t.Fatalf("status %d, upstream saw %v; want %d and %q", res.StatusCode, paths, test.status, test.path)
			}
			if result := map[string]string{"screened": "upgrade_declined", "screened_other": "refused"}[test.name]; result != "" {
				if rows := h.trafficRows(1); len(rows) != 1 || rows[0].result != result || rows[0].request != "POST /api/x/v1/messages" {
					t.Fatalf("screened request's row: %+v, want %s", rows, result)
				}
			}
		})
	}
}

// TestProviderWithoutProbeOrCapture checks that a provider without a Probe refuses a
// quota refresh, sending nothing, and one without Capture refuses a capture directory.
// The control arm refreshes with Claude's probe, which reaches the upstream.
func TestProviderWithoutProbeOrCapture(t *testing.T) {
	h := newHarness(t, "sample-a")
	provider := Claude
	provider.Probe, provider.Capture = nil, false
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRouter(h.store, h.accounts, target, provider, accountTransport{}).refresh(t.Context(), "sample-a"); !errors.Is(err, errNoProbe) || requests.Load() != 0 {
		t.Fatalf("refresh without a probe: %v, %d upstream requests", err, requests.Load())
	}
	dir, limits := t.TempDir(), Limits{Idle: time.Second, MaxRequest: time.Minute, MaxTunnel: time.Minute, Tunnels: 1}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(target, provider, dir, h.store, h.accounts, limits); err == nil {
		t.Fatal("a capture directory was accepted for a provider without Capture")
	}
	if _, err := NewHandler(target, Claude, dir, h.store, h.accounts, limits); err != nil {
		t.Fatalf("Claude's capture directory: %v", err)
	}
	if _, err := newRouter(h.store, h.accounts, target, Claude, accountTransport{}).refresh(t.Context(), "sample-a"); err != nil || requests.Load() != 1 {
		t.Fatalf("refresh with Claude's probe: %v, %d upstream requests", err, requests.Load())
	}
}
