package claude

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// waitSeen waits until the fake exit has seen n requests.
func waitSeen(t *testing.T, e *exit, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.seen()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("exit saw %d requests, want %d", len(e.seen()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAccountConcurrency checks that the account concurrency sets each account's
// request slots and connection pool, also after a reload: with 2, a third request
// while two are held at the exit is refused locally with 503 and recorded as limited;
// with the default, the third reaches the exit.
func TestAccountConcurrency(t *testing.T) {
	for _, concurrency := range []int{2, DefaultAccountConcurrency} {
		t.Run(map[int]string{2: "two", DefaultAccountConcurrency: "default"}[concurrency], func(t *testing.T) {
			h := newReloadHarness(t, onlyA)
			set, err := LoadAccounts(t.Context(), h.dir, h.resolve, concurrency)
			if err != nil {
				t.Fatal(err)
			}
			h.accounts.set.Store(set)
			h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential"})
			if _, err := h.reload(nil); err != nil {
				t.Fatal(err)
			}
			a := h.accounts.set.Load().lookup("sample-a")
			if cap(a.slots) != concurrency || a.transport.MaxConnsPerHost != concurrency || a.transport.MaxIdleConnsPerHost != concurrency || a.transport.MaxIdleConns != concurrency {
				t.Fatalf("slots %d, pool %d/%d/%d after the reload; want %d", cap(a.slots), a.transport.MaxConnsPerHost, a.transport.MaxIdleConnsPerHost, a.transport.MaxIdleConns, concurrency)
			}
			release := make(chan struct{})
			h.exit.mu.Lock()
			h.exit.hold = release
			h.exit.mu.Unlock()
			statuses := make(chan int, 3)
			post := func() {
				res, err := h.clientFor(h.token).Post(h.gateway.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
				if err != nil {
					statuses <- 0
					return
				}
				_ = res.Body.Close()
				statuses <- res.StatusCode
			}
			go post()
			go post()
			waitSeen(t, h.exit, 2)
			go post()
			if concurrency == 2 {
				if status := <-statuses; status != http.StatusServiceUnavailable {
					t.Fatalf("third request: status %d, want 503", status)
				}
			} else {
				waitSeen(t, h.exit, 3)
			}
			close(release)
			for range map[int]int{2: 2, DefaultAccountConcurrency: 3}[concurrency] {
				if status := <-statuses; status != http.StatusOK {
					t.Fatalf("held request: status %d", status)
				}
			}
			if concurrency == 2 {
				rows := waitTraffic(t, h.harness, 3)
				if rows[0].result != "limited" {
					t.Fatalf("first recorded row %+v, want the refused request as limited", rows[0])
				}
			}
		})
	}
}

// TestTunnelLimit checks that the tunnel limit bounds the open tunnels: with 2, a third
// CONNECT while two are open is refused with 503 and recorded as limited; with the
// default, the third opens.
func TestTunnelLimit(t *testing.T) {
	for _, tunnels := range []int{2, DefaultLimits.Tunnels} {
		t.Run(map[int]string{2: "two", DefaultLimits.Tunnels: "default"}[tunnels], func(t *testing.T) {
			exit := startFakeExit(t, 200)
			h := newHarness(t)
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
			h.token, h.sessionID = h.issue()
			forward := newForwardProxy(h.store, h.accounts, nil)
			limits := DefaultLimits
			limits.Tunnels = tunnels
			forward.setLimits(limits)
			server := httptest.NewServer(forward)
			defer server.Close()
			address := server.Listener.Addr().String()
			for range 2 {
				if _, _, response := connect(t, address, "example.invalid:443", proxyAuthorization(h.token)); response.StatusCode != 200 {
					t.Fatalf("open tunnel: status %d", response.StatusCode)
				}
			}
			_, _, third := connect(t, address, "example.invalid:443", proxyAuthorization(h.token))
			want := map[int]int{2: http.StatusServiceUnavailable, DefaultLimits.Tunnels: http.StatusOK}[tunnels]
			if third.StatusCode != want {
				t.Fatalf("third tunnel: status %d, want %d", third.StatusCode, want)
			}
			if tunnels == 2 {
				if rows := waitTraffic(t, h, 1); rows[0].result != "limited" {
					t.Fatalf("first recorded row %+v, want the refused tunnel as limited", rows[0])
				}
			}
		})
	}
}
