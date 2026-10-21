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
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoService is a fake IP echo service behind a fake proxy. The proxy answers CONNECT
// with the proxy credentials of harness accounts and tunnels every target to the echo
// service, recording the targets and the Authorization headers of the CONNECTs; the
// echo service answers with ip and status, or with body when it is set (a 302 points
// at another host), and records the Authorization headers it received. A CONNECT to a
// target other than the echo host gets reach[proxy user] instead when it is set: that
// status, or no answer at all for noAnswer.
type echoService struct {
	proxy    *httptest.Server
	addr     string // the echo service itself, which a direct account reaches without the proxy
	roots    *x509.CertPool
	mu       sync.Mutex
	ip       string
	status   int
	body     string
	reach    map[string]int
	targets  []string
	tokens   []string
	connects []string
}

// noAnswer as a reach status makes the proxy close the connection without answering.
const noAnswer = -1

// testUpstream is the upstream the egress checks of these tests ask the exits to reach.
var testUpstream = &url.URL{Scheme: "https", Host: "api.anthropic.com"}

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
	s.addr = echo.Listener.Addr().String()
	s.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := basicUser(r.Header.Get("Proxy-Authorization"))
		if r.Method != http.MethodConnect || !ok || !strings.HasPrefix(user, "synthetic-user-") {
			http.Error(w, "proxy credentials", http.StatusProxyAuthRequired)
			return
		}
		s.mu.Lock()
		s.targets = append(s.targets, r.Host)
		s.connects = append(s.connects, r.Header.Get("Authorization"))
		reach, refuse := s.reach[user]
		s.mu.Unlock()
		if refuse && r.Host != "ip.oxylabs.io:443" {
			if reach == noAnswer {
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			http.Error(w, "forbidden by the exit", reach)
			return
		}
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

// setReach makes the proxy answer the CONNECTs of alias's account to any target but the
// echo host with status (noAnswer: none); 0 tunnels them again.
func (s *echoService) setReach(alias string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reach == nil {
		s.reach = map[string]int{}
	}
	if status == 0 {
		delete(s.reach, "synthetic-user-"+alias)
		return
	}
	s.reach["synthetic-user-"+alias] = status
}

// seen returns the CONNECT targets and the Authorization headers of the CONNECTs so far.
func (s *echoService) seen() (targets, authorizations []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.targets), slices.Clone(s.connects)
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
	e := NewEgress(h.accounts, h.store, testUpstream)
	e.rootCAs = echo.roots
	return h, e
}

// TestEgressCheck checks the three outcomes of one check through the account's proxy:
// the expected IP passes, another IP is a mismatch, and an answer without an IP (a 5xx,
// a body that is not the echo service's) is a failed check and not a mismatch. Every
// check asks the proxy for the fixed echo host, then, unless the IP differs, for the
// upstream's host, and sends no account token.
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
			wantTargets := []string{"ip.oxylabs.io:443", "api.anthropic.com:443"}
			if test.mismatch {
				wantTargets = wantTargets[:1]
			}
			targets, connects := echo.seen()
			if !slices.Equal(targets, wantTargets) || slices.ContainsFunc(connects, func(v string) bool { return v != "" }) || !slices.Equal(echo.tokens, []string{""}) {
				t.Fatalf("proxy targets %v, CONNECT authorization %q, echo authorization %q", targets, connects, echo.tokens)
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
// so is one whose check could not read an IP. One whose exit refuses the CONNECT to
// the upstream joins the set aside, so a later sweep can end that; one whose CONNECT
// gets no answer joins and is not set aside.
func TestReloadWithEgressCheck(t *testing.T) {
	for _, arm := range []string{"match", "mismatch", "echo_down", "upstream_refused", "upstream_unanswered"} {
		t.Run(arm, func(t *testing.T) {
			// The inventory files expect 192.0.2.1 (inventoryYAML).
			echo := newEchoService(t, map[string]string{"mismatch": "198.51.100.7"}[arm])
			switch arm {
			case "match", "upstream_refused", "upstream_unanswered":
				echo.set("192.0.2.1", http.StatusOK, "")
			case "echo_down":
				echo.set("192.0.2.1", http.StatusBadGateway, "")
			}
			echo.setReach("sample-b", map[string]int{"upstream_refused": http.StatusForbidden, "upstream_unanswered": noAnswer}[arm])
			h := newReloadHarness(t, onlyA)
			e := NewEgress(h.accounts, h.store, testUpstream)
			e.rootCAs = echo.roots
			// Every account's exit is the fake exit; the check reaches the echo proxy instead.
			proxy, _ := url.Parse(echo.proxy.URL)
			h.rewrite(aAndB)
			check := func(ctx context.Context, a *account) error {
				a.exit, a.proxyUsername = proxy, "synthetic-user-"+a.alias
				return e.Check(ctx, a)
			}
			r, err := h.reload(check)
			if err != nil {
				t.Fatal(err)
			}
			joined := arm == "match" || arm == "upstream_refused" || arm == "upstream_unanswered"
			wantSetAside := map[string][]string{"upstream_refused": {"sample-b"}}[arm]
			if joined && (!slices.Equal(r.Added, []string{"sample-b"}) || r.Rejected != nil || !slices.Equal(h.accounts.SetAside(), wantSetAside)) {
				t.Fatalf("%s: %+v, set aside %v", arm, r, h.accounts.SetAside())
			}
			if arm == "upstream_refused" {
				echo.setReach("sample-b", 0)
				if err := e.Sweep(t.Context()); err != nil || h.accounts.SetAside() != nil {
					t.Fatalf("sweep %v, set aside %v after the exit reaches the upstream", err, h.accounts.SetAside())
				}
			}
			if !joined && (!slices.Equal(r.Rejected, []string{"sample-b"}) || r.Added != nil || !slices.Equal(h.accounts.Candidates(), []string{"sample-a"})) {
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
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": "192.0.2.1", "sample-c": "192.0.2.1"})
	for alias, reason := range map[string]string{"sample-a": "upstream_401", "sample-b": "synthetic-session-123 synthetic-sensitive-value", "sample-c": upstreamFailedReason} {
		if err := h.store.Pause(t.Context(), alias, indefinitePause, reason); err != nil {
			t.Fatal(err)
		}
	}
	answer := readHealth(t, e)
	if want := []string{"sample-a: paused (upstream_401)", "sample-b: paused (manual)", "sample-c: paused (upstream_failed)"}; answer.Status != "degraded" || !slices.Equal(answer.Reasons, want) {
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
			e := NewEgress(h.accounts, h.store, testUpstream)
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
	e := NewEgress(h.accounts, h.store, testUpstream)
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

// TestEgressUpstreamReach checks the reach check of the egress sweep. An exit that
// answers the CONNECT to the upstream's host with 403 sets its account aside: it is
// not chosen, nothing is paused and the health endpoint and status name it; the other account,
// whose exit tunnels, serves. A later sweep whose CONNECT gets 200 ends the set-aside
// without resume, and a CONNECT without an answer neither sets aside nor ends one and
// marks health degraded. Control arm: exits that tunnel set nothing aside.
func TestEgressUpstreamReach(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": "192.0.2.1"})
	sweep := func(want health, setAside ...string) {
		t.Helper()
		if err := e.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := h.accounts.SetAside(); !slices.Equal(got, setAside) {
			t.Fatalf("set aside %v, want %v", got, setAside)
		}
		if paused := h.paused(); len(paused) != 0 {
			t.Fatalf("paused %v; the reach check pauses nothing", paused)
		}
		if answer := readHealth(t, e); answer.Status != want.Status || !slices.Equal(answer.Reasons, want.Reasons) {
			t.Fatalf("health %+v, want %+v", answer, want)
		}
	}
	bound := func() string {
		t.Helper()
		token, _ := h.issue()
		binding, _, err := h.accounts.bind(t.Context(), token, "local")
		if err != nil {
			t.Fatal(err)
		}
		return binding.Alias
	}
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	defer admin.Close()
	statusSetAside := func() any {
		t.Helper()
		status, body := adminCall(t, admin, http.MethodGet, "/status", "")
		if status != http.StatusOK {
			t.Fatalf("status answered %d", status)
		}
		return body["set_aside"]
	}
	sweep(health{Status: "ok"})

	echo.setReach("sample-a", http.StatusForbidden)
	sweep(health{"degraded", []string{"sample-a: exit cannot reach api.anthropic.com: 403"}}, "sample-a")
	if got := statusSetAside(); !reflect.DeepEqual(got, []any{"sample-a: exit cannot reach api.anthropic.com: 403"}) {
		t.Fatalf("status set_aside %v", got)
	}
	for range 3 {
		if alias := bound(); alias != "sample-b" {
			t.Fatalf("bound to %s while sample-a is set aside", alias)
		}
	}
	if targets, connects := echo.seen(); !slices.Contains(targets, "api.anthropic.com:443") || slices.ContainsFunc(connects, func(v string) bool { return v != "" }) {
		t.Fatalf("CONNECT targets %v, authorization %q", targets, connects)
	}

	echo.setReach("sample-a", noAnswer)
	sweep(health{"degraded", []string{"sample-a: exit cannot reach api.anthropic.com: 403", "sample-a: egress check got no answer to CONNECT api.anthropic.com"}}, "sample-a")

	echo.setReach("sample-a", 0)
	sweep(health{Status: "ok"})
	if got := statusSetAside(); got != nil {
		t.Fatalf("status set_aside %v after the exit reaches the upstream", got)
	}

	echo.setReach("sample-b", noAnswer)
	sweep(health{"degraded", []string{"sample-b: egress check got no answer to CONNECT api.anthropic.com"}})
}

// TestUpstreamReachKeepsPauses checks that the reach check neither ends nor replaces a
// pause: an account paused for upstream_401 and set aside keeps its pause, with its
// end, once a CONNECT gets 200 again; a manual pause likewise. An exit IP that differs
// pauses as before and the CONNECT is not asked, so a set-aside in effect stays.
func TestUpstreamReachKeepsPauses(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": "192.0.2.1", "sample-c": "192.0.2.9"})
	until := h.clock.Add(time.Hour)
	if err := h.store.Pause(t.Context(), "sample-a", until, "upstream_401"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Pause(t.Context(), "sample-b", indefinitePause, "synthetic manual pause"); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"sample-a", "sample-b", "sample-c"} {
		echo.setReach(alias, http.StatusForbidden)
	}
	// sample-c's IP matches during the first sweep only.
	h.accounts.set.Load().lookup("sample-c").expectedIP = netip.MustParseAddr("192.0.2.1")
	if err := e.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.accounts.SetAside(); !slices.Equal(got, []string{"sample-a", "sample-b", "sample-c"}) {
		t.Fatalf("set aside %v", got)
	}
	h.accounts.set.Load().lookup("sample-c").expectedIP = netip.MustParseAddr("192.0.2.9")
	for _, alias := range []string{"sample-a", "sample-b", "sample-c"} {
		echo.setReach(alias, 0)
	}
	before, _ := echo.seen()
	if err := e.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.accounts.SetAside(); !slices.Equal(got, []string{"sample-c"}) {
		t.Fatalf("set aside %v after the exits reach the upstream; want only sample-c, whose IP differs", got)
	}
	want := map[string]string{"sample-a": "upstream_401", "sample-b": "synthetic manual pause", "sample-c": EgressMismatchReason}
	if paused := h.paused(); !maps.Equal(paused, want) {
		t.Fatalf("paused %v, want %v", paused, want)
	}
	status, err := h.store.Status(t.Context(), []string{"sample-a"})
	if err != nil || !status.Accounts[0].PausedUntil.Equal(until) {
		t.Fatalf("sample-a paused until %v (%v), want %v", status.Accounts[0].PausedUntil, err, until)
	}
	after, _ := echo.seen()
	if n := strings.Count(strings.Join(after[len(before):], " "), "api.anthropic.com:443"); n != 2 {
		t.Fatalf("%d CONNECTs to the upstream in the second sweep, want 2 (none for sample-c)", n)
	}
}

// TestReloadMigrationSkipsSetAside checks that a reload's migration does not move a
// session to an account the egress check set aside: the session bound to sample-a,
// whose token changes, is left unplaced while sample-b is set aside. Control arm:
// with sample-b not set aside the session moves to it.
func TestReloadMigrationSkipsSetAside(t *testing.T) {
	for _, setAside := range []bool{true, false} {
		t.Run(map[bool]string{true: "set_aside", false: "reachable"}[setAside], func(t *testing.T) {
			h := newReloadHarness(t, aAndB)
			if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
				t.Fatal("session not on sample-a")
			}
			if setAside {
				reason := "exit cannot reach api.anthropic.com: 403"
				h.accounts.set.Load().lookup("sample-b").setAside.Store(&reason)
			}
			h.rewrite(map[string]string{"sample-a": "op://example-vault/token-a-new/credential", "sample-b": "op://example-vault/token-b/credential"})
			r, err := h.reload(nil)
			if err != nil {
				t.Fatal(err)
			}
			want, wantMoved, wantUnplaced := "sample-b", 1, 0
			if setAside {
				want, wantMoved, wantUnplaced = "", 0, 1
			}
			if r.Moved != wantMoved || r.Unplaced != wantUnplaced || h.bound(h.sessionID) != want {
				t.Fatalf("reload %+v, session on %q; want moved %d, unplaced %d, on %q", r, h.bound(h.sessionID), wantMoved, wantUnplaced, want)
			}
		})
	}
}

// TestDirectEgressCheck checks the egress check of a direct account: it reads the IP
// echo and dials the upstream from the gateway, without the proxy and in the family of
// expected_egress_ip only, and sends nothing on the upstream connection. A connection is
// the exit's 200; a failed dial is no answer, marking health degraded and setting
// nothing aside; an IP that differs pauses the account and skips the dial.
func TestDirectEgressCheck(t *testing.T) {
	for _, test := range []struct {
		name, expected, read string
		reachable            bool
		upstream             string // the check's upstream outcome
		health               health
		paused               map[string]string
	}{
		{"reaches_ipv4", "192.0.2.7", "192.0.2.7", true, "ok", health{Status: "ok"}, map[string]string{}},
		{"reaches_ipv6", "2001:db8::7", "2001:db8::7", true, "ok", health{Status: "ok"}, map[string]string{}},
		{"dial_fails", "192.0.2.7", "192.0.2.7", false, "unanswered", health{"degraded", []string{"sample-d: egress check got no answer to CONNECT api.anthropic.com"}}, map[string]string{}},
		{"ip_differs", "192.0.2.7", "198.51.100.7", true, "", health{"degraded", []string{"sample-d: paused (egress_ip_mismatch)"}}, map[string]string{"sample-d": EgressMismatchReason}},
	} {
		t.Run(test.name, func(t *testing.T) {
			echo := newEchoService(t, test.read)
			upstream, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			received := make(chan int, 1)
			go func() {
				conn, err := upstream.Accept()
				if err != nil {
					return
				}
				n, _ := io.Copy(io.Discard, conn)
				received <- int(n)
			}()
			reachable := upstream.Addr().String()
			if !test.reachable {
				closeQuietly(upstream)
			} else {
				t.Cleanup(func() { closeQuietly(upstream) })
			}
			h := newHarness(t)
			h.add("sample-d", "sk-ant-oat01-synthetic-d-token")
			a, err := newAccount("sample-d", accountInput{Credential: claudeCredential{token: "sk-ant-oat01-synthetic-d-token"}, Direct: true, ExpectedIP: netip.MustParseAddr(test.expected)}, DefaultAccountConcurrency)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var dialed []string
			a.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, network+" "+address)
				mu.Unlock()
				target := map[string]string{"ip.oxylabs.io:443": echo.addr, "api.anthropic.com:443": reachable}[address]
				if target == "" {
					return nil, errors.New("unexpected target")
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", target)
			}
			h.accounts.set.Load().byAlias["sample-d"] = a
			e := NewEgress(h.accounts, h.store, testUpstream)
			e.rootCAs = echo.roots
			if err := e.Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			network := map[bool]string{true: "tcp4", false: "tcp6"}[netip.MustParseAddr(test.expected).Is4()]
			want := []string{network + " ip.oxylabs.io:443", network + " api.anthropic.com:443"}
			if test.upstream == "" {
				want = want[:1]
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(dialed, want) {
				t.Fatalf("dialed %v, want %v", dialed, want)
			}
			if targets, _ := echo.seen(); len(targets) != 0 {
				t.Fatalf("the proxy saw %v", targets)
			}
			if got := e.lastCheck(a).Upstream; got != test.upstream {
				t.Fatalf("upstream outcome %q, want %q", got, test.upstream)
			}
			if test.upstream == "ok" {
				if n := <-received; n != 0 {
					t.Fatalf("the check sent %d bytes to the upstream", n)
				}
			}
			if got := h.accounts.SetAside(); len(got) != 0 {
				t.Fatalf("set aside %v", got)
			}
			if got := h.paused(); !maps.Equal(got, test.paused) {
				t.Fatalf("paused %v, want %v", got, test.paused)
			}
			if answer := readHealth(t, e); answer.Status != test.health.Status || !slices.Equal(answer.Reasons, test.health.Reasons) {
				t.Fatalf("health %+v, want %+v", answer, test.health)
			}
		})
	}
}
