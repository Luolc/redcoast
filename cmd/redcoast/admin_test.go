package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminCommandParsing checks which management invocations are accepted and the
// request each one sends.
func TestAdminCommandParsing(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		method    string // empty when refused
		path      string
		body      string // JSON, or empty for none
	}{
		{"reload", []string{"reload", "--admin-socket", "admin.sock"}, "POST", "/reload", ""},
		{"status", []string{"status", "--admin-socket", "admin.sock"}, "GET", "/status", ""},
		{"pause_default", []string{"pause", "sample-a", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/pause", `{"reason":"manual"}`},
		{"pause_until", []string{"pause", "sample-a", "--admin-socket", "admin.sock", "--reason", "egress", "--until", "2026-10-07T00:00:00Z"}, "POST", "/accounts/sample-a/pause", `{"reason":"egress","until":"2026-10-07T00:00:00Z"}`},
		{"resume", []string{"resume", "sample-a", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/resume", ""},
		{"refresh", []string{"refresh", "sample-a", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/refresh", ""},
		{"refresh_without_alias", []string{"refresh", "--admin-socket", "admin.sock"}, "", "", ""},
		{"import", []string{"import", "sample-a", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/import", ""},
		{"import_without_alias", []string{"import", "--admin-socket", "admin.sock"}, "", "", ""},
		{"refresh_all", []string{"refresh", "--all", "--admin-socket", "admin.sock"}, "POST", "/refresh", ""},
		{"refresh_all_with_alias", []string{"refresh", "sample-a", "--all", "--admin-socket", "admin.sock"}, "", "", ""},
		{"all_outside_refresh", []string{"status", "--all", "--admin-socket", "admin.sock"}, "", "", ""},
		{"no_socket", []string{"reload"}, "", "", ""},
		{"reload_with_alias", []string{"reload", "sample-a", "--admin-socket", "admin.sock"}, "", "", ""},
		{"pause_without_alias", []string{"pause", "--admin-socket", "admin.sock"}, "", "", ""},
		{"pause_bad_alias", []string{"pause", "Sample A", "--admin-socket", "admin.sock"}, "", "", ""},
		{"pause_bad_until", []string{"pause", "sample-a", "--admin-socket", "admin.sock", "--until", "tomorrow"}, "", "", ""},
		{"resume_with_until", []string{"resume", "sample-a", "--admin-socket", "admin.sock", "--until", "2026-10-07T00:00:00Z"}, "", "", ""},
		{"sessions", []string{"sessions", "--admin-socket", "admin.sock"}, "GET", "/sessions", ""},
		{"sessions_filtered", []string{"sessions", "--admin-socket", "admin.sock", "--cwd-prefix", "/srv/wt/repo a&b", "--machine", "client-a", "--since", "2026-10-06T00:00:00Z", "--until", "2026-10-07T00:00:00Z", "--state", "all"}, "GET", "/sessions?cwd_prefix=%2Fsrv%2Fwt%2Frepo+a%26b&machine=client-a&since=2026-10-06T00%3A00%3A00Z&state=all&until=2026-10-07T00%3A00%3A00Z", ""},
		{"sessions_with_argument", []string{"sessions", "client-a", "--admin-socket", "admin.sock"}, "", "", ""},
		{"cwd_prefix_outside_sessions", []string{"status", "--admin-socket", "admin.sock", "--cwd-prefix", "/srv"}, "", "", ""},
		{"machine_list", []string{"machine", "list", "--admin-socket", "admin.sock"}, "GET", "/machines", ""},
		{"machine_add", []string{"machine", "add", "client-a", "abababababababababababababababababababababababababababababababab", "--admin-socket", "admin.sock"}, "POST", "/machines", `{"credential_hash":"abababababababababababababababababababababababababababababababab","max_sessions":0,"name":"client-a"}`},
		{"machine_add_limit", []string{"machine", "add", "client-a", "abababababababababababababababababababababababababababababababab", "--admin-socket", "admin.sock", "--max-sessions", "20"}, "POST", "/machines", `{"credential_hash":"abababababababababababababababababababababababababababababababab","max_sessions":20,"name":"client-a"}`},
		{"machine_revoke", []string{"machine", "revoke", "client-a", "--admin-socket", "admin.sock"}, "DELETE", "/machines/client-a", ""},
		{"machine_limit", []string{"machine", "limit", "client-a", "12", "--admin-socket", "admin.sock"}, "POST", "/machines/client-a/limit", `{"max_sessions":12}`},
		{"machine_add_without_hash", []string{"machine", "add", "client-a", "--admin-socket", "admin.sock"}, "", "", ""},
		{"machine_limit_zero", []string{"machine", "limit", "client-a", "0", "--admin-socket", "admin.sock"}, "", "", ""},
		{"machine_unknown_verb", []string{"machine", "drop", "client-a", "--admin-socket", "admin.sock"}, "", "", ""},
		{"max_sessions_outside_machine", []string{"reload", "--admin-socket", "admin.sock", "--max-sessions", "2"}, "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := parseAdminCommand(test.arguments)
			if test.method == "" {
				if err == nil {
					t.Fatalf("accepted: %+v", cmd)
				}
				return
			}
			if err != nil || cmd.method != test.method || cmd.path != test.path || cmd.socket != "admin.sock" {
				t.Fatalf("got %+v %v", cmd, err)
			}
			var body string
			if cmd.body != nil {
				encoded, _ := json.Marshal(cmd.body)
				body = string(encoded)
			}
			if body != test.body {
				t.Fatalf("body %s, want %s", body, test.body)
			}
		})
	}
}

// TestRunAdmin checks the client against a fake management socket: a success is
// written out indented, a refusal becomes an error carrying the gateway's message, and
// an import sends its input as it is, refusing one over 64 KiB without sending it.
func TestRunAdmin(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var received []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = append(received, r.Method+" "+r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/reload" {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"error":"sample-a: oauth_token: cannot resolve"}`))
			return
		}
		_, _ = w.Write([]byte(`{"alias":"sample-a","reason":"manual"}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	var out bytes.Buffer
	cmd, err := parseAdminCommand([]string{"pause", "sample-a", "--admin-socket", socket})
	if err != nil {
		t.Fatal(err)
	}
	if err := runAdmin(t.Context(), cmd, &out); err != nil || out.String() != "{\n  \"alias\": \"sample-a\",\n  \"reason\": \"manual\"\n}\n" {
		t.Fatalf("pause: %v output %q", err, out.String())
	}
	cmd, err = parseAdminCommand([]string{"reload", "--admin-socket", socket})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runAdmin(t.Context(), cmd, &out); err == nil || !strings.Contains(err.Error(), "reload refused: sample-a: oauth_token: cannot resolve") || out.Len() != 0 {
		t.Fatalf("refused reload: %v output %q", err, out.String())
	}
	cmd, err = parseAdminCommand([]string{"import", "sample-a", "--admin-socket", socket})
	if err != nil {
		t.Fatal(err)
	}
	login := `{"tokens": {"access_token": "synthetic-access-token"}}`
	cmd.input = strings.NewReader(login)
	out.Reset()
	if err := runAdmin(t.Context(), cmd, &out); err != nil || strings.Contains(out.String(), "synthetic-access-token") {
		t.Fatalf("import: %v output %q", err, out.String())
	}
	cmd.input = strings.NewReader(strings.Repeat(" ", maxImportInput+1))
	if err := runAdmin(t.Context(), cmd, &out); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("oversize import: %v", err)
	}
	if len(received) != 3 || received[0] != `POST /accounts/sample-a/pause {"reason":"manual"}` || received[1] != "POST /reload " || received[2] != "POST /accounts/sample-a/import "+login {
		t.Fatalf("socket received %q", received)
	}
	cmd.socket = filepath.Join(t.TempDir(), "missing.sock")
	if err := runAdmin(t.Context(), cmd, &out); err == nil || !strings.Contains(err.Error(), "management socket") {
		t.Fatalf("missing socket: %v", err)
	}
}

// TestRefreshAllCommand checks refresh --all against a fake management socket: one line
// per account, an error naming the accounts whose refresh failed, and none when the
// only accounts not refreshed were skipped.
func TestRefreshAllCommand(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	answer := `[{"alias":"sample-a","upstream_status":200,"quota":{},"pause_ended":false},{"alias":"sample-b","skipped":"account paused: upstream_401"},{"alias":"sample-c","error":"upstream not reached through the account's exit"}]`
	var received []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer+"\n")
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	cmd, err := parseAdminCommand([]string{"refresh", "--all", "--admin-socket", socket})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	want := `{"alias":"sample-a","upstream_status":200,"quota":{},"pause_ended":false}
{"alias":"sample-b","skipped":"account paused: upstream_401"}
{"alias":"sample-c","error":"upstream not reached through the account's exit"}
`
	if err := runAdmin(t.Context(), cmd, &out); err == nil || err.Error() != "refresh failed for sample-c" || out.String() != want {
		t.Fatalf("with a failure: %v output %q", err, out.String())
	}
	answer = `[{"alias":"sample-a","upstream_status":200,"quota":{},"pause_ended":false},{"alias":"sample-b","skipped":"account paused: upstream_401"}]`
	out.Reset()
	if err := runAdmin(t.Context(), cmd, &out); err != nil || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("with a skip only: %v output %q", err, out.String())
	}
	if len(received) != 2 || received[0] != "POST /refresh" {
		t.Fatalf("socket received %q", received)
	}
}
