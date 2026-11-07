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

const (
	setupRequestID = "0123456789abcdef0123456789abcdef"
	setupRunID     = "20261107T041325123Z"
	slackThread    = "C0TESTCHAN1/1700000000.000100"
	slackTS        = "1700000000.000200"
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
		{"setup_next", []string{"setup", "next", "--admin-socket", "admin.sock"}, "POST", "/setup/next", ""},
		{"setup_report", []string{"setup", "report", "sample-a", setupRequestID, "--run-id", setupRunID, "--source-kind", "dashboard", "--state", "running", "--step", "email_page", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/setup/report",
			`{"reason":null,"request_id":"` + setupRequestID + `","run_id":"` + setupRunID + `","source_kind":"dashboard","state":"running","step":"email_page"}`},
		{"setup_report_with_reason", []string{"setup", "report", "sample-a", setupRequestID, "--admin-socket", "admin.sock", "--run-id", setupRunID, "--source-kind", "slack", "--state", "failed", "--step", "cli", "--reason", "cli_login_failed"}, "POST", "/accounts/sample-a/setup/report",
			`{"reason":"cli_login_failed","request_id":"` + setupRequestID + `","run_id":"` + setupRunID + `","source_kind":"slack","state":"failed","step":"cli"}`},
		{"setup_report_wait", []string{"setup", "report", "sample-a", setupRequestID, "--run-id", setupRunID, "--source-kind", "slack", "--state", "running", "--step", "awaiting_captcha", "--wait-until", "2026-11-07T04:30:00Z", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/setup/report",
			`{"reason":null,"request_id":"` + setupRequestID + `","run_id":"` + setupRunID + `","source_kind":"slack","state":"running","step":"awaiting_captcha","wait_until":"2026-11-07T04:30:00Z"}`},
		{"setup_report_without_source_kind", []string{"setup", "report", "sample-a", setupRequestID, "--run-id", setupRunID, "--state", "running", "--step", "cli", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token_without_source_kind", []string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token_with_wait_until", []string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--source-kind", "slack", "--wait-until", "2026-11-07T04:30:00Z", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_next_by_id", []string{"setup", "next", "--request-id", setupRequestID, "--admin-socket", "admin.sock"}, "POST", "/setup/next?request_id=" + setupRequestID, ""},
		{"setup_next_with_source_kind", []string{"setup", "next", "--source-kind", "slack", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_request", []string{"setup", "request", "sample-a", "--rev", "3", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/setup/request",
			`{"rev":3,"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"setup_request_without_rev", []string{"setup", "request", "sample-a", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_request_without_source", []string{"setup", "request", "sample-a", "--rev", "3", "--source-thread", slackThread, "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_request_with_run_id", []string{"setup", "request", "sample-a", "--rev", "3", "--run-id", setupRunID, "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_create", append([]string{"account", "create", "sample-new"}, accountCreateFlags...), "POST", "/accounts",
			`{"alias":"sample-new","device_id":"` + testDeviceID + `","email":"new@example.test","plan":"max-20x","proxy":{"expected_egress_ip":"192.0.2.9","host":"proxy.example.test","password_ref":"op://example-proxies/proxy-account/password","port":8080,"username_ref":"op://example-proxies/proxy-account/username"},"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_create_with_receiver_and_proxy_kind", append(append([]string{"account", "create", "sample-new"}, accountCreateFlags...), "--receiver-email", "receiver@example.test", "--proxy-provider", "example-provider", "--proxy-type", "dedicated-isp"), "POST", "/accounts",
			`{"alias":"sample-new","device_id":"` + testDeviceID + `","email":"new@example.test","plan":"max-20x","proxy":{"expected_egress_ip":"192.0.2.9","host":"proxy.example.test","password_ref":"op://example-proxies/proxy-account/password","port":8080,"provider":"example-provider","type":"dedicated-isp","username_ref":"op://example-proxies/proxy-account/username"},"receiver_email":"receiver@example.test","source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_set", []string{"account", "set", "sample-a", "--rev", "2", "--receiver-email", "", "--proxy-type", "dedicated-isp", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "PATCH", "/accounts/sample-a",
			`{"proxy":{"type":"dedicated-isp"},"receiver_email":"","rev":2,"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_set_device_id", []string{"account", "set", "sample-a", "--rev", "2", "--device-id", testDeviceID, "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "PATCH", "/accounts/sample-a",
			`{"device_id":"` + testDeviceID + `","rev":2,"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_set_without_field", []string{"account", "set", "sample-a", "--rev", "2", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_set_without_rev", []string{"account", "set", "sample-a", "--receiver-email", "r@example.test", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_set_other_field", []string{"account", "set", "sample-a", "--rev", "2", "--email", "r@example.test", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_create_without_device_id", withoutFlag(accountCreateFlags, "--device-id"), "", "", ""},
		{"account_create_without_plan", withoutFlag(accountCreateFlags, "--plan"), "", "", ""},
		{"account_create_without_source_ts", withoutFlag(accountCreateFlags, "--source-ts"), "", "", ""},
		{"account_create_with_rev", append(append([]string{"account", "create", "sample-new"}, accountCreateFlags...), "--rev", "1"), "", "", ""},
		{"account_show", []string{"account", "show", "sample-a", "--admin-socket", "admin.sock"}, "GET", "/accounts/sample-a", ""},
		{"account_show_with_source", []string{"account", "show", "sample-a", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_verify", []string{"account", "verify", "sample-a", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/verify",
			`{"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_verify_with_rev", []string{"account", "verify", "sample-a", "--rev", "2", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/verify",
			`{"rev":2,"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_activate", []string{"account", "activate", "sample-a", "--rev", "2", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/activate",
			`{"rev":2,"source":{"kind":"slack","thread":"` + slackThread + `","ts":"` + slackTS + `"}}`},
		{"account_activate_without_rev", []string{"account", "activate", "sample-a", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_unknown_verb", []string{"account", "retire", "sample-a", "--rev", "2", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_bad_alias", []string{"account", "show", "Sample A", "--admin-socket", "admin.sock"}, "", "", ""},
		{"account_with_pause_reason", []string{"account", "show", "sample-a", "--reason", "x", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token", []string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--source-kind", "dashboard", "--admin-socket", "admin.sock"}, "POST", "/accounts/sample-a/setup/token?request_id=" + setupRequestID + "&run_id=" + setupRunID + "&source_kind=dashboard", ""},
		{"setup_without_verb", []string{"setup", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_unknown_verb", []string{"setup", "done", "sample-a", setupRequestID, "--run-id", setupRunID, "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_next_with_alias", []string{"setup", "next", "sample-a", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_next_with_run_id", []string{"setup", "next", "--run-id", setupRunID, "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_report_without_run_id", []string{"setup", "report", "sample-a", setupRequestID, "--state", "running", "--step", "cli", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_report_without_state", []string{"setup", "report", "sample-a", setupRequestID, "--run-id", setupRunID, "--step", "cli", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_report_without_step", []string{"setup", "report", "sample-a", setupRequestID, "--run-id", setupRunID, "--state", "running", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_report_without_request_id", []string{"setup", "report", "sample-a", "--run-id", setupRunID, "--state", "running", "--step", "cli", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_report_bad_alias", []string{"setup", "report", "Sample A", setupRequestID, "--run-id", setupRunID, "--state", "running", "--step", "cli", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token_with_state", []string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--state", "running", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token_with_reason", []string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--reason", "x", "--admin-socket", "admin.sock"}, "", "", ""},
		{"setup_token_without_run_id", []string{"setup", "token", "sample-a", setupRequestID, "--admin-socket", "admin.sock"}, "", "", ""},
		{"run_id_outside_setup", []string{"status", "--run-id", setupRunID, "--admin-socket", "admin.sock"}, "", "", ""},
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

// TestSetupTokenCommand checks that the token is read from stdin and sent as the raw body
// of the handover, with one line break dropped, as text, and that a token of more than
// 4 KiB is refused before anything is sent; an answer of the gateway's, success or refusal,
// never holds the token on the command's output.
func TestSetupTokenCommand(t *testing.T) {
	const token = "sk-ant-oat01-SYNTHETIC-TOKEN_0123"
	socket := filepath.Join(t.TempDir(), "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var received []string
	var types []string
	status := http.StatusOK
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = append(received, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		types = append(types, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"probe":"ok","written":true,"reload":"ok"}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"probe_refused","written":false}`)
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	parse := func() adminCommand {
		cmd, err := parseAdminCommand([]string{"setup", "token", "sample-a", setupRequestID, "--run-id", setupRunID, "--source-kind", "dashboard", "--admin-socket", socket})
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	wantPath := "POST /accounts/sample-a/setup/token?request_id=" + setupRequestID + "&run_id=" + setupRunID + "&source_kind=dashboard "
	for _, input := range []string{token, token + "\n", token + "\r\n"} {
		cmd := parse()
		cmd.input = strings.NewReader(input)
		var out bytes.Buffer
		if err := runAdmin(t.Context(), cmd, &out); err != nil || strings.Contains(out.String(), token) {
			t.Fatalf("handover of %q: %v %q", input, err, out.String())
		}
	}
	if len(received) != 3 || received[0] != wantPath+token || received[1] != received[0] || received[2] != received[0] || types[0] != "text/plain" {
		t.Fatalf("received %q with types %q", received, types)
	}
	// A refusal exits with an error that names the code, not the token.
	status = http.StatusBadGateway
	cmd := parse()
	cmd.input = strings.NewReader(token)
	var out bytes.Buffer
	if err := runAdmin(t.Context(), cmd, &out); err == nil || !strings.HasSuffix(err.Error(), "refused: probe_refused (written: false)") || strings.Contains(err.Error(), token) || out.Len() != 0 {
		t.Fatalf("refusal: %v %q", err, out.String())
	}
	// 4096 bytes go; 4097 do not, and nothing is sent.
	status = http.StatusOK
	before := len(received)
	cmd = parse()
	cmd.input = strings.NewReader(strings.Repeat("a", maxSetupInput) + "\n")
	if err := runAdmin(t.Context(), cmd, &out); err != nil {
		t.Fatalf("4096 bytes: %v", err)
	}
	cmd = parse()
	cmd.input = strings.NewReader(strings.Repeat("a", maxSetupInput+1))
	if err := runAdmin(t.Context(), cmd, &out); err == nil || !strings.Contains(err.Error(), "4 KiB") || len(received) != before+1 {
		t.Fatalf("4097 bytes: %v, requests %d", err, len(received)-before)
	}
}

// testDeviceID is a made-up device ID.
const testDeviceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// accountCreateFlags are the flags of a complete `account create` after its alias.
var accountCreateFlags = []string{"--email", "new@example.test", "--device-id", testDeviceID, "--plan", "max-20x", "--proxy-host", "proxy.example.test", "--proxy-port", "8080",
	"--expected-egress-ip", "192.0.2.9", "--proxy-username-ref", "op://example-proxies/proxy-account/username",
	"--proxy-password-ref", "op://example-proxies/proxy-account/password", "--source-thread", slackThread, "--source-ts", slackTS, "--admin-socket", "admin.sock"}

// withoutFlag returns `account create sample-new` with the flags but without the named one
// and its value.
func withoutFlag(flags []string, name string) []string {
	out := []string{"account", "create", "sample-new"}
	for i := 0; i < len(flags); i++ {
		if flags[i] == name {
			i++
			continue
		}
		out = append(out, flags[i])
	}
	return out
}
