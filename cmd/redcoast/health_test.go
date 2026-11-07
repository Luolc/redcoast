package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/internal/testtls"
)

// TestHealthAndEgressConfiguration checks the health endpoint's and the dashboard's
// listen addresses, one IP and port each, never a wildcard and never the same, and the
// lower bound of the egress interval.
func TestHealthAndEgressConfiguration(t *testing.T) {
	// The first address of 100.64.0.0/10, a synthetic tailnet address.
	shared := "100.64.0.1:7804"
	for _, test := range []struct {
		name     string
		yaml     string
		health   string // expected when accepted; empty when refused
		interval time.Duration
	}{
		{"defaults", "", "127.0.0.1:7804", 15 * time.Minute},
		{"tailnet_address", "listen: {health: '" + shared + "'}\negress_interval: 1m\n", shared, time.Minute},
		{"ipv4_wildcard", "listen: {health: '0.0.0.0:7804'}\n", "", 0},
		{"ipv6_wildcard", "listen: {health: '[::]:7804'}\n", "", 0},
		{"no_host", "listen: {health: ':7804'}\n", "", 0},
		{"host_name", "listen: {health: 'gateway.example.test:7804'}\n", "", 0},
		{"on_reverse_address", "listen: {health: '127.0.0.1:8789'}\n", "", 0},
		{"interval_below_a_minute", "egress_interval: 59s\n", "", 0},
		{"dashboard_on_health_address", "listen: {dashboard: '127.0.0.1:7804'}\n", "", 0},
		{"dashboard_wildcard", "listen: {dashboard: '0.0.0.0:7805'}\n", "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig("claude", []byte(baseConfig+test.yaml))
			if test.health == "" {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil || cfg.healthListen != test.health || cfg.egressInterval != test.interval || cfg.dashboardListen != "127.0.0.1:7805" {
				t.Fatalf("configuration refused or changed: %+v %v", cfg, err)
			}
		})
	}
}

// TestReadOnlyHosts checks which Host names /health and the dashboard answer, through
// a real HTTP server: the IPs of their listen addresses, the TLS server name and
// listen.host_names, and no other name (DNS rebinding) or listener's IP, whether or
// not the request accepts gzip. Entries of listen.host_names that are not lower-case
// DNS names without a port are refused.
func TestReadOnlyHosts(t *testing.T) {
	cert, key := writeTLSFiles(t, testtls.New(t))
	tailnet := "listen: {health: '100.64.0.1:7804', dashboard: '[fd00::7]:7805', reverse: '10.0.0.2:7802', host_names: [gateway-a]}\n"
	withTLS := "listen:\n  tls: {cert_file: '" + cert + "', key_file: '" + key + "', server_name: '" + testtls.ServerName + "'}\n"
	for _, arm := range []struct {
		name, yaml string
		allowed    []string
		refused    []string
	}{
		{"defaults", "", []string{"127.0.0.1:7804", "127.0.0.1:7805"}, []string{"gateway-a:7804", "localhost:7804"}},
		{"tailnet", tailnet, []string{"100.64.0.1:7804", "[fd00::7]:7805", "gateway-a:7805"}, []string{"10.0.0.2:7804", "127.0.0.1:7804", "rebind.example.test:7805"}},
		{"tls", withTLS, []string{testtls.ServerName + ":7804", "127.0.0.1:7805"}, []string{"gateway-a:7804"}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			cfg, err := parseConfig("claude", []byte(baseConfig+arm.yaml))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(cfg.hostGuarded(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("guarded"))
			})))
			defer server.Close()
			for _, host := range append(arm.allowed, arm.refused...) {
				for _, encoding := range []string{"", "gzip"} {
					request, err := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
					if err != nil {
						t.Fatal(err)
					}
					request.Host = host
					if encoding != "" {
						request.Header.Set("Accept-Encoding", encoding)
					}
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					answered := response.StatusCode == http.StatusOK && string(body) == "guarded"
					if want := !slices.Contains(arm.refused, host); answered != want || !want && response.StatusCode != http.StatusForbidden {
						t.Errorf("Host %s, Accept-Encoding %q: status %d, body %q; want answered %v", host, encoding, response.StatusCode, body, want)
					}
				}
			}
		})
	}
	for _, name := range []string{"Gateway-A", "gateway-a:7805", "*.example.test", "", "-gateway"} {
		_, err := parseConfig("claude", []byte(baseConfig+"listen: {host_names: ['"+name+"']}\n"))
		if err == nil || !strings.Contains(err.Error(), "config: listen.host_names: ") {
			t.Errorf("host name %q: %v; want refused, naming listen.host_names", name, err)
		}
	}
}

// TestNotifyReady checks that the gateway sends READY=1 to the socket NOTIFY_SOCKET
// names, at a path and in the abstract namespace; the control arm without the variable
// sends nothing to the same socket.
func TestNotifyReady(t *testing.T) {
	for _, arm := range []string{"path", "abstract", "unset"} {
		t.Run(arm, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "notify.sock")
			variable := name
			if arm == "abstract" {
				variable = "@" + name
				name = "\x00" + name
			}
			listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			if arm == "unset" {
				variable = ""
			}
			t.Setenv("NOTIFY_SOCKET", variable)
			if err := notifyReady(); err != nil {
				t.Fatal(err)
			}
			if err := listener.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, 64)
			n, err := listener.Read(buffer)
			if arm == "unset" {
				if err == nil {
					t.Fatalf("received %q without NOTIFY_SOCKET", buffer[:n])
				}
				return
			}
			if want := fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid()); err != nil || string(buffer[:n]) != want {
				t.Fatalf("received %q, %v; want %q", buffer[:n], err, want)
			}
		})
	}
}

// TestDashboardLinksConfiguration checks dashboard_links: each entry names the other
// provider once, with an http or https URL that has a host and no user name or
// password, or ":<port>"; no key gives no links. A refused URL is not repeated in the
// error.
func TestDashboardLinksConfiguration(t *testing.T) {
	const section = "codex: {accounts_dir: /var/lib/redcoast-codex/accounts, session_key: 'env://SESSION_KEY'}\n"
	link := func(provider, url string) string {
		return "dashboard_links:\n  - provider: " + provider + "\n    url: '" + url + "'\n"
	}
	for _, test := range []struct {
		name, provider, yaml string
		want                 []claude.DashboardLink // expected when accepted
		problem              string                 // part of the error when refused
	}{
		{"none", "claude", "", nil, ""},
		{"empty", "claude", "dashboard_links: []\n", nil, ""},
		{"http", "claude", link("codex", "http://127.0.0.1:7807/"), []claude.DashboardLink{{Provider: "codex", URL: "http://127.0.0.1:7807/"}}, ""},
		{"https_with_path", "codex", link("claude", "https://gateway.example.test/claude/"), []claude.DashboardLink{{Provider: "claude", URL: "https://gateway.example.test/claude/"}}, ""},
		{"port_only", "claude", link("codex", ":7807"), []claude.DashboardLink{{Provider: "codex", URL: ":7807"}}, ""},
		{"highest_port", "claude", link("codex", ":65535"), []claude.DashboardLink{{Provider: "codex", URL: ":65535"}}, ""},
		{"own_provider", "claude", link("claude", ":7805"), nil, "dashboard_links[0].provider: must be another provider"},
		{"unknown_provider", "claude", link("gemini", ":7809"), nil, "dashboard_links[0].provider: must be claude or codex"},
		{"twice", "claude", link("codex", ":7807") + "  - {provider: codex, url: ':7808'}\n", nil, "dashboard_links[1].provider: listed twice"},
		{"userinfo", "claude", link("codex", "http://operator:example-password@127.0.0.1:7807/"), nil, "dashboard_links[0].url: must not carry a user name or password"},
		{"user_only", "claude", link("codex", "http://operator@127.0.0.1:7807/"), nil, "dashboard_links[0].url: must not carry a user name or password"},
		{"javascript", "claude", link("codex", "javascript:alert(1)"), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"ftp", "claude", link("codex", "ftp://192.0.2.10/"), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"no_host", "claude", link("codex", "http:///dashboard"), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"relative_path", "claude", link("codex", "/codex/"), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"protocol_relative", "claude", link("codex", "//192.0.2.10:7807/"), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"empty_url", "claude", link("codex", ""), nil, "dashboard_links[0].url: must be an http or https URL"},
		{"url_port_too_high", "claude", link("codex", "http://127.0.0.1:99999/"), nil, "dashboard_links[0].url: needs a port between 1 and 65535"},
		{"url_port_zero", "claude", link("codex", "http://127.0.0.1:0/"), nil, "dashboard_links[0].url: needs a port between 1 and 65535"},
		{"port_zero", "claude", link("codex", ":0"), nil, "dashboard_links[0].url: :<port> needs a port"},
		{"port_too_high", "claude", link("codex", ":65536"), nil, "dashboard_links[0].url: :<port> needs a port"},
		{"port_leading_zero", "claude", link("codex", ":07807"), nil, "dashboard_links[0].url: :<port> needs a port"},
		{"port_with_path", "claude", link("codex", ":7807/x"), nil, "dashboard_links[0].url: :<port> needs a port"},
		{"unknown_key", "claude", "dashboard_links:\n  - {provider: codex, url: ':7807', label: Codex}\n", nil, "field label not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			yaml := baseConfig + test.yaml
			if test.provider == "codex" {
				yaml += section
			}
			cfg, err := parseConfig(test.provider, []byte(yaml))
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want %q", err, test.problem)
				}
				if strings.Contains(err.Error(), "example-password") {
					t.Fatalf("the error repeats the URL: %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(cfg.dashboardLinks, test.want) {
				t.Fatalf("got %+v %v, want %+v", cfg.dashboardLinks, err, test.want)
			}
		})
	}
}

// TestDashboardWriteConfiguration checks dashboard_write: no key writes from nowhere;
// allow_from takes IP addresses (an address is a prefix of its own) and prefixes of at
// least one bit, nothing else; the token reference template holds {alias} once and is a
// credential reference with an alias in it; the Codex gateway refuses the section.
func TestDashboardWriteConfiguration(t *testing.T) {
	const section = "codex: {accounts_dir: /var/lib/redcoast-codex/accounts, session_key: 'env://SESSION_KEY'}\n"
	for _, test := range []struct {
		name, provider, yaml string
		want                 []string // the prefixes, when accepted
		template             string
		problem              string // part of the error when refused
	}{
		{"none", "claude", "", nil, "", ""},
		{"empty_list", "claude", "dashboard_write: {allow_from: []}\n", nil, "", ""},
		{"addresses_and_prefixes", "claude", "dashboard_write: {allow_from: ['192.0.2.7', '198.51.100.0/24', '2001:db8::/32', '2001:db8::1']}\n",
			[]string{"192.0.2.7/32", "198.51.100.0/24", "2001:db8::/32", "2001:db8::1/128"}, "", ""},
		{"prefix_is_masked", "claude", "dashboard_write: {allow_from: ['192.0.2.77/24']}\n", []string{"192.0.2.0/24"}, "", ""},
		{"mapped_address", "claude", "dashboard_write: {allow_from: ['::ffff:192.0.2.7']}\n", []string{"192.0.2.7/32"}, "", ""},
		{"one_bit", "claude", "dashboard_write: {allow_from: ['128.0.0.0/1']}\n", []string{"128.0.0.0/1"}, "", ""},
		{"zero_bits_v4", "claude", "dashboard_write: {allow_from: ['0.0.0.0/0']}\n", nil, "", "dashboard_write.allow_from[0]: a prefix of no bit"},
		{"zero_bits_v6", "claude", "dashboard_write: {allow_from: ['192.0.2.7', '::/0']}\n", nil, "", "dashboard_write.allow_from[1]: a prefix of no bit"},
		{"not_an_address", "claude", "dashboard_write: {allow_from: ['example.test']}\n", nil, "", "dashboard_write.allow_from[0]: not an IP address or a prefix"},
		{"zone", "claude", "dashboard_write: {allow_from: ['fe80::1%eth0']}\n", nil, "", "dashboard_write.allow_from[0]: not an IP address or a prefix"},
		{"unknown_key", "claude", "dashboard_write: {allow_from: [], trusted_proxy: true}\n", nil, "", "field trusted_proxy not found"},
		{"template", "claude", "dashboard_write: {token_ref_template: 'op://example-vault/claude-oauth-token-{alias}/credential'}\n", nil, "op://example-vault/claude-oauth-token-{alias}/credential", ""},
		{"template_without_alias", "claude", "dashboard_write: {token_ref_template: 'op://example-vault/token/credential'}\n", nil, "", "must contain {alias} once"},
		{"template_with_alias_twice", "claude", "dashboard_write: {token_ref_template: 'op://example-vault/{alias}/{alias}'}\n", nil, "", "must contain {alias} once"},
		{"template_not_a_reference", "claude", "dashboard_write: {token_ref_template: 'sk-ant-{alias}'}\n", nil, "", "must give a credential reference"},
		{"codex_allow_from", "codex", "dashboard_write: {allow_from: ['192.0.2.7']}\n", nil, "", "dashboard_write: only the Claude gateway takes it"},
		{"codex_template", "codex", "dashboard_write: {token_ref_template: 'op://example-vault/{alias}/x'}\n", nil, "", "dashboard_write: only the Claude gateway takes it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			yaml := baseConfig + test.yaml
			if test.provider == "codex" {
				yaml += section
			}
			cfg, err := parseConfig(test.provider, []byte(yaml))
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want %q", err, test.problem)
				}
				return
			}
			var got []string
			for _, prefix := range cfg.writeAllowFrom {
				got = append(got, prefix.String())
			}
			if err != nil || !slices.Equal(got, test.want) || cfg.tokenRefTemplate != test.template {
				t.Fatalf("got %v %q %v, want %v %q", got, cfg.tokenRefTemplate, err, test.want, test.template)
			}
		})
	}
}

// TestSetupConfiguration checks setup.enabled: off by default and when the section is
// absent; on only for the Claude gateway with a 1Password service account token to write
// with and a token reference template to say where; the Codex gateway refuses the key.
func TestSetupConfiguration(t *testing.T) {
	const section = "codex: {accounts_dir: /var/lib/redcoast-codex/accounts, session_key: 'env://SESSION_KEY'}\n"
	const credentials = "credentials: {onepassword_token: 'env://OP_TOKEN'}\n"
	const template = "dashboard_write: {token_ref_template: 'op://example-vault/token-{alias}/credential'}\n"
	for _, test := range []struct {
		name, provider, yaml string
		want                 bool
		problem              string
	}{
		{"absent", "claude", credentials, false, ""},
		{"off", "claude", credentials + "setup: {enabled: false}\n", false, ""},
		{"on", "claude", credentials + template + "setup: {enabled: true}\n", true, ""},
		{"on_without_token", "claude", template + "credentials: {}\nsetup: {enabled: true}\n", false, "setup.enabled: needs credentials.onepassword_token"},
		{"on_with_the_stdin_entry", "claude", template + "credentials: {stdin_test_entry: true}\nsetup: {enabled: true}\n", false, "setup.enabled: needs credentials.onepassword_token"},
		{"on_without_template", "claude", credentials + "setup: {enabled: true}\n", false, "setup.enabled: needs dashboard_write.token_ref_template"},
		{"unknown_key", "claude", credentials + "setup: {enabled: true, login_machine: x}\n", false, "field login_machine not found"},
		{"codex_off", "codex", "setup: {enabled: false}\n", false, "setup: only the Claude gateway takes it"},
		{"codex_on", "codex", "setup: {enabled: true}\n", false, "setup: only the Claude gateway takes it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			yaml := basePaths + test.yaml
			if test.provider == "codex" {
				yaml += section
			}
			cfg, err := parseConfig(test.provider, []byte(yaml))
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want %q", err, test.problem)
				}
				return
			}
			if err != nil || cfg.setupEnabled != test.want {
				t.Fatalf("got %v %v, want %v", cfg.setupEnabled, err, test.want)
			}
		})
	}
}

// TestAdminWriteConfiguration checks admin_write.enabled: off by default and when the
// section is absent; on only for the Claude gateway with the token reference template,
// since an account it creates has the reference that gives; the Codex gateway refuses
// the key.
func TestAdminWriteConfiguration(t *testing.T) {
	const section = "codex: {accounts_dir: /var/lib/redcoast-codex/accounts, session_key: 'env://SESSION_KEY'}\n"
	const template = "dashboard_write: {token_ref_template: 'op://example-vault/token-{alias}/credential'}\n"
	for _, test := range []struct {
		name, provider, yaml string
		want                 bool
		problem              string
	}{
		{"absent", "claude", "", false, ""},
		{"off", "claude", "admin_write: {enabled: false}\n", false, ""},
		{"on", "claude", template + "admin_write: {enabled: true}\n", true, ""},
		{"on_without_template", "claude", "admin_write: {enabled: true}\n", false, "admin_write.enabled: needs dashboard_write.token_ref_template"},
		{"unknown_key", "claude", template + "admin_write: {enabled: true, who: x}\n", false, "field who not found"},
		{"codex_off", "codex", "admin_write: {enabled: false}\n", false, "admin_write: only the Claude gateway takes it"},
		{"codex_on", "codex", "admin_write: {enabled: true}\n", false, "admin_write: only the Claude gateway takes it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			yaml := basePaths + test.yaml
			if test.provider == "codex" {
				yaml += section
			}
			cfg, err := parseConfig(test.provider, []byte(yaml))
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want %q", err, test.problem)
				}
				return
			}
			if err != nil || cfg.adminWrite != test.want {
				t.Fatalf("got %v %v, want %v", cfg.adminWrite, err, test.want)
			}
		})
	}
}
