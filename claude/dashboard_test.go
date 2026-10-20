package claude

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// inventoryTree builds an inventory link the way the deploy scripts lay it out and
// returns the link's path.
func inventoryTree(t *testing.T, inventoryCommit string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inventories", inventoryCommit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("inventories", inventoryCommit), filepath.Join(root, "inventory")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "inventory")
}

// TestDashboardJSON reads the dashboard over a store with one matching and one
// mismatched exit: the overview, each account's process state and the store's
// tables, with no credential in the answer.
func TestDashboardJSON(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1", "sample-b": "192.0.2.2"})
	if err := e.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	a := h.accounts.set.Load().lookup("sample-a")
	a.email = "a@example.test"
	a.slots <- struct{}{}
	if _, err := h.store.Bind(t.Context(), h.token, h.accounts.Candidates()); err != nil {
		t.Fatal(err)
	}
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	inventoryCommit := strings.Repeat("b", 40)
	d := NewDashboard(e, DashboardConfig{Provider: "claude", Reader: reader, Store: h.store, DBPath: h.path, Version: "1.2.3", Inventory: inventoryTree(t, inventoryCommit),
		Started: h.clock.Add(-time.Hour), Tunnels: func() (int, int) { return 3, 256 }})

	recorder := httptest.NewRecorder()
	d.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dashboard.json", nil))
	body := recorder.Body.String()
	// The top-level keys are the frontend's contract.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, []string{"accounts", "at", "days", "db_bytes", "destinations", "gateway", "hard", "health", "history", "hourly", "idle_sessions", "machines", "results", "sessions", "soft"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	var view struct {
		Health   health
		Gateway  gatewayView
		Accounts []struct {
			Alias       string
			PauseReason string `json:"pause_reason"`
			Email       string
			Egress      EgressResult `json:"egress_check"`
			InFlight    int          `json:"in_flight"`
			Concurrency int
		}
		Sessions []session.LiveSession
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &view) != nil {
		t.Fatalf("dashboard answered %d: %s", recorder.Code, body)
	}
	if view.Health.Status != "degraded" || !slices.Equal(view.Health.Reasons, []string{"sample-b: paused (egress_ip_mismatch)"}) {
		t.Fatalf("health = %+v", view.Health)
	}
	g := view.Gateway
	if g.Provider != "claude" || g.Dashboards == nil || len(g.Dashboards) != 0 || g.Version != "1.2.3" || g.InventoryCommit != inventoryCommit || g.OpenTunnels != 3 || g.TunnelLimit != 256 || g.Backup != nil {
		t.Fatalf("gateway = %+v", g)
	}
	if len(view.Accounts) != 2 {
		t.Fatalf("accounts = %+v", view.Accounts)
	}
	first, second := view.Accounts[0], view.Accounts[1]
	if first.Alias != "sample-a" || first.Email != "a@example.test" || first.Egress.Outcome != "ok" || first.InFlight != 1 || first.Concurrency != DefaultAccountConcurrency {
		t.Fatalf("sample-a = %+v", first)
	}
	if second.Alias != "sample-b" || second.PauseReason != EgressMismatchReason || second.Egress.Outcome != "mismatch" || second.InFlight != 0 {
		t.Fatalf("sample-b = %+v", second)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].ID != h.sessionID[:8] || view.Sessions[0].Alias != "sample-a" {
		t.Fatalf("sessions = %+v", view.Sessions)
	}
	for _, secret := range []string{h.token, h.sessionID, "sk-ant-oat01-", "Synthetic-Password-", "synthetic-user-"} {
		if strings.Contains(body, secret) {
			t.Fatalf("dashboard shows %q: %s", secret, body)
		}
	}
}

// TestDashboardLinks reads the other providers' dashboards from the JSON: an absolute
// URL as configured, and ":<port>" as the scheme and Host name the request came with at
// that port, for an IP, a name, an IPv6 literal and a Host without a port. With no
// links the list is empty, not null, so the page shows only its own provider.
func TestDashboardLinks(t *testing.T) {
	h := newHarness(t, "sample-a")
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	read := func(t *testing.T, links []DashboardLink, host string, overTLS bool) json.RawMessage {
		t.Helper()
		handler := NewDashboard(NewEgress(h.accounts, h.store), DashboardConfig{Provider: "codex", Links: links, Reader: reader, Store: h.store, DBPath: h.path}).Handler()
		request := httptest.NewRequest(http.MethodGet, "/dashboard.json", nil)
		request.Host = host
		if overTLS {
			request.TLS = &tls.ConnectionState{}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var view struct {
			Gateway struct {
				Provider   string
				Dashboards json.RawMessage
			}
		}
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &view) != nil || view.Gateway.Provider != "codex" {
			t.Fatalf("dashboard answered %d: %s", recorder.Code, recorder.Body.String())
		}
		return view.Gateway.Dashboards
	}
	if got := string(read(t, nil, "127.0.0.1:7807", false)); got != "[]" {
		t.Fatalf("no links gave %s", got)
	}
	for _, arm := range []struct {
		name, link, host string
		overTLS          bool
		want             string
	}{
		{"absolute", "https://gateway.example.test:7805/", "127.0.0.1:7807", false, "https://gateway.example.test:7805/"},
		{"absolute_with_path", "http://192.0.2.10/claude/", "127.0.0.1:7807", true, "http://192.0.2.10/claude/"},
		{"port_ip", ":7805", "127.0.0.1:7807", false, "http://127.0.0.1:7805/"},
		{"port_name_tls", ":7805", "gateway.example.test:7807", true, "https://gateway.example.test:7805/"},
		{"port_ipv6", ":7805", "[fd00::7]:7807", false, "http://[fd00::7]:7805/"},
		{"port_host_without_port", ":7805", "gateway.example.test", false, "http://gateway.example.test:7805/"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			var got []DashboardLink
			if err := json.Unmarshal(read(t, []DashboardLink{{Provider: "claude", URL: arm.link}}, arm.host, arm.overTLS), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, []DashboardLink{{Provider: "claude", URL: arm.want}}) {
				t.Fatalf("dashboards = %+v, want %s", got, arm.want)
			}
		})
	}
}

// TestCommitOf reads the commit from an inventory link and nothing from a directory
// whose name is not one, such as an inventory checked out by hand.
func TestCommitOf(t *testing.T) {
	commit := strings.Repeat("c", 40)
	if got := commitOf(inventoryTree(t, commit)); got != commit {
		t.Fatalf("commit %q read from the inventory link", got)
	}
	if got := commitOf(t.TempDir()); got != "" {
		t.Fatalf("commit %q read from a path without one", got)
	}
}

// TestDashboardFrontend serves index.html at / next to the JSON and nothing else: no
// other file of the directory, no directory listing. A missing directory, or one
// without index.html, answers 404 at / and keeps the JSON.
func TestDashboardFrontend(t *testing.T) {
	h := newHarness(t, "sample-a")
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	built, unbuilt := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(built, "index.html"), []byte("<title>Claude 网关</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{built, unbuilt} {
		if err := os.Mkdir(filepath.Join(dir, "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "assets", "index.js"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, arm := range []struct {
		name, dir string
		root      int
	}{{"built", built, http.StatusOK}, {"no_index", unbuilt, http.StatusNotFound}, {"missing", filepath.Join(built, "missing"), http.StatusNotFound}} {
		t.Run(arm.name, func(t *testing.T) {
			handler := NewDashboard(NewEgress(h.accounts, h.store), DashboardConfig{Reader: reader, Store: h.store, DBPath: h.path, Dir: arm.dir}).Handler()
			for path, want := range map[string]int{"/": arm.root, "/dashboard.json": http.StatusOK, "/assets/index.js": http.StatusNotFound, "/assets/": http.StatusNotFound, "/index.html": http.StatusNotFound} {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				if recorder.Code != want {
					t.Fatalf("%s answered %d, want %d", path, recorder.Code, want)
				}
				if path == "/" && want == http.StatusOK && !strings.Contains(recorder.Body.String(), "Claude 网关") {
					t.Fatalf("/ answered %q", recorder.Body.String())
				}
			}
		})
	}
}

// dashboardServer serves the dashboard with its page in dir.
func dashboardServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	h := newHarness(t, "sample-a")
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	server := httptest.NewServer(NewDashboard(NewEgress(h.accounts, h.store), DashboardConfig{Reader: reader, Store: h.store, DBPath: h.path, Dir: dir}).Handler())
	t.Cleanup(server.Close)
	return server
}

// getPage reads / from server and returns the status and the body.
func getPage(t *testing.T, server *httptest.Server) (int, string) {
	t.Helper()
	response, err := server.Client().Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// TestDashboardReadsTheReplacedPage replaces the page while the gateway runs, the way
// the deployment does (a new file renamed over the old one): the next request gets
// the new page, with no restart.
func TestDashboardReadsTheReplacedPage(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index.html")
	if err := os.WriteFile(index, []byte("old page"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := dashboardServer(t, dir)
	if status, body := getPage(t, server); status != http.StatusOK || body != "old page" {
		t.Fatalf("before the replacement / answered %d %q", status, body)
	}
	if err := os.WriteFile(index+".new", []byte("new page"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(index+".new", index); err != nil {
		t.Fatal(err)
	}
	if status, body := getPage(t, server); status != http.StatusOK || body != "new page" {
		t.Fatalf("after the replacement / answered %d %q", status, body)
	}
}

// TestDashboardRefusesALinkOutOfTheDirectory: the deploy account writes the page's
// directory, so index.html may be a symbolic link; one that leads out of the directory
// answers 404 and never the linked file. Control arm: a regular index.html is served.
func TestDashboardRefusesALinkOutOfTheDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "sessions.db")
	const secret = "synthetic-store-content-7f3a"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arm := range []string{"regular", "link_out"} {
		t.Run(arm, func(t *testing.T) {
			dir := t.TempDir()
			index := filepath.Join(dir, "index.html")
			var err error
			if arm == "regular" {
				err = os.WriteFile(index, []byte("page"), 0o644)
			} else {
				err = os.Symlink(outside, index)
			}
			if err != nil {
				t.Fatal(err)
			}
			status, body := getPage(t, dashboardServer(t, dir))
			if strings.Contains(body, secret) {
				t.Fatalf("/ served the file outside the directory: %d %q", status, body)
			}
			want := map[string]int{"regular": http.StatusOK, "link_out": http.StatusNotFound}[arm]
			if status != want || (arm == "regular" && body != "page") {
				t.Fatalf("/ answered %d %q, want %d", status, body, want)
			}
		})
	}
}
