package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// adminCall sends one management request and returns the status and decoded body.
func adminCall(t *testing.T, server *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%s %s answered non-JSON %q", method, path, data)
	}
	return res.StatusCode, decoded
}

// TestAdminReload checks that POST /reload answers with the report, and with 422 and
// the message when the reload refused (nothing changed).
func TestAdminReload(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, func(ctx context.Context) (Reload, error) { return h.accounts.Reload(ctx, h.dir, h.resolve, nil) }).Handler())
	defer admin.Close()
	h.rewrite(aAndB)
	status, body := adminCall(t, admin, http.MethodPost, "/reload", "")
	if status != 200 || body["added"] == nil || !slices.Equal(h.accounts.Candidates(), []string{"sample-a", "sample-b"}) {
		t.Fatalf("reload: status=%d body=%v candidates=%v", status, body, h.accounts.Candidates())
	}
	h.rewrite(map[string]string{"sample-a": "op://example-vault/token-missing/credential"})
	status, body = adminCall(t, admin, http.MethodPost, "/reload", "")
	message, _ := body["error"].(string)
	if status != 422 || !strings.Contains(message, "sample-a: oauth_token: cannot resolve") || !slices.Equal(h.accounts.Candidates(), []string{"sample-a", "sample-b"}) {
		t.Fatalf("refused reload: status=%d body=%v candidates=%v", status, body, h.accounts.Candidates())
	}
}

// TestAdminPauseResumeStatus checks the account commands against the binding rules: a
// paused account loses its sessions on their next request and shows as paused in
// status with its reason; after resume it is chosen again; an alias outside the set is
// refused, as is a pause without a reason.
func TestAdminPauseResumeStatus(t *testing.T) {
	h := newReloadHarness(t, aAndB)
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	defer admin.Close()
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-a" {
		t.Fatal("session not on sample-a")
	}
	if status, _ := adminCall(t, admin, http.MethodPost, "/accounts/sample-a/pause", `{}`); status != 400 {
		t.Fatalf("pause without a reason: status=%d", status)
	}
	if status, _ := adminCall(t, admin, http.MethodPost, "/accounts/sample-z/pause", `{"reason":"manual"}`); status != 404 {
		t.Fatalf("pause of an unknown alias: status=%d", status)
	}
	if h.bound(h.sessionID) != "sample-a" {
		t.Fatal("refused commands changed the binding")
	}
	status, body := adminCall(t, admin, http.MethodPost, "/accounts/sample-a/pause", `{"reason":"manual"}`)
	if status != 200 || body["paused_until"] != indefinitePause.Format(time.RFC3339) {
		t.Fatalf("pause: status=%d body=%v", status, body)
	}
	if h.send(h.token) != 200 || h.bound(h.sessionID) != "sample-b" {
		t.Fatalf("after the pause the session is on %q, want sample-b", h.bound(h.sessionID))
	}
	status, body = adminCall(t, admin, http.MethodGet, "/status", "")
	accounts, _ := body["accounts"].([]any)
	if status != 200 || len(accounts) != 2 || body["live_sessions"] != float64(1) || body["soft"] != 0.8 || body["hard"] != 0.9 {
		t.Fatalf("status: %d %v", status, body)
	}
	a, _ := accounts[0].(map[string]any)
	b, _ := accounts[1].(map[string]any)
	if a["alias"] != "sample-a" || a["pause_reason"] != "manual" || a["bound_sessions"] != float64(0) || b["alias"] != "sample-b" || b["pause_reason"] != nil || b["bound_sessions"] != float64(1) || b["active_sessions"] != float64(1) {
		t.Fatalf("status accounts: %v", accounts)
	}
	if status, _ := adminCall(t, admin, http.MethodPost, "/accounts/sample-a/resume", ""); status != 200 {
		t.Fatalf("resume: status=%d", status)
	}
	// Control arm of the pause: resumed, sample-a is chosen again by a fresh session
	// (sample-b carries the active one).
	token, id := h.issue()
	if h.send(token) != 200 || h.bound(id) != "sample-a" {
		t.Fatalf("after resume a fresh session landed on %q, want sample-a", h.bound(id))
	}
	status, body = adminCall(t, admin, http.MethodGet, "/status", "")
	accounts, _ = body["accounts"].([]any)
	a, _ = accounts[0].(map[string]any)
	if status != 200 || a["pause_reason"] != nil || a["paused_until"] != nil {
		t.Fatalf("status after resume: %v", accounts)
	}
}

// TestAdminStatusQuota checks that status carries the latest quota readings by window.
func TestAdminStatusQuota(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	defer admin.Close()
	reset := h.clock.Add(time.Hour)
	if err := h.store.RecordQuota(t.Context(), "sample-a", []session.Reading{{Window: session.Window7d, Utilization: 0.42, Status: "allowed", ResetAt: reset, Source: "response"}}); err != nil {
		t.Fatal(err)
	}
	status, body := adminCall(t, admin, http.MethodGet, "/status", "")
	accounts, _ := body["accounts"].([]any)
	a, _ := accounts[0].(map[string]any)
	quota, _ := a["quota"].(map[string]any)
	window, _ := quota["7d"].(map[string]any)
	if status != 200 || window["utilization"] != 0.42 || window["reset_at"] != reset.Format(time.RFC3339) {
		t.Fatalf("status quota: %v", a)
	}
	if _, present := quota["5h"]; present {
		t.Fatal("status reports a window without a reading")
	}
}

// TestAdminReloadFailureShape checks the error answer carries the reload's own message.
func TestAdminReloadFailureShape(t *testing.T) {
	h := newReloadHarness(t, onlyA)
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, func(context.Context) (Reload, error) { return Reload{}, errors.New("synthetic refusal") }).Handler())
	defer admin.Close()
	if status, body := adminCall(t, admin, http.MethodPost, "/reload", ""); status != 422 || body["error"] != "synthetic refusal" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

// TestAdminMachines checks the machine routes: add lists the machine with its limit
// (the default when 0), a second add of the same name is 409, a bad hash is 400, limit
// changes the limit, revoke ends the machine's sessions and answers how many, and an
// unknown name is 404 for revoke and limit.
func TestAdminMachines(t *testing.T) {
	h := newHarness(t, "sample-a")
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	defer admin.Close()
	hash := strings.Repeat("ab", 32)
	status, body := adminCall(t, admin, http.MethodPost, "/machines", `{"name":"client-a","credential_hash":"`+hash+`","max_sessions":0}`)
	if status != 201 || body["name"] != "client-a" {
		t.Fatalf("add: %d %v", status, body)
	}
	if status, body = adminCall(t, admin, http.MethodPost, "/machines", `{"name":"client-a","credential_hash":"`+strings.Repeat("cd", 32)+`"}`); status != 409 {
		t.Fatalf("duplicate add: %d %v", status, body)
	}
	if status, body = adminCall(t, admin, http.MethodPost, "/machines", `{"name":"client-b","credential_hash":"short"}`); status != 400 {
		t.Fatalf("bad hash: %d %v", status, body)
	}
	if status, body = adminCall(t, admin, http.MethodPost, "/machines/client-a/limit", `{"max_sessions":3}`); status != 200 || body["max_sessions"] != float64(3) {
		t.Fatalf("limit: %d %v", status, body)
	}
	if status, body = adminCall(t, admin, http.MethodPost, "/machines/client-a/limit", `{"max_sessions":0}`); status != 400 {
		t.Fatalf("zero limit: %d %v", status, body)
	}
	if status, body = adminCall(t, admin, http.MethodPost, "/machines/client-z/limit", `{"max_sessions":3}`); status != 404 {
		t.Fatalf("limit of unknown: %d %v", status, body)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, admin.URL+"/machines", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := admin.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var machines []session.Machine
	if err := json.NewDecoder(res.Body).Decode(&machines); err != nil || len(machines) != 1 || machines[0].Name != "client-a" || machines[0].MaxSessions != 3 {
		t.Fatalf("list: %+v %v", machines, err)
	}
	_ = res.Body.Close()
	// A session of the machine ends with its revocation; the harness's own session,
	// issued over the socket for test-machine, is untouched (control arm).
	if _, err := h.store.Issue(t.Context(), "client-a", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	if status, body = adminCall(t, admin, http.MethodDelete, "/machines/client-a", ""); status != 200 || body["sessions_ended"] != float64(1) {
		t.Fatalf("revoke: %d %v", status, body)
	}
	if _, err := h.store.Bind(t.Context(), h.token, []string{"sample-a"}); err != nil {
		t.Fatalf("socket session after revoking another machine: %v", err)
	}
	if status, body = adminCall(t, admin, http.MethodDelete, "/machines/client-a", ""); status != 404 {
		t.Fatalf("second revoke: %d %v", status, body)
	}
}

// TestAdminSessions checks GET /sessions: the window and state reach the store, the
// default window is the day before now, and malformed parameters are refused.
func TestAdminSessions(t *testing.T) {
	h := newHarness(t, "sample-a")
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	defer admin.Close()
	window := "since=2026-10-06T11:00:00Z&until=2026-10-06T13:00:00Z"
	status, body := adminCall(t, admin, http.MethodGet, "/sessions?"+window+"&state=all&machine=test-machine&cwd_prefix=", "")
	sessions, _ := body["sessions"].([]any)
	if status != 200 || len(sessions) != 1 || sessions[0].(map[string]any)["id"] != h.sessionID || body["truncated"] != false || body["limit"] != float64(session.MaxRecords) {
		t.Fatalf("all: %d %v", status, body)
	}
	status, body = adminCall(t, admin, http.MethodGet, "/sessions?"+window, "")
	if sessions, _ := body["sessions"].([]any); status != 200 || len(sessions) != 0 || body["ended_only"] != true {
		t.Fatalf("ended only: %d %v", status, body)
	}
	status, body = adminCall(t, admin, http.MethodGet, "/sessions", "")
	since, _ := time.Parse(time.RFC3339, body["since"].(string))
	until, _ := time.Parse(time.RFC3339, body["until"].(string))
	if status != 200 || until.Sub(since) != 24*time.Hour || time.Since(until) > time.Minute {
		t.Fatalf("default window: %d %v", status, body)
	}
	for _, query := range []string{"since=yesterday", "until=2026-10-06", "since=2026-10-06T13:00:00Z&until=2026-10-06T13:00:00Z", "state=live", window + "&cwd_prefix=srv/wt"} {
		if status, body := adminCall(t, admin, http.MethodGet, "/sessions?"+query, ""); status != 400 || body["error"] == nil {
			t.Errorf("%s: %d %v", query, status, body)
		}
	}
}
