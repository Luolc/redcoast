package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// The Slack message the commands of these tests say they came from.
const (
	slackThread = "C0TESTCHAN1/1700000000.000100"
	slackTS     = "1700000000.000200"
)

func slackSource() map[string]string {
	return map[string]string{"kind": "slack", "thread": slackThread, "ts": slackTS}
}

// agent sends a JSON request to the management interface and returns the answer.
func (h *setupHarness) agent(t *testing.T, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	var text string
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		text = string(data)
	}
	return h.adminDo(t, method, path, "application/json", text)
}

// createBody is a complete `account create` body for alias, behind the fixture's exit.
func (h *setupHarness) agentCreateBody(alias string) map[string]any {
	host, port := h.exitAddress()
	var portNumber int
	_, _ = fmt.Sscan(port, &portNumber)
	return map[string]any{"alias": alias, "email": "new@example.test", "device_id": testDeviceID, "plan": "pro", "source": slackSource(),
		"proxy": map[string]any{"host": host, "port": portNumber, "expected_egress_ip": "192.0.2.2",
			"username_ref": "op://example-proxies/proxy-account/username", "password_ref": "op://example-proxies/proxy-account/password"}}
}

// show returns the answer of `account show`.
func (h *setupHarness) show(t *testing.T, alias string) map[string]any {
	t.Helper()
	status, answer, body := h.agent(t, "GET", "/accounts/"+alias, nil)
	if status != 200 {
		t.Fatalf("show %s: %d %s", alias, status, body)
	}
	return answer
}

// revOf returns the account's revision as the store has it.
func (h *setupHarness) revOf(t *testing.T, alias string) int64 {
	t.Helper()
	row, err := h.store.Account(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	return row.Rev
}

// TestAgentPathEndToEnd walks the whole path of an account added from a Slack message,
// through the management socket only: create with a plan, queue the task, take it by its
// ID, report, hand the token over, verify and activate. Every step is audited with the
// message, and the account ends active and served. The task is not handed to the poll
// the login machine at the desk makes, and a report or a token that says the task came
// from the dashboard is refused and writes nothing.
func TestAgentPathEndToEnd(t *testing.T) {
	h := newSetupHarness(t, true)
	h.echo.mu.Lock()
	h.echo.ip = "192.0.2.2"
	h.echo.mu.Unlock()

	status, answer, body := h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	if status != 200 || answer["account"].(map[string]any)["status"] != "testing" {
		t.Fatalf("create: %d %s", status, body)
	}
	shown := h.show(t, "sample-new")
	created, _ := shown["created"].(map[string]any)
	if created == nil || created["action"] != "account-create" || created["source_ip"] != "admin-socket" || created["source"].(map[string]any)["thread"] != slackThread || shown["served"] != false {
		t.Fatalf("show after create: %s", body)
	}
	if setup := shown["setup"].(map[string]any); setup["state"] != "not_started" {
		t.Fatalf("setup block = %v", setup)
	}

	rev := h.revOf(t, "sample-new")
	status, answer, body = h.agent(t, "POST", "/accounts/sample-new/setup/request", map[string]any{"rev": rev, "source": slackSource()})
	block, _ := answer["setup"].(map[string]any)
	if status != 200 || block["state"] != "requested" || block["source"].(map[string]any)["kind"] != "slack" || block["source"].(map[string]any)["ts"] != slackTS {
		t.Fatalf("setup request: %d %s", status, body)
	}
	id := block["request_id"].(string)

	// The poll of the login machine at the desk does not take it; the one by ID does.
	if task := h.next(t); task != nil {
		t.Fatalf("the desk's poll took a task from Slack: %v", task)
	}
	status, answer, body = h.adminDo(t, "POST", "/setup/next?request_id="+id, "", "")
	task, _ := answer["task"].(map[string]any)
	if status != 200 || task == nil || task["request_id"] != id || task["status"] != "testing" || task["source"].(map[string]any)["thread"] != slackThread {
		t.Fatalf("next by ID: %d %s", status, body)
	}

	// A caller that says the task came from the dashboard is refused, and nothing is claimed.
	if status, answer, _ := h.reportAs(t, "dashboard", "sample-new", id, setupRun, "running", "lock", nil, nil); status != 409 || answer["error"] != "source_mismatch" {
		t.Fatalf("report as dashboard: %d %v", status, answer)
	}
	if status, answer, _ := h.reportAs(t, "slack", "sample-new", id, setupRun, "running", "lock", nil, nil); status != 200 {
		t.Fatalf("report as slack: %d %v", status, answer)
	}
	if status, answer, _ := h.tokenAs(t, "dashboard", "sample-new", id, setupRun, setupToken); status != 409 || answer["error"] != "source_mismatch" {
		t.Fatalf("token as dashboard: %d %v", status, answer)
	}
	h.nothingSent(t)
	status, answer, body = h.tokenAs(t, "slack", "sample-new", id, setupRun, setupToken)
	if status != 200 || answer["written"] != true || answer["setup"].(map[string]any)["state"] != "done" {
		t.Fatalf("token as slack: %d %s", status, body)
	}

	rev = h.revOf(t, "sample-new")
	if status, answer, body := h.agent(t, "POST", "/accounts/sample-new/verify", map[string]any{"rev": rev, "source": slackSource()}); status != 200 || answer["verify"].(map[string]any)["token_ok"].(map[string]any)["result"] != "pass" {
		t.Fatalf("verify: %d %s", status, body)
	}
	rev = h.revOf(t, "sample-new")
	if status, answer, body := h.agent(t, "POST", "/accounts/sample-new/activate", map[string]any{"rev": rev, "source": slackSource()}); status != 200 || answer["account"].(map[string]any)["status"] != "active" {
		t.Fatalf("activate: %d %s", status, body)
	}
	shown = h.show(t, "sample-new")
	if shown["served"] != true {
		t.Fatalf("the activated account is not served: %v", shown["served"])
	}
	// Every step the path wrote carries the message in the audit.
	want := map[string]bool{"account-create": false, "account-setup": false, "account-setup-token": false, "account-verify": false, "account-activate": false}
	for _, row := range shown["history"].([]any) {
		row := row.(map[string]any)
		action, _ := row["action"].(string)
		if _, ok := want[action]; !ok && action != "account-create" {
			continue
		}
		if source := row["source"].(map[string]any); source["kind"] != "slack" || source["thread"] != slackThread || source["ts"] != slackTS {
			t.Fatalf("audit row %s has source %v", action, source)
		}
		want[action] = true
	}
	for action, seen := range want {
		if !seen {
			t.Fatalf("no audit row for %s: %v", action, shown["history"])
		}
	}
	if strings.Contains(h.logs.String(), slackThread) || strings.Contains(body, setupToken) {
		t.Fatal("the thread is in the log, or the token in an answer")
	}
}

// TestAgentPathStopsWhereItFailed: a handover the upstream refuses leaves the account as
// it was and the setup block, the audit and the next activation say where it stopped.
func TestAgentPathStopsWhereItFailed(t *testing.T) {
	h := newSetupHarness(t, true)
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	_, answer, _ := h.agent(t, "POST", "/accounts/sample-new/setup/request", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	id := answer["setup"].(map[string]any)["request_id"].(string)
	h.adminDo(t, "POST", "/setup/next?request_id="+id, "", "")
	h.reportAs(t, "slack", "sample-new", id, setupRun, "running", "token_read", nil, nil)
	h.mu.Lock()
	h.probeStatus = 401
	h.mu.Unlock()
	status, answer, body := h.tokenAs(t, "slack", "sample-new", id, setupRun, setupToken)
	if status != 502 || answer["error"] != "probe_refused" || answer["written"] != false {
		t.Fatalf("handover: %d %s", status, body)
	}
	shown := h.show(t, "sample-new")
	setup := shown["setup"].(map[string]any)
	if setup["state"] != "failed" || setup["step"] != "probe" || setup["reason"] != "probe_refused" || shown["account"].(map[string]any)["status"] != "testing" {
		t.Fatalf("show after a failed handover: %v", shown)
	}
	history := shown["history"].([]any)
	last := history[len(history)-1].(map[string]any)
	if last["action"] != "account-setup-token" || last["source"].(map[string]any)["thread"] != slackThread || last["changes"].(map[string]any)["setup"].(map[string]any)["new"] != "failed:probe_refused" {
		t.Fatalf("last audit row = %v", last)
	}
	// The account is not activated: its checks, run again, stop where the upstream said no.
	h.echo.mu.Lock()
	h.echo.ip = "192.0.2.2"
	h.echo.mu.Unlock()
	status, answer, _ = h.agent(t, "POST", "/accounts/sample-new/activate", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	if status != 409 || answer["error"] != "not_ready" || answer["check"] != "probe_ok" {
		t.Fatalf("activate: %d %v", status, answer)
	}
}

// TestAgentWritesNeedTheSwitch: with admin_write off every writing command answers 501
// account_write_unavailable before it reads the body, and writes nothing; show still
// reads. With setup off as well, the switch is named first. The control arm is the whole
// path of the test above with the switch on.
func TestAgentWritesNeedTheSwitch(t *testing.T) {
	h := newSetupHarness(t, true)
	h.writer.cfg.AdminWrite = false
	before := h.state(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/accounts", h.agentCreateBody("sample-new")},
		{"POST", "/accounts/sample-a/verify", map[string]any{"source": slackSource()}},
		{"POST", "/accounts/sample-a/activate", map[string]any{"rev": 1, "source": slackSource()}},
		{"POST", "/accounts/sample-a/setup/request", map[string]any{"rev": 1, "source": slackSource()}},
		{"PATCH", "/accounts/sample-a", map[string]any{"rev": 1, "receiver_email": "x@example.test", "source": slackSource()}},
		{"POST", "/accounts", "not even json"},
	} {
		if status, answer, body := h.agent(t, c.method, c.path, c.body); status != 501 || answer["error"] != "account_write_unavailable" {
			t.Fatalf("%s %s: %d %s", c.method, c.path, status, body)
		}
	}
	if h.state(t) != before {
		t.Fatal("a refused command wrote")
	}
	if shown := h.show(t, "sample-a"); shown["account"].(map[string]any)["alias"] != "sample-a" {
		t.Fatalf("show with the switch off: %v", shown)
	}
	// Switch on, setup off: a setup request names the setup switch.
	off := newSetupHarness(t, false)
	if status, answer, _ := off.agent(t, "POST", "/accounts/sample-a/setup/request", map[string]any{"rev": 1, "source": slackSource()}); status != 501 || answer["error"] != "setup_unavailable" {
		t.Fatalf("setup off: %d %v", status, answer)
	}
	off.writer.cfg.AdminWrite = false
	if status, answer, _ := off.agent(t, "POST", "/accounts/sample-a/setup/request", map[string]any{"rev": 1, "source": slackSource()}); status != 501 || answer["error"] != "account_write_unavailable" {
		t.Fatalf("both off: %d %v", status, answer)
	}
	// No writer at all (a gateway with no write path): every account command is 501.
	admin := NewAdmin(h.accounts, h.store, nil)
	recorder := httptest.NewRecorder()
	admin.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/accounts/sample-a", nil))
	if recorder.Code != 501 {
		t.Fatalf("no writer: %d", recorder.Code)
	}
}

// TestAgentSourceChecks: a write needs a source in the form of a Slack message; each way
// it can be wrong is refused with its own code and writes nothing, and the body is as
// strict as the page's.
func TestAgentSourceChecks(t *testing.T) {
	h := newSetupHarness(t, true)
	before := h.state(t)
	with := func(source any) map[string]any {
		body := h.agentCreateBody("sample-new")
		if source == nil {
			delete(body, "source")
		} else {
			body["source"] = source
		}
		return body
	}
	for _, test := range []struct {
		name   string
		body   any
		status int
		code   string
		field  string
	}{
		{"no source", with(nil), 400, "source_missing", "source"},
		{"kind is dashboard", with(map[string]string{"kind": "dashboard", "thread": slackThread, "ts": slackTS}), 400, "source_invalid", "source.kind"},
		{"thread without a slash", with(map[string]string{"kind": "slack", "thread": "C0TESTCHAN11700000000.000100", "ts": slackTS}), 400, "source_invalid", "source.thread"},
		{"thread in lower case", with(map[string]string{"kind": "slack", "thread": "c0testchan1/1700000000.000100", "ts": slackTS}), 400, "source_invalid", "source.thread"},
		{"ts without microseconds", with(map[string]string{"kind": "slack", "thread": slackThread, "ts": "1700000000"}), 400, "source_invalid", "source.ts"},
		{"unknown key", func() any { b := h.agentCreateBody("sample-new"); b["access"] = "direct"; return b }(), 400, "bad_request", ""},
		{"a direct key in the proxy", func() any {
			b := h.agentCreateBody("sample-new")
			b["proxy"].(map[string]any)["direct"] = true
			return b
		}(), 400, "bad_request", ""},
		{"not an object", []string{"x"}, 400, "bad_request", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, answer, body := h.agent(t, "POST", "/accounts", test.body)
			if status != test.status || answer["error"] != test.code || answer["field"] != nilIfEmpty(test.field) {
				t.Fatalf("%d %s", status, body)
			}
			if h.state(t) != before {
				t.Fatal("a refused command wrote")
			}
		})
	}
	if status, answer, _ := h.adminDo(t, "POST", "/accounts", "application/json", `{"alias":"sample-new","alias":"x"}`); status != 400 || answer["error"] != "bad_request" {
		t.Fatalf("repeated key: %d %v", status, answer)
	}
	if status, answer, _ := h.adminDo(t, "POST", "/accounts", "application/json", `{"alias":"`+strings.Repeat("a", 9000)+`"}`); status != 400 || answer["error"] != "bad_request" {
		t.Fatalf("over 8 KiB: %d %v", status, answer)
	}
	// A path alias that is none.
	if status, answer, _ := h.agent(t, "POST", "/accounts/Sample%20A/verify", map[string]any{"source": slackSource()}); status != 400 || answer["error"] != "field_invalid" || answer["field"] != "alias" {
		t.Fatalf("bad alias: %d %v", status, answer)
	}
	// Control arm: the same request with a good source is taken.
	if status, _, body := h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new")); status != 200 {
		t.Fatalf("control: %d %s", status, body)
	}
}

// nilIfEmpty returns nil for "" so that a missing field in an answer compares equal.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestAgentCreateRefusals: each way a create can be refused answers a fixed code, the
// field it concerns, and writes nothing; the controls are the creates that are taken.
func TestAgentCreateRefusals(t *testing.T) {
	h := newSetupHarness(t, true)
	before := h.state(t)
	change := func(edit func(b map[string]any)) any {
		b := h.agentCreateBody("sample-new")
		edit(b)
		return b
	}
	proxy := func(b map[string]any) map[string]any { return b["proxy"].(map[string]any) }
	for _, test := range []struct {
		name        string
		body        any
		status      int
		code, field string
	}{
		{"no email", change(func(b map[string]any) { delete(b, "email") }), 400, "field_missing", "email"},
		{"no proxy", change(func(b map[string]any) { delete(b, "proxy") }), 400, "field_missing", "proxy"},
		{"no plan", change(func(b map[string]any) { delete(b, "plan") }), 400, "field_missing", "plan"},
		{"alias is none", change(func(b map[string]any) { b["alias"] = "Sample New" }), 400, "field_invalid", "alias"},
		{"email is none", change(func(b map[string]any) { b["email"] = "nobody" }), 400, "field_invalid", "email"},
		{"plan does not exist", change(func(b map[string]any) { b["plan"] = "nope" }), 400, "field_invalid", "plan"},
		{"port is none", change(func(b map[string]any) { proxy(b)["port"] = 0 }), 400, "field_invalid", "proxy.port"},
		{"egress IP is none", change(func(b map[string]any) { proxy(b)["expected_egress_ip"] = "x" }), 400, "field_invalid", "proxy.expected_egress_ip"},
		{"proxy login that no account uses", change(func(b map[string]any) { proxy(b)["username_ref"] = "op://example-proxies/other/username" }), 400, "field_invalid", "proxy.username_ref"},
		{"device ID is missing", change(func(b map[string]any) { delete(b, "device_id") }), 400, "field_missing", "device_id"},
		{"device ID is none", change(func(b map[string]any) { b["device_id"] = "nope" }), 400, "field_invalid", "device_id"},
		{"alias exists", change(func(b map[string]any) { b["alias"] = "sample-a" }), 409, "account_exists", "alias"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, answer, body := h.agent(t, "POST", "/accounts", test.body)
			if status != test.status || answer["error"] != test.code || answer["field"] != test.field {
				t.Fatalf("%d %s", status, body)
			}
			if h.state(t) != before {
				t.Fatal("a refused create wrote")
			}
		})
	}
	h.writer.cfg.TokenRefTemplate = ""
	if status, answer, _ := h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new")); status != 409 || answer["error"] != "no_token_template" {
		t.Fatalf("no template: %d %v", status, answer)
	}
	h.writer.cfg.TokenRefTemplate = h.cfg.TokenRefTemplate
	if status, _, body := h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new")); status != 200 {
		t.Fatalf("control: %d %s", status, body)
	}
	// The account has the token reference the template gives and cannot be set otherwise.
	row, _ := h.store.Account(t.Context(), "sample-new")
	if row.Access != "gateway" || row.Direct || row.Status != "testing" || !strings.Contains(row.TokenRef, "sample-new") {
		t.Fatalf("created row = %+v", row)
	}
}

// TestAgentActivateOnlyTesting: through the management socket only a testing account is
// activated; the page's activation of the same account is not held to that.
func TestAgentActivateOnlyTesting(t *testing.T) {
	h := newSetupHarness(t, true)
	h.echo.mu.Lock()
	h.echo.ip = "192.0.2.2"
	h.echo.mu.Unlock()
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	for _, status := range []string{"paused", "suspended", "retired", "active"} {
		row, _ := h.store.Account(t.Context(), "sample-new")
		saved, err := h.store.UpdateAccountKeepRev(t.Context(), "sample-new", row.Rev, session.AuditEntry{Action: "test"}, func(r *session.AccountRow) { r.Status = status })
		if err != nil {
			t.Fatal(err)
		}
		code, answer, body := h.agent(t, "POST", "/accounts/sample-new/activate", map[string]any{"rev": saved.Rev, "source": slackSource()})
		if code != 409 || answer["error"] != "not_ready" || answer["check"] != "status" {
			t.Fatalf("%s: %d %s", status, code, body)
		}
	}
	// The page is not held to testing: for a paused account it goes on to its own checks
	// and activates it.
	row, _ := h.store.Account(t.Context(), "sample-new")
	paused, _ := h.store.UpdateAccountKeepRev(t.Context(), "sample-new", row.Rev, session.AuditEntry{Action: "test"}, func(r *session.AccountRow) { r.Status = "paused" })
	pageStatus, pageAnswer, pageBody := h.serve(t, request("POST", "/accounts/sample-new/activate", fmt.Sprintf(`{"rev":%d}`, paused.Rev)))
	if pageStatus == 409 && pageAnswer["check"] == "status" {
		t.Fatalf("the page's activation was held to testing: %d %s", pageStatus, pageBody)
	}
}

// TestAgentVerifyRefusals: the codes of verify that the page answers with sentences.
func TestAgentVerifyRefusals(t *testing.T) {
	h := newSetupHarness(t, true)
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	rev := h.revOf(t, "sample-new")
	post := func(alias string, rev any) (int, map[string]any) {
		body := map[string]any{"source": slackSource()}
		if rev != nil {
			body["rev"] = rev
		}
		status, answer, _ := h.agent(t, "POST", "/accounts/"+alias+"/verify", body)
		return status, answer
	}
	if status, answer := post("nobody", nil); status != 404 || answer["error"] != "unknown_account" {
		t.Fatalf("unknown: %d %v", status, answer)
	}
	if status, answer := post("sample-new", rev+5); status != 409 || answer["error"] != "rev_stale" || answer["field"] != "rev" {
		t.Fatalf("stale: %d %v", status, answer)
	}
	h.writer.cfg.Resolve = func(context.Context) (Resolver, error) { return nil, fmt.Errorf("no resolver") }
	if status, answer := post("sample-new", rev); status != 502 || answer["error"] != "resolver_unavailable" {
		t.Fatalf("resolver: %d %v", status, answer)
	}
	if status, answer, _ := h.agent(t, "POST", "/accounts/sample-new/activate", map[string]any{"rev": rev, "source": slackSource()}); status != 502 || answer["error"] != "resolver_unavailable" {
		t.Fatalf("activate with no resolver: %d %v", status, answer)
	}
	// An account that is not on the gateway path is not verified.
	direct, err := h.store.CreateAccount(t.Context(), session.AccountRow{Alias: "sample-dir", Email: "dir@example.test", Access: "direct", Status: "testing", TokenRef: "op://example-vault/token-sample-dir/credential"}, session.AuditEntry{Action: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h.writer.cfg.Resolve = h.cfg.Resolve
	if status, answer := post("sample-dir", direct.Rev); status != 409 || answer["error"] != "not_ready" || answer["check"] != "access" {
		t.Fatalf("access: %d %v", status, answer)
	}
}

// TestAgentShow: show gives the settings, the setup block and the audit rows without any
// credential; an unknown account is 404; a long history is cut to the last 100 rows with
// the creation row still given.
func TestAgentShow(t *testing.T) {
	h := newSetupHarness(t, true)
	if status, answer, _ := h.agent(t, "GET", "/accounts/nobody", nil); status != 404 || answer["error"] != "unknown_account" {
		t.Fatalf("unknown: %d %v", status, answer)
	}
	if status, answer, _ := h.agent(t, "GET", "/accounts/Sample%20A", nil); status != 400 || answer["error"] != "field_invalid" {
		t.Fatalf("bad alias: %d %v", status, answer)
	}
	shown := h.show(t, "sample-a")
	if shown["served"] != true || shown["created"] != nil || shown["history_truncated"] != false || len(shown["history"].([]any)) != 0 {
		t.Fatalf("an imported account: %v", shown)
	}
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	for i := 0; i < 105; i++ {
		entry := session.AuditEntry{SourceIP: "admin-socket", Action: "test", Source: session.Source{Kind: "slack", Thread: slackThread, TS: slackTS}}
		if err := h.store.RecordAudit(t.Context(), "sample-new", entry, map[string]session.FieldChange{"n": {Old: fmt.Sprint(i), New: fmt.Sprint(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	shown = h.show(t, "sample-new")
	history := shown["history"].([]any)
	if len(history) != 100 || shown["history_truncated"] != true || history[99].(map[string]any)["changes"].(map[string]any)["n"].(map[string]any)["new"] != "105" || shown["created"] == nil {
		t.Fatalf("history of %d rows, truncated %v, created %v", len(history), shown["history_truncated"], shown["created"])
	}
	// No answer holds a token reference's value, and the setup block is null with setup off.
	text, _ := json.Marshal(shown)
	if strings.Contains(string(text), oldToken) {
		t.Fatal("show holds a token")
	}
	off := newSetupHarness(t, false)
	if shown := off.show(t, "sample-a"); shown["setup"] != nil {
		t.Fatalf("setup with setup off: %v", shown["setup"])
	}
}

// TestSetupNextByID: the agent takes only its own task, and each way it cannot is a code.
func TestSetupNextByID(t *testing.T) {
	h := newSetupHarness(t, true)
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	_, answer, _ := h.agent(t, "POST", "/accounts/sample-new/setup/request", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	slackID := answer["setup"].(map[string]any)["request_id"].(string)
	row, _ := h.store.Account(t.Context(), "sample-a")
	_, answer, _ = h.page(t, "sample-a", fmt.Sprintf(`{"action":"login_and_token","rev":%d}`, row.Rev))
	dashboardID := answer["setup"].(map[string]any)["request_id"].(string)
	byID := func(id string) (int, map[string]any) {
		status, answer, _ := h.adminDo(t, "POST", "/setup/next?request_id="+id, "", "")
		return status, answer
	}
	if status, answer := byID("xyz"); status != 400 || answer["error"] != "bad_request" {
		t.Fatalf("bad ID: %d %v", status, answer)
	}
	if status, answer := byID(strings.Repeat("a", 32)); status != 404 || answer["error"] != "task_not_found" {
		t.Fatalf("unknown ID: %d %v", status, answer)
	}
	if status, answer := byID(dashboardID); status != 409 || answer["error"] != "task_not_slack" {
		t.Fatalf("dashboard task: %d %v", status, answer)
	}
	// The desk's poll takes the dashboard task, and then the Slack task has to wait.
	if task := h.next(t); task == nil || task["request_id"] != dashboardID {
		t.Fatalf("the desk's poll gave %v", task)
	}
	if status, answer := byID(slackID); status != 200 || answer["task"] != nil {
		t.Fatalf("while another task runs: %d %v", status, answer)
	}
	// Once the other task is over, the Slack one is handed out, once.
	h.reportAs(t, "dashboard", "sample-a", dashboardID, setupRun, "failed", "lock", "account_busy", nil)
	if status, answer := byID(slackID); status != 200 || answer["task"] == nil {
		t.Fatalf("after the other task: %d %v", status, answer)
	}
	if status, answer := byID(slackID); status != 409 || answer["error"] != "task_not_requested" {
		t.Fatalf("a second time: %d %v", status, answer)
	}
}

// TestSetupWaitUntil: a login program at a captcha page asks for a wait once per task, for
// at most 20 minutes; a later step clears it, and the task cannot ask again.
func TestSetupWaitUntil(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	at := func(d time.Duration) string { return h.clock.Add(d).UTC().Format(time.RFC3339) }
	report := func(state, step string, wait any) (int, map[string]any) {
		reason := any(nil)
		if state == "failed" {
			reason = "account_busy"
		}
		status, answer, _ := h.reportAs(t, "dashboard", "sample-a", id, setupRun, state, step, reason, wait)
		return status, answer
	}
	for _, test := range []struct {
		name        string
		state, step string
		wait        any
		status      int
		code        string
	}{
		{"not a time", "running", "awaiting_captcha", "soon", 400, "wait_until_invalid"},
		{"not the captcha step", "running", "lock", at(10 * time.Minute), 400, "wait_until_invalid"},
		{"not the running state", "failed", "awaiting_captcha", at(10 * time.Minute), 400, "wait_until_invalid"},
		{"in the past", "running", "awaiting_captcha", at(-time.Minute), 400, "wait_until_invalid"},
		{"over 20 minutes", "running", "awaiting_captcha", at(20*time.Minute + time.Second), 400, "wait_until_invalid"},
	} {
		if status, answer := report(test.state, test.step, test.wait); status != test.status || answer["error"] != test.code {
			t.Fatalf("%s: %d %v", test.name, status, answer)
		}
		if got := h.setupState(t, "sample-a"); got.Step != "lock" || got.State != "running" || got.WaitUntilSet {
			t.Fatalf("%s: a refused report changed the task: %+v", test.name, got)
		}
	}
	// 20 minutes exactly is taken, and the block says until when.
	status, answer := report("running", "awaiting_captcha", at(20*time.Minute))
	block, _ := answer["setup"].(map[string]any)
	if status != 200 || block["wait_until"] != at(20*time.Minute) || block["step"] != "awaiting_captcha" {
		t.Fatalf("20 minutes: %d %v", status, answer)
	}
	// Once only: a second wait is refused; a report without one is taken and leaves it.
	if status, answer := report("running", "awaiting_captcha", at(15*time.Minute)); status != 400 || answer["error"] != "wait_until_invalid" {
		t.Fatalf("second wait: %d %v", status, answer)
	}
	status, answer = report("running", "awaiting_captcha", nil)
	if block, _ := answer["setup"].(map[string]any); status != 200 || block["wait_until"] != at(20*time.Minute) {
		t.Fatalf("a report without a wait: %d %v", status, answer)
	}
	// Another step clears it, and the wait cannot be asked for again.
	status, answer = report("running", "email_sent", nil)
	if block, _ := answer["setup"].(map[string]any); status != 200 || block["wait_until"] != nil {
		t.Fatalf("after another step: %d %v", status, answer)
	}
	if status, answer := report("running", "awaiting_captcha", at(5*time.Minute)); status != 400 || answer["error"] != "wait_until_invalid" {
		t.Fatalf("asking again: %d %v", status, answer)
	}
}

// TestStoreUnavailableCode: the code for a store that cannot be read is the same on the
// new commands and on the setup ones.
func TestStoreUnavailableCode(t *testing.T) {
	h := newSetupHarness(t, true)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if status, answer, _ := h.agent(t, "GET", "/accounts/sample-a", nil); status != 500 || answer["error"] != "store_unavailable" {
		t.Fatalf("show: %d %v", status, answer)
	}
	if status, answer, _ := h.adminDo(t, "POST", "/setup/next", "", ""); status != 500 || answer["error"] != "store_unavailable" {
		t.Fatalf("setup next: %d %v", status, answer)
	}
}

// TestAgentOverTheSocket sends the first steps of the path over a real unix socket, as the
// command does: create, show, and a setup request, each audited with the message.
func TestAgentOverTheSocket(t *testing.T) {
	h := newSetupHarness(t, true)
	socket := filepath.Join(t.TempDir(), "admin.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = session.ServeUnixListener(ctx, listener, h.admin); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	send := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
		req, _ := http.NewRequestWithContext(ctx, method, "http://gateway"+path, reader)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var answer map[string]any
		_ = json.NewDecoder(res.Body).Decode(&answer)
		return res.StatusCode, answer
	}
	if status, answer := send("POST", "/accounts", h.agentCreateBody("sample-new")); status != 200 {
		t.Fatalf("create: %d %v", status, answer)
	}
	status, answer := send("POST", "/accounts/sample-new/setup/request", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	if status != 200 || answer["setup"].(map[string]any)["state"] != "requested" {
		t.Fatalf("setup request: %d %v", status, answer)
	}
	status, answer = send("GET", "/accounts/sample-new", nil)
	history, _ := answer["history"].([]any)
	if status != 200 || len(history) < 3 || history[len(history)-1].(map[string]any)["source"].(map[string]any)["ts"] != slackTS {
		t.Fatalf("show: %d %v", status, answer)
	}
	if status, answer := send("POST", "/accounts", map[string]any{"alias": "x"}); status != 400 || answer["error"] != "source_missing" {
		t.Fatalf("a write with no source: %d %v", status, answer)
	}
}

// reportRows returns the audit rows of the reports of alias, oldest first.
func (h *setupHarness) reportRows(t *testing.T, alias string) []session.AuditRow {
	t.Helper()
	rows, _, err := h.store.AccountAuditLast(t.Context(), alias, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []session.AuditRow
	for _, row := range rows {
		if row.Action == actionSetupReport {
			out = append(out, row)
		}
	}
	return out
}

// TestSetupReportIsAudited: each accepted report writes one audit row of codes and a
// time, with the task's source; a refused report writes none.
func TestSetupReportIsAudited(t *testing.T) {
	h := newSetupHarness(t, true)
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	_, answer, _ := h.agent(t, "POST", "/accounts/sample-new/setup/request", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	id := answer["setup"].(map[string]any)["request_id"].(string)
	if status, _, body := h.adminDo(t, "POST", "/setup/next?request_id="+id, "", ""); status != 200 {
		t.Fatalf("next by ID: %d %s", status, body)
	}
	wait := h.clock.Add(15 * time.Minute).UTC().Format(time.RFC3339)
	if status, _, body := h.reportAs(t, "slack", "sample-new", id, setupRun, "running", "awaiting_captcha", nil, wait); status != 200 {
		t.Fatalf("captcha report: %d %s", status, body)
	}
	// Refused: a free-text reason, and a wrong request ID.
	h.reportAs(t, "slack", "sample-new", id, setupRun, "failed", "lock", setupToken, nil)
	h.reportAs(t, "slack", "sample-new", strings.Repeat("c", 32), setupRun, "running", "lock", nil, nil)
	if status, _, body := h.reportAs(t, "slack", "sample-new", id, setupRun, "needs_human", "email_page", "captcha_challenge", nil); status != 200 {
		t.Fatalf("needs_human report: %d %s", status, body)
	}
	rows := h.reportRows(t, "sample-new")
	if len(rows) != 2 {
		t.Fatalf("%d report rows, want 2 (refused reports write none)", len(rows))
	}
	first, second := rows[0], rows[1]
	if first.Source != (session.Source{Kind: "slack", Thread: slackThread, TS: slackTS}) || first.Changes["state"].New != "running" ||
		first.Changes["step"].New != "awaiting_captcha" || first.Changes["wait_until"].New != wait || len(first.Changes) != 3 {
		t.Fatalf("first row: %+v", first)
	}
	if second.Changes["state"].New != "needs_human" || second.Changes["reason"].New != "captcha_challenge" || len(second.Changes) != 3 {
		t.Fatalf("second row: %+v", second)
	}
	// A dashboard task's report carries no source.
	h.running(t, "sample-a")
	if rows := h.reportRows(t, "sample-a"); len(rows) != 1 || rows[0].Source.Kind != "" {
		t.Fatalf("dashboard task rows: %+v", rows)
	}
}

// TestDashboardRecord: GET /accounts/{alias}/record answers what `account show` does,
// behind the guards of the dashboard, with no credential in it, and 501 where writes
// are not configured.
func TestDashboardRecord(t *testing.T) {
	h := newSetupHarness(t, true)
	h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new"))
	_, answer, _ := h.agent(t, "POST", "/accounts/sample-new/setup/request", map[string]any{"rev": h.revOf(t, "sample-new"), "source": slackSource()})
	id := answer["setup"].(map[string]any)["request_id"].(string)
	h.adminDo(t, "POST", "/setup/next?request_id="+id, "", "")
	h.reportAs(t, "slack", "sample-new", id, setupRun, "running", "lock", nil, nil)
	get := func(alias, host string) (int, map[string]any, string) {
		r := httptest.NewRequest("GET", "/accounts/"+alias+"/record", nil)
		r.RemoteAddr, r.Host = "192.0.2.50:4000", host
		return h.serve(t, r)
	}
	status, record, body := get("sample-new", writeHost)
	if status != 200 {
		t.Fatalf("record: %d %s", status, body)
	}
	shown := h.show(t, "sample-new")
	for _, key := range []string{"account", "served", "setup", "created", "history", "history_truncated"} {
		if !reflect.DeepEqual(record[key], shown[key]) {
			t.Fatalf("%s differs from account show: %v vs %v", key, record[key], shown[key])
		}
	}
	for _, value := range append([]string{setupToken, oldToken}, writeValues...) {
		if strings.Contains(body, value) {
			t.Fatalf("the record holds %q", value)
		}
	}
	if status, answer, _ := get("sample-new", "other.example.test"); status == 200 || answer["history"] != nil {
		t.Fatalf("a request for another host got %d", status)
	}
	if status, answer, _ := get("Sample%20A", writeHost); status != 400 || answer["error"] != "field_invalid" {
		t.Fatalf("bad alias: %d %v", status, answer)
	}
	if status, answer, _ := get("nobody", writeHost); status != 404 || answer["error"] != "unknown_account" {
		t.Fatalf("unknown: %d %v", status, answer)
	}
	// With no source allowed to write, the route is 501.
	off := newSetupHarness(t, true)
	off.cfg.AllowFrom = nil
	off.build()
	r := httptest.NewRequest("GET", "/accounts/sample-a/record", nil)
	r.RemoteAddr, r.Host = "192.0.2.50:4000", writeHost
	if status, answer, _ := off.serve(t, r); status != 501 || answer["error"] != "account_write_unavailable" {
		t.Fatalf("without dashboard_write: %d %v", status, answer)
	}
	// A gateway with no account writer answers 501 too.
	none := HostGuard([]string{writeHost}, NewDashboard(h.egress, DashboardConfig{Provider: "codex", Store: h.store}).Handler())
	rec := httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/accounts/sample-a/record", nil)
	r.Host = writeHost
	none.ServeHTTP(rec, r)
	if rec.Code != 501 {
		t.Fatalf("without a writer: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAgentCreateTakesReceiverEmail: the management socket's create takes the optional
// receiver email and the proxy's provider and type, `account show` gives them, and a
// receiver email that is not a bare address is refused.
func TestAgentCreateTakesReceiverEmail(t *testing.T) {
	h := newSetupHarness(t, true)
	bad := h.agentCreateBody("sample-new")
	bad["receiver_email"] = "Someone <receiver@example.test>"
	if status, answer, body := h.agent(t, "POST", "/accounts", bad); status != 400 || answer["field"] != "receiver_email" {
		t.Fatalf("display name: %d %s", status, body)
	}
	good := h.agentCreateBody("sample-new")
	good["receiver_email"] = "receiver@example.test"
	good["proxy"].(map[string]any)["provider"], good["proxy"].(map[string]any)["type"] = "example-provider", "dedicated-isp"
	if status, _, body := h.agent(t, "POST", "/accounts", good); status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
	account := h.show(t, "sample-new")["account"].(map[string]any)
	proxy := account["proxy"].(map[string]any)
	if account["receiver_email"] != "receiver@example.test" || proxy["provider"] != "example-provider" || proxy["type"] != "dedicated-isp" {
		t.Fatalf("show = %v", account)
	}
}

// TestAgentSetBackfillsThreeFields: PATCH through the management socket changes the
// receiver email and the proxy's provider and type at the revision given, with the
// source audited, a key left out stays, an empty value clears; a stale revision is 409,
// and no other field is accepted.
func TestAgentSetBackfillsThreeFields(t *testing.T) {
	h := newSetupHarness(t, true)
	if status, _, body := h.agent(t, "POST", "/accounts", h.agentCreateBody("sample-new")); status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
	rev := h.revOf(t, "sample-new")
	set := func(rev int64, fields map[string]any) (int, map[string]any, string) {
		fields["rev"], fields["source"] = rev, slackSource()
		return h.agent(t, "PATCH", "/accounts/sample-new", fields)
	}
	status, answer, body := set(rev, map[string]any{"receiver_email": "receiver@example.test", "proxy": map[string]any{"provider": "example-provider"}})
	account, _ := answer["account"].(map[string]any)
	if status != 200 || account["receiver_email"] != "receiver@example.test" || account["proxy"].(map[string]any)["provider"] != "example-provider" || account["rev"] != float64(rev+1) {
		t.Fatalf("set: %d %s", status, body)
	}
	audit, err := h.store.AccountAudit(t.Context(), "sample-new")
	last := audit[len(audit)-1]
	if err != nil || last.Action != actionUpdate || last.Source.Thread != slackThread || last.Changes["receiver_email"].New != "receiver@example.test" {
		t.Fatalf("audit = %+v, %v", last, err)
	}
	// A key left out stays; an empty value clears.
	status, answer, body = set(rev+1, map[string]any{"receiver_email": ""})
	account, _ = answer["account"].(map[string]any)
	if status != 200 || account["receiver_email"] != "" || account["proxy"].(map[string]any)["provider"] != "example-provider" {
		t.Fatalf("clear: %d %s", status, body)
	}
	for _, test := range []struct {
		name   string
		rev    int64
		fields map[string]any
		status int
		field  string
	}{
		{"stale", rev, map[string]any{"receiver_email": "x@example.test"}, 409, "rev"},
		{"bad_address", rev + 2, map[string]any{"receiver_email": "Someone <x@example.test>"}, 400, "receiver_email"},
		{"email_is_not_settable", rev + 2, map[string]any{"email": "x@example.test"}, 400, ""},
		{"proxy_host_is_not_settable", rev + 2, map[string]any{"proxy": map[string]any{"host": "x.example.test"}}, 400, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, answer, body := set(test.rev, test.fields)
			if status != test.status || (test.field != "" && answer["field"] != test.field) {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
	if row, _ := h.store.Account(t.Context(), "sample-new"); row.Rev != rev+2 || row.Email != "new@example.test" {
		t.Fatalf("a refused set wrote: %+v", row)
	}
}
