package claude

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoService is a fake IP echo service behind a fake proxy. The proxy answers CONNECT
// with the proxy credentials of harness accounts and tunnels every target to the echo
// service, recording the targets; the echo service answers with ip and status, or
// with body when it is set (a 302 points at another host), and records the Authorization headers it received.
type echoService struct {
	proxy   *httptest.Server
	roots   *x509.CertPool
	mu      sync.Mutex
	ip      string
	status  int
	body    string
	targets []string
	tokens  []string
}

// newEchoService starts the echo service with a certificate for the real echo host,
// so the check's fixed URL reaches it through the proxy, and the proxy in front of it.
func newEchoService(t *testing.T, ip string) *echoService {
	t.Helper()
	s := &echoService{ip: ip, status: http.StatusOK}
	certificate, roots := echoCertificate(t, "ip.oxylabs.io")
	s.roots = roots
	echo := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tokens = append(s.tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/location" {
			http.NotFound(w, r)
			return
		}
		body := s.body
		if body == "" {
			body = `{"ip":"` + s.ip + `","providers":{"maxmind":{"country":"US"}}}`
		}
		if s.status == http.StatusFound {
			w.Header().Set("Location", "https://other.example.test/location")
		}
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, body)
	}))
	echo.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	echo.StartTLS()
	t.Cleanup(echo.Close)
	s.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := basicUser(r.Header.Get("Proxy-Authorization"))
		if r.Method != http.MethodConnect || !ok || !strings.HasPrefix(user, "synthetic-user-") {
			http.Error(w, "proxy credentials", http.StatusProxyAuthRequired)
			return
		}
		s.mu.Lock()
		s.targets = append(s.targets, r.Host)
		s.mu.Unlock()
		upstream, err := net.Dial("tcp", echo.Listener.Addr().String())
		if err != nil {
			http.Error(w, "dial", http.StatusBadGateway)
			return
		}
		// Hijack before answering: Hijack flushes a written header before it stops the
		// server's background read, which can then take the client's first TLS byte.
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = upstream.Close()
			_ = client.Close()
			return
		}
		go func() { _, _ = io.Copy(upstream, buffered.Reader); _ = upstream.Close() }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}))
	t.Cleanup(s.proxy.Close)
	return s
}

// echoCertificate returns a self-signed certificate for host and a pool trusting it.
func echoCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// set changes what the echo service answers.
func (s *echoService) set(ip string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ip, s.status, s.body = ip, status, body
}

// newEgressHarness returns a harness with one account per alias behind the echo
// service's proxy, each expecting expected[alias], and the egress check over them.
func newEgressHarness(t *testing.T, echo *echoService, expected map[string]string) (*harness, *Egress) {
	t.Helper()
	h := newHarness(t)
	exit, _ := url.Parse(echo.proxy.URL)
	for alias, ip := range expected {
		h.addWithExit(alias, "sk-ant-oat01-synthetic-"+alias+"-token", exit).expectedIP = netip.MustParseAddr(ip)
	}
	e := NewEgress(h.accounts, h.store)
	e.rootCAs = echo.roots
	return h, e
}

// TestEgressCheck checks the three outcomes of one check through the account's proxy:
// the expected IP passes, another IP is a mismatch, and an answer without an IP (a 5xx,
// a body that is not the echo service's) is a failed check and not a mismatch. Every
// check asks the proxy for the fixed echo host and sends no account token.
func TestEgressCheck(t *testing.T) {
	for _, test := range []struct {
		name     string
		ip       string
		status   int
		body     string
		mismatch bool
		failed   bool
	}{
		{"expected_ip", "192.0.2.1", 200, "", false, false},
		{"other_ip", "198.51.100.7", 200, "", true, false},
		{"server_error", "192.0.2.1", 503, "", false, true},
		{"not_json", "", 200, "192.0.2.1\n", false, true},
		{"redirect", "", 302, `{"ip":"192.0.2.1"}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			echo := newEchoService(t, test.ip)
			echo.set(test.ip, test.status, test.body)
			h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1"})
			a := h.accounts.set.Load().lookup("sample-a")
			err := e.Check(t.Context(), a)
			if (err != nil) != (test.mismatch || test.failed) || errors.Is(err, errEgressMismatch) != test.mismatch || e.failedCheck(a) != test.failed {
				t.Fatalf("err %v, failed %t; want mismatch %t, failed %t", err, e.failedCheck(a), test.mismatch, test.failed)
			}
			if err != nil && strings.Contains(err.Error(), "Synthetic-Password-") {
				t.Fatalf("error carries the proxy password: %v", err)
			}
			if !slices.Equal(echo.targets, []string{"ip.oxylabs.io:443"}) || !slices.Equal(echo.tokens, []string{""}) {
				t.Fatalf("proxy targets %v, echo authorization %q", echo.targets, echo.tokens)
			}
		})
	}
}

// TestEgressSweep checks startup and periodic sweeps: an account whose exit IP differs
// is paused with egress_ip_mismatch while the one that matches stays available, and the
// health endpoint reports the pause. Control arms: when every IP matches nothing is
// paused and the endpoint is ok; when the echo service fails nothing is paused either
// and the endpoint reports the failed checks.
func TestEgressSweep(t *testing.T) {
	for _, arm := range []string{"mismatch", "match", "echo_down"} {
		t.Run(arm, func(t *testing.T) {
			echo := newEchoService(t, "192.0.2.1")
			expectedB := map[string]string{"mismatch": "192.0.2.2", "match": "192.0.2.1", "echo_down": "192.0.2.1"}[arm]
			if arm == "echo_down" {
				echo.set("", http.StatusServiceUnavailable, "")
			}
			h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": expectedB})
			if err := e.Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, err := h.store.Status(t.Context(), h.accounts.Candidates())
			if err != nil {
				t.Fatal(err)
			}
			reasons := map[string]string{}
			for _, a := range status.Accounts {
				reasons[a.Alias] = a.PauseReason
			}
			answer := readHealth(t, e)
			want := map[string]health{
				"mismatch":  {"degraded", []string{"sample-b: paused (egress_ip_mismatch)"}},
				"match":     {"ok", nil},
				"echo_down": {"degraded", []string{"sample-a: egress check could not read the exit IP", "sample-b: egress check could not read the exit IP"}},
			}[arm]
			wantB := map[string]string{"mismatch": EgressMismatchReason}[arm]
			if reasons["sample-a"] != "" || reasons["sample-b"] != wantB || answer.Status != want.Status || !slices.Equal(answer.Reasons, want.Reasons) {
				t.Fatalf("pauses %v, health %+v; want sample-b %q, health %+v", reasons, answer, wantB, want)
			}
		})
	}
}

// readHealth reads the health endpoint.
func readHealth(t *testing.T, e *Egress) health {
	t.Helper()
	recorder := httptest.NewRecorder()
	e.HealthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	var answer health
	if recorder.Code != http.StatusOK || json.NewDecoder(recorder.Body).Decode(&answer) != nil {
		t.Fatalf("health answered %d", recorder.Code)
	}
	return answer
}

// TestEgressRun checks the periodic sweep: the exit IP changes after startup and a
// later sweep pauses the account; until then it stays available.
func TestEgressRun(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, 20*time.Millisecond) }()
	paused := func() string {
		status, err := h.store.Status(t.Context(), h.accounts.Candidates())
		if err != nil {
			t.Fatal(err)
		}
		return status.Accounts[0].PauseReason
	}
	time.Sleep(100 * time.Millisecond)
	if reason := paused(); reason != "" {
		t.Fatalf("paused %q while the exit IP matched", reason)
	}
	echo.set("198.51.100.7", http.StatusOK, "")
	deadline := time.Now().Add(5 * time.Second)
	for paused() != EgressMismatchReason {
		if time.Now().After(deadline) {
			t.Fatal("not paused after the exit IP changed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestReloadWithEgressCheck runs a reload with the gateway's check as its hook: an added
// account whose exit IP matches joins the set, one whose IP differs is rejected, and
// so is one whose check could not read an IP.
func TestReloadWithEgressCheck(t *testing.T) {
	for _, arm := range []string{"match", "mismatch", "echo_down"} {
		t.Run(arm, func(t *testing.T) {
			// The inventory files expect 192.0.2.1 (inventoryYAML).
			echo := newEchoService(t, map[string]string{"match": "192.0.2.1", "mismatch": "198.51.100.7", "echo_down": "192.0.2.1"}[arm])
			if arm == "echo_down" {
				echo.set("192.0.2.1", http.StatusBadGateway, "")
			}
			h := newReloadHarness(t, onlyA)
			e := NewEgress(h.accounts, h.store)
			e.rootCAs = echo.roots
			// Every account's exit is the fake exit; the check reaches the echo proxy instead.
			proxy, _ := url.Parse(echo.proxy.URL)
			h.rewrite(aAndB)
			check := func(ctx context.Context, a *account) error {
				through := *a
				through.exit, through.proxyUsername = proxy, "synthetic-user-"+a.alias
				return e.Check(ctx, &through)
			}
			r, err := h.reload(check)
			if err != nil {
				t.Fatal(err)
			}
			if arm == "match" && (!slices.Equal(r.Added, []string{"sample-b"}) || r.Rejected != nil) {
				t.Fatalf("match: %+v", r)
			}
			if arm != "match" && (!slices.Equal(r.Rejected, []string{"sample-b"}) || r.Added != nil || !slices.Equal(h.accounts.Candidates(), []string{"sample-a"})) {
				t.Fatalf("%s: %+v, candidates %v", arm, r, h.accounts.Candidates())
			}
		})
	}
}

// TestHealthPauseReasons checks that the health endpoint shows the gateway's own pause
// reasons as they are and an operator's free-text reason only as manual; the text, which
// may name a session or carry anything else, stays on the management interface.
func TestHealthPauseReasons(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": "192.0.2.1"})
	for alias, reason := range map[string]string{"sample-a": "upstream_401", "sample-b": "synthetic-session-123 synthetic-sensitive-value"} {
		if err := h.store.Pause(t.Context(), alias, indefinitePause, reason); err != nil {
			t.Fatal(err)
		}
	}
	answer := readHealth(t, e)
	if want := []string{"sample-a: paused (upstream_401)", "sample-b: paused (manual)"}; answer.Status != "degraded" || !slices.Equal(answer.Reasons, want) {
		t.Fatalf("health %+v, want degraded %v", answer, want)
	}
}

// TestEgressPauseExcludesReload checks that a sweep's pause cannot land after a reload
// published a new configuration of the account: the sweep holds the old configuration's
// mismatch between its comparison and its pause, and a reload that replaces the account
// meanwhile publishes only after the pause. Control arm without the exclusive region:
// the reload publishes while the sweep still holds, so the pause lands on the new
// configuration.
func TestEgressPauseExcludesReload(t *testing.T) {
	for _, exclusive := range []bool{true, false} {
		t.Run(map[bool]string{true: "exclusive", false: "no_exclusion"}[exclusive], func(t *testing.T) {
			// The inventory files expect 192.0.2.1 (inventoryYAML); the old account expects another IP.
			echo := newEchoService(t, "192.0.2.1")
			h := newReloadHarness(t, onlyA)
			h.accounts.noExclusion = !exclusive
			e := NewEgress(h.accounts, h.store)
			e.rootCAs = echo.roots
			proxy, _ := url.Parse(echo.proxy.URL)
			old := h.accounts.set.Load().lookup("sample-a")
			old.exit, old.proxyUsername, old.expectedIP = proxy, "synthetic-user-sample-a", netip.MustParseAddr("192.0.2.9")
			held, release := make(chan struct{}), make(chan struct{})
			h.accounts.beforePause = func() { close(held); <-release }
			swept := make(chan error, 1)
			go func() { swept <- e.Sweep(t.Context()) }()
			select {
			case <-held:
			case err := <-swept:
				t.Fatalf("sweep returned without reaching the pause: %v", err)
			}
			h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential"})
			reloaded := make(chan error, 1)
			go func() {
				_, err := h.reload(nil)
				reloaded <- err
			}()
			time.Sleep(200 * time.Millisecond)
			published := h.accounts.set.Load().lookup("sample-a") != old
			close(release)
			if err := <-swept; err != nil {
				t.Fatal(err)
			}
			if err := <-reloaded; err != nil {
				t.Fatal(err)
			}
			if published == exclusive {
				t.Fatalf("published while the sweep held its pause: %t", published)
			}
		})
	}
}

// TestEgressHealthFollowsConfiguration checks that a failed check of a configuration a
// reload replaced does not mark the new one: the health endpoint is degraded while the
// old configuration serves and ok once the reload published the new one.
func TestEgressHealthFollowsConfiguration(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	echo.set("", http.StatusServiceUnavailable, "")
	h := newReloadHarness(t, onlyA)
	e := NewEgress(h.accounts, h.store)
	e.rootCAs = echo.roots
	proxy, _ := url.Parse(echo.proxy.URL)
	old := h.accounts.set.Load().lookup("sample-a")
	old.exit, old.proxyUsername = proxy, "synthetic-user-sample-a"
	if err := e.Check(t.Context(), old); err == nil || readHealth(t, e).Status != "degraded" {
		t.Fatalf("check %v; want a failed check and a degraded endpoint", err)
	}
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential"})
	if _, err := h.reload(nil); err != nil {
		t.Fatal(err)
	}
	if answer := readHealth(t, e); answer.Status != "ok" {
		t.Fatalf("health %+v after the reload replaced the failed configuration", answer)
	}
}
