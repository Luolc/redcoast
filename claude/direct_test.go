package claude

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// directYAML lists hosts in a direct-hosts.yaml.
func directYAML(hosts ...string) string {
	return "hosts:\n  - " + strings.Join(hosts, "\n  - ") + "\n"
}

// TestReadDirectHosts checks which lists load and how a CONNECT target is matched:
// exactly by host and port, the host's case and trailing dot ignored. A list holding
// an IP literal, a wildcard or a target without a port is refused whole.
func TestReadDirectHosts(t *testing.T) {
	account := inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-a/credential", "8001")
	for _, test := range []struct {
		name    string
		list    string // "" writes no direct-hosts.yaml
		direct  []string
		account []string
		refused bool
	}{
		{"no_file", "", nil, []string{"github.com:443"}, false},
		{"exact", directYAML("github.com:443", "PyPI.org.:443"),
			[]string{"github.com:443", "GitHub.COM:443", "github.com.:443", "pypi.org:443"},
			[]string{"github.com:22", "api.github.com:443", "gist.github.com:443", "203.0.113.3:443", "pypi.org.evil.test:443"}, false},
		{"ipv4_literal", directYAML("github.com:443", "203.0.113.3:443"), nil, nil, true},
		{"short_ipv4_literal", directYAML("127.1:443"), nil, nil, true},
		{"wildcard", directYAML("*.github.com:443"), nil, nil, true},
		{"no_port", directYAML("github.com"), nil, nil, true},
		{"port_out_of_range", directYAML("github.com:70000"), nil, nil, true},
		{"unknown_key", "hosts: []\nwildcards: true\n", nil, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{"sample-a.yaml": account}
			if test.list != "" {
				files[directHostsFile] = test.list
			}
			set, err := LoadAccounts(t.Context(), writeInventory(t, files), (&reloadHarness{}).resolve, DefaultAccountConcurrency)
			if test.refused {
				if err == nil || !strings.Contains(err.Error(), directHostsFile) {
					t.Fatalf("list accepted or error does not name the file: %v", err)
				}
				return
			}
			// The list is not read as an account.
			if err != nil || !slices.Equal(set.Candidates(), []string{"sample-a"}) {
				t.Fatalf("candidates %v err %v", set, err)
			}
			for _, target := range test.direct {
				if !set.direct(target) {
					t.Errorf("%s not direct", target)
				}
			}
			for _, target := range test.account {
				if set.direct(target) {
					t.Errorf("%s direct", target)
				}
			}
		})
	}
}

// TestForwardProxyDirectRoute checks the two routes of a CONNECT with github.com:443
// listed: a listed target, in any case and with a trailing dot, is resolved and its
// public address dialed, and its row says direct; an unlisted target goes through the
// account's exit and says account. A direct target that cannot be reached fails
// without trying the exit.
func TestForwardProxyDirectRoute(t *testing.T) {
	exit := startFakeExit(t, 200)
	h, forward, dialed := directHarness(t, exit, map[string][]netip.Addr{
		"github.com":        {netip.MustParseAddr("192.0.2.10")},
		"down.example.test": {netip.MustParseAddr("192.0.2.20")},
	})
	exitAddress := exit.listener.Addr().String()
	for _, test := range []struct {
		target, dialed, route, result string
		status                        int
	}{
		{"github.com:443", "192.0.2.10:443", "direct", "ok", 200},
		{"GitHub.COM.:443", "192.0.2.10:443", "direct", "ok", 200},
		{"github.com:8443", exitAddress, "account", "ok", 200},
		{"api.github.com:443", exitAddress, "account", "ok", 200},
		{"203.0.113.3:443", exitAddress, "account", "ok", 200},
		{"down.example.test:443", "192.0.2.20:443", "direct", "egress_failed", 502},
	} {
		got := directConnect(t, h, forward, exit, dialed, test.target, test.status, test.result == "ok")
		if len(got.dialed) != 1 || got.dialed[0] != test.dialed {
			t.Fatalf("%s: dialed %q, want only %q", test.target, got.dialed, test.dialed)
		}
		if got.reachedExit != (test.route == "account") {
			t.Fatalf("%s: exit received a CONNECT: %v", test.target, got.reachedExit)
		}
		if got.route != test.route || got.result != test.result || got.alias != "sample-test" {
			t.Fatalf("%s: row %+v, want %s %s", test.target, got, test.route, test.result)
		}
	}
}

// tailnetAddr is the first address of 100.64.0.0/10, a synthetic tailnet address.
const tailnetAddr = "100.64.0.1"

// TestDirectRouteDialsOnlyPublicAddresses checks that a listed name resolving to the
// gateway machine, a private network, the tailnet or link-local (cloud metadata) is
// refused with 502 without any connection, the exit's included. Control arm: the same
// name resolving to such an address and a public one connects to the public one.
func TestDirectRouteDialsOnlyPublicAddresses(t *testing.T) {
	exit := startFakeExit(t, 200)
	blocked := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", tailnetAddr, "169.254.169.254", "fe80::1", "fd00::1", "0.0.0.0", "::ffff:127.0.0.1", "224.0.0.1"}
	addresses := map[string][]netip.Addr{"github.com": {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.10")}}
	for i, address := range blocked {
		addresses[fmt.Sprintf("blocked%d.example.test", i)] = []netip.Addr{netip.MustParseAddr(address)}
	}
	h, forward, dialed := directHarness(t, exit, addresses)
	for name := range addresses {
		h.accounts.set.Load().directHosts[name+":443"] = true
	}
	for i, address := range blocked {
		got := directConnect(t, h, forward, exit, dialed, fmt.Sprintf("blocked%d.example.test:443", i), 502, false)
		if len(got.dialed) != 0 || got.reachedExit || got.route != "direct" || got.result != "egress_failed" {
			t.Fatalf("%s: %+v, want no connection", address, got)
		}
	}
	got := directConnect(t, h, forward, exit, dialed, "github.com:443", 200, true)
	if len(got.dialed) != 1 || got.dialed[0] != "192.0.2.10:443" || got.reachedExit {
		t.Fatalf("control arm: %+v, want only the public address", got)
	}
}

// TestAccountRouteRefusesNonPublicLiterals checks that an unlisted CONNECT target
// whose host is a non-public IP literal, or a number some resolvers read as one, is
// refused with 403 before the exit is dialed, and its record says refused. IPv6
// literals never pass the target syntax and are refused with 400; accountTarget is
// checked on them directly. Control arm: a public IP literal and a host name still
// go through the exit.
func TestAccountRouteRefusesNonPublicLiterals(t *testing.T) {
	exit := startFakeExit(t, 200)
	h, forward, dialed := directHarness(t, exit, nil)
	var records []connectRecord
	forward.sink = func(record connectRecord) error {
		records = append(records, record)
		return nil
	}
	server := httptest.NewServer(forward)
	defer server.Close()
	for _, test := range []struct {
		target string
		status int
	}{
		{"127.0.0.1:443", 403}, {"10.1.2.3:443", 403}, {"172.16.0.1:443", 403}, {"192.168.1.1:443", 403},
		{tailnetAddr + ":443", 403}, {"169.254.169.254:80", 403}, {"0.0.0.0:443", 403}, {"224.0.0.1:443", 403},
		{"255.255.255.255:443", 403}, {"127.1:443", 403}, {"0x7f.0.0.1:443", 403}, {"2130706433:443", 403},
		{"[::1]:443", 400}, {"[fd00::1]:443", 400},
	} {
		*dialed, records = nil, nil
		if _, _, response := connect(t, server.Listener.Addr().String(), test.target, proxyAuthorization(h.token)); response.StatusCode != test.status {
			t.Fatalf("%s: status %d, want %d", test.target, response.StatusCode, test.status)
		}
		if err := forward.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(*dialed) != 0 || len(records) != 1 || records[0].Result != "refused" {
			t.Fatalf("%s: dialed %q, records %+v", test.target, *dialed, records)
		}
	}
	exit.mu.Lock()
	heads := len(exit.heads)
	exit.mu.Unlock()
	if heads != 0 {
		t.Fatalf("exit received %d CONNECTs, want 0", heads)
	}
	for _, target := range []string{"[::1]:443", "[fd00::1]:443", "[fe80::1]:443", "[ff02::1]:443", "[::]:443", "[::ffff:127.0.0.1]:443"} {
		if accountTarget(target) {
			t.Errorf("%s allowed", target)
		}
	}
	for _, target := range []string{"203.0.113.3:443", "api.example.test:443"} {
		got := directConnect(t, h, forward, exit, dialed, target, 200, true)
		if len(got.dialed) != 1 || got.dialed[0] != exit.listener.Addr().String() || !got.reachedExit || got.route != "account" || got.result != "ok" {
			t.Fatalf("control arm %s: %+v", target, got)
		}
	}
}

// directHarness starts a forward proxy for one account behind exit with github.com:443
// and down.example.test:443 listed. Names resolve through addresses; 192.0.2.10:443
// reaches an echoing origin, any other public address a closed port. dialed collects
// every address the proxy dials.
func directHarness(t *testing.T, exit *fakeExit, addresses map[string][]netip.Addr) (*harness, *forwardProxy, *[]string) {
	origin := startFakeOrigin(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := closed.Addr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.addWithExit("sample-test", "synthetic-token-value", &url.URL{Scheme: "http", Host: exit.listener.Addr().String()})
	h.token, h.sessionID = h.issue()
	h.accounts.set.Load().directHosts = map[string]bool{"github.com:443": true, "down.example.test:443": true}
	forward := newForwardProxy(h.store, h.accounts, nil)
	forward.resolve = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		return addresses[strings.ToLower(strings.TrimSuffix(host, "."))], nil
	}
	dialed := &[]string{}
	dial := forward.dial
	forward.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
		*dialed = append(*dialed, target)
		switch target {
		case "192.0.2.10:443":
			target = origin
		case exit.listener.Addr().String():
		default:
			target = down
		}
		return dial(ctx, network, target)
	}
	return h, forward, dialed
}

// directOutcome is what one CONNECT through directHarness did.
type directOutcome struct {
	dialed               []string
	reachedExit          bool
	route, result, alias string
}

// directConnect sends one CONNECT to target, checks the status and, when echo is set,
// that the tunnel carries bytes both ways, and returns what it did.
func directConnect(t *testing.T, h *harness, forward *forwardProxy, exit *fakeExit, dialed *[]string, target string, status int, echo bool) directOutcome {
	t.Helper()
	server := httptest.NewServer(forward)
	defer server.Close()
	*dialed = nil
	exit.mu.Lock()
	before := len(exit.heads)
	exit.mu.Unlock()
	conn, reader, response := connect(t, server.Listener.Addr().String(), target, proxyAuthorization(h.token))
	if response.StatusCode != status {
		t.Fatalf("%s: status %d, want %d", target, response.StatusCode, status)
	}
	if echo {
		if _, err := io.WriteString(conn, "ping"); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(reader, buffer); err != nil || string(buffer) != "ping" {
			t.Fatalf("%s: tunnel did not carry bytes both ways", target)
		}
	}
	closeQuietly(conn)
	if err := forward.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := directOutcome{dialed: *dialed}
	exit.mu.Lock()
	out.reachedExit = len(exit.heads) > before
	exit.mu.Unlock()
	if err := h.openFile().QueryRow("SELECT route, result, alias FROM traffic ORDER BY id DESC LIMIT 1").Scan(&out.route, &out.result, &out.alias); err != nil {
		t.Fatal(err)
	}
	return out
}

// startFakeOrigin starts a TCP server that echoes every byte back, the far end of a
// direct tunnel, and returns its address.
func startFakeOrigin(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer closeQuietly(conn)
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() { closeQuietly(listener) })
	return listener.Addr().String()
}

// TestReloadDirectHosts checks that a reload replaces the list with the inventory's:
// a target becomes direct when the file lists it, goes back to the account when the
// file is removed, and a list that does not load leaves the previous one in place.
// Control arm: before the first reload the target is not direct.
func TestReloadDirectHosts(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	path := filepath.Join(h.dir, directHostsFile)
	if h.accounts.set.Load().direct("github.com:443") {
		t.Fatal("control arm: github.com direct before the list exists")
	}
	if err := os.WriteFile(path, []byte(directYAML("github.com:443")), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := h.reload(nil)
	if err != nil || !slices.Equal(r.Direct, []string{"github.com:443"}) || !h.accounts.set.Load().direct("github.com:443") {
		t.Fatalf("reload with the list: %+v %v", r, err)
	}
	if err := os.WriteFile(path, []byte(directYAML("203.0.113.3:443")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reload(nil); err == nil || !h.accounts.set.Load().direct("github.com:443") {
		t.Fatalf("a refused list replaced the previous one: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r, err := h.reload(nil); err != nil || r.Direct != nil || h.accounts.set.Load().direct("github.com:443") {
		t.Fatalf("reload without the list: %+v %v", r, err)
	}
}
