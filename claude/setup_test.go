package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// The tokens of the setup tests: the one a login machine hands over, and the one the
// account has before. Neither may reach a log, an audit row, an answer, a table or a
// snapshot.
const (
	setupToken = "sk-ant-oat01-SETUP-NEW-VALUE_0123456789-abc"
	oldToken   = "sk-ant-oat01-synthetic-value-sample-a"
	setupRun   = "20261107T041325123Z"
)

// fakeSecrets is a secret store in memory: one vault, example-vault, with the item of
// sample-a's token. It counts the writes asked of it and can be made to fail.
type fakeSecrets struct {
	mu       sync.Mutex
	vaults   []credential.Vault
	items    []*secretItem // all in the one vault v1
	sets     int
	creates  int
	failSet  bool
	failOpen bool
	// readback, when set, is what a reference to a written item resolves to instead.
	readback string
}

type secretItem struct {
	info    credential.ItemInfo
	fields  []secretField
	updated time.Time
}

type secretField struct{ id, title, value string }

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{
		vaults: []credential.Vault{{ID: "v1", Title: "example-vault"}},
		items: []*secretItem{{
			info:    credential.ItemInfo{ID: "i-a", Title: "token-sample-a"},
			fields:  []secretField{{"credential", "credential", oldToken}, {"notes", "notes", "keep me"}},
			updated: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}},
	}
}

func (f *fakeSecrets) Vaults(context.Context) ([]credential.Vault, error) { return f.vaults, nil }

func (f *fakeSecrets) Items(context.Context, string) ([]credential.ItemInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []credential.ItemInfo
	for _, it := range f.items {
		out = append(out, it.info)
	}
	return out, nil
}

func (f *fakeSecrets) Item(_ context.Context, _, itemID string) (credential.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.info.ID == itemID {
			item := credential.Item{ID: it.info.ID, Title: it.info.Title, UpdatedAt: it.updated}
			for _, field := range it.fields {
				item.Fields = append(item.Fields, credential.Field{ID: field.id, Title: field.title})
			}
			return item, nil
		}
	}
	return credential.Item{}, errors.New("no such item")
}

func (f *fakeSecrets) SetField(_ context.Context, _, itemID string, field credential.Field, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return errors.New("the store refused " + value)
	}
	for _, it := range f.items {
		if it.info.ID != itemID {
			continue
		}
		for i := range it.fields {
			if it.fields[i].id == field.ID && it.fields[i].title == field.Title {
				it.fields[i].value = value
				f.sets++
				return nil
			}
		}
	}
	return errors.New("no such field")
}

func (f *fakeSecrets) Create(_ context.Context, _, title, fieldTitle, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return errors.New("the store refused " + value)
	}
	f.creates++
	f.items = append(f.items, &secretItem{info: credential.ItemInfo{ID: "i-" + title, Title: title}, fields: []secretField{{fieldTitle, fieldTitle, value}}})
	return nil
}

// value resolves an op://vault/item/field reference to what the store holds, ok false
// when it holds nothing there.
func (f *fakeSecrets) value(reference string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.TrimPrefix(reference, "op://"), "/")
	if len(parts) != 3 || parts[0] != "example-vault" {
		return "", false
	}
	for _, it := range f.items {
		if it.info.Title == parts[1] && !it.info.Archived {
			for _, field := range it.fields {
				if field.title == parts[2] {
					if f.readback != "" && field.value != oldToken {
						return f.readback, true
					}
					return field.value, true
				}
			}
		}
	}
	return "", false
}

// setupHarness is a write harness with setup on, a fake secret store behind both the
// resolver and the setup's writes, and the admin interface.
type setupHarness struct {
	*writeHarness
	secrets *fakeSecrets
	admin   http.Handler
	reloads int
	// reloadErr makes the next reloads fail.
	reloadErr error
}

func newSetupHarness(t *testing.T, enabled bool) *setupHarness {
	t.Helper()
	h := &setupHarness{writeHarness: newWriteHarness(t), secrets: newFakeSecrets()}
	resolve := func(ctx context.Context, reference string) (string, error) {
		if value, ok := h.secrets.value(reference); ok {
			return value, nil
		}
		return writeResolve(ctx, reference)
	}
	h.cfg.SetupEnabled = enabled
	h.cfg.SetupSecrets = func(context.Context) (credential.Store, error) {
		if h.secrets.failOpen {
			return nil, errors.New("no client")
		}
		return h.secrets, nil
	}
	h.cfg.Resolve = func(context.Context) (Resolver, error) { return resolve, nil }
	h.cfg.Reload = func(ctx context.Context) (Reload, error) {
		h.reloads++
		if h.reloadErr != nil {
			return Reload{}, h.reloadErr
		}
		return h.accounts.ReloadFrom(ctx, StoreSource(h.store, h.dir), resolve, h.egress.Check)
	}
	h.build()
	h.writer.now = func() time.Time { return h.clock }
	admin := NewAdmin(h.accounts, h.store, nil)
	admin.Setup = h.writer.Setup()
	h.admin = admin.Handler()
	return h
}

// page sends the setup request of the page for alias.
func (h *setupHarness) page(t *testing.T, alias, body string) (int, map[string]any, string) {
	t.Helper()
	return h.serve(t, request("POST", "/accounts/"+alias+"/setup", body))
}

// adminDo sends a request to the admin interface and returns the status and the body.
func (h *setupHarness) adminDo(t *testing.T, method, path, contentType, body string) (int, map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	h.admin.ServeHTTP(recorder, r)
	text := recorder.Body.String()
	if strings.Contains(text, setupToken) {
		t.Fatalf("the token is in an answer: %s", text)
	}
	var answer map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return recorder.Code, answer, text
}

func (h *setupHarness) next(t *testing.T) map[string]any {
	t.Helper()
	status, answer, body := h.adminDo(t, "POST", "/setup/next", "", "")
	if status != 200 {
		t.Fatalf("setup next: %d %s", status, body)
	}
	task, _ := answer["task"].(map[string]any)
	return task
}

func (h *setupHarness) report(t *testing.T, alias, requestID, run, state, step string, reason any) (int, map[string]any, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"request_id": requestID, "run_id": run, "state": state, "step": step, "reason": reason})
	return h.adminDo(t, "POST", "/accounts/"+alias+"/setup/report", "application/json", string(body))
}

func (h *setupHarness) token(t *testing.T, alias, requestID, run, token string) (int, map[string]any, string) {
	t.Helper()
	return h.adminDo(t, "POST", "/accounts/"+alias+"/setup/token?request_id="+requestID+"&run_id="+run, "text/plain", token)
}

// running takes the account through a request and a first report: it returns the request
// ID of a task that is running with the run ID set.
func (h *setupHarness) running(t *testing.T, alias string) string {
	t.Helper()
	row, err := h.store.Account(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	status, answer, body := h.page(t, alias, fmt.Sprintf(`{"action":"login_and_token","rev":%d}`, row.Rev))
	if status != 202 {
		t.Fatalf("setup request: %d %s", status, body)
	}
	id, _ := answer["setup"].(map[string]any)["request_id"].(string)
	if task := h.next(t); task == nil || task["request_id"] != id {
		t.Fatalf("setup next gave %v, want the task %s", task, id)
	}
	if status, _, body := h.report(t, alias, id, setupRun, "running", "lock", nil); status != 200 {
		t.Fatalf("first report: %d %s", status, body)
	}
	return id
}

func (h *setupHarness) setupState(t *testing.T, alias string) session.SetupRow {
	t.Helper()
	rows, err := h.store.SetupRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows[alias]
}

// created makes sample-new through the dashboard, as the wizard does, and returns its
// revision.
func (h *setupHarness) created(t *testing.T) {
	t.Helper()
	if status, _, body := h.serve(t, request("POST", "/accounts", h.newBody())); status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
}

// nothingSent asserts that no quota request went upstream and the secret store was not
// written to.
func (h *setupHarness) nothingSent(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	probes := len(h.probeTokens)
	h.mu.Unlock()
	if probes != 0 || h.secrets.sets != 0 || h.secrets.creates != 0 {
		t.Fatalf("probes=%d sets=%d creates=%d, want none", probes, h.secrets.sets, h.secrets.creates)
	}
}

// TestSetupOff: with setup off every setup route answers 501 before it reads anything,
// the dashboard has no setup state, and a gateway with no write path (Codex) answers
// the page's route the same.
func TestSetupOff(t *testing.T) {
	h := newSetupHarness(t, false)
	for _, r := range []*http.Request{
		request("POST", "/accounts/sample-a/setup", `{"action":"login_and_token","rev":1}`),
		func() *http.Request {
			r := httptest.NewRequest("POST", "/accounts/sample-a/setup", nil)
			r.RemoteAddr, r.Host = "192.0.2.50:4000", writeHost
			return r
		}(),
	} {
		status, answer, _ := h.serve(t, r)
		if status != 501 || answer["error"] != "setup_unavailable" {
			t.Fatalf("page route: %d %v", status, answer)
		}
	}
	for _, c := range []struct{ path, body string }{
		{"/setup/next", ""},
		{"/accounts/sample-a/setup/report", `{}`},
		{"/accounts/sample-a/setup/token?request_id=x&run_id=y", setupToken},
	} {
		status, answer, _ := h.adminDo(t, "POST", c.path, "", c.body)
		if status != 501 || answer["error"] != "setup_unavailable" {
			t.Fatalf("%s: %d %v", c.path, status, answer)
		}
	}
	status, view, _ := h.dashboardJSON(t)
	if status != 200 || view["gateway"].(map[string]any)["setup"] != nil {
		t.Fatalf("gateway.setup = %v", view["gateway"].(map[string]any)["setup"])
	}
	for _, a := range view["accounts"].([]any) {
		if _, has := a.(map[string]any)["setup"]; has {
			t.Fatal("an account has a setup block while setup is off")
		}
	}
	if rows, _ := h.store.SetupRows(t.Context()); len(rows) != 0 {
		t.Fatalf("setup rows while off: %v", rows)
	}
	// No write path at all.
	d := NewDashboard(h.egress, DashboardConfig{Provider: "codex", Reader: nil, Store: h.store})
	recorder := httptest.NewRecorder()
	d.Handler().ServeHTTP(recorder, request("POST", "/accounts/sample-a/setup", `{}`))
	if recorder.Code != 501 {
		t.Fatalf("no write path: %d", recorder.Code)
	}
}

// dashboardJSON reads /dashboard.json from a listed source.
func (h *setupHarness) dashboardJSON(t *testing.T) (int, map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/dashboard.json", nil)
	r.RemoteAddr, r.Host = "192.0.2.50:4000", writeHost
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, r)
	var view map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &view)
	return recorder.Code, view, recorder.Body.String()
}

// TestSetupRequestRoute: the page's request passes the checks of a write and nothing
// else records a task; each refusal leaves the account's setup as it was.
func TestSetupRequestRoute(t *testing.T) {
	h := newSetupHarness(t, true)
	good := `{"action":"login_and_token","rev":1}`
	for _, test := range []struct {
		name   string
		status int
		alter  func(r *http.Request) *http.Request
	}{
		{"source_outside_the_list", 403, func(r *http.Request) *http.Request { r.RemoteAddr = "198.51.100.7:4000"; return r }},
		{"origin_foreign", 403, func(r *http.Request) *http.Request { r.Header.Set("Origin", "http://other.example.test"); return r }},
		{"origin_missing", 403, func(r *http.Request) *http.Request { r.Header.Del("Origin"); return r }},
		{"action_missing", 403, func(r *http.Request) *http.Request { r.Header.Del(QuotaRefreshHeader); return r }},
		{"action_of_refresh", 403, func(r *http.Request) *http.Request { r.Header.Set(QuotaRefreshHeader, "refresh"); return r }},
		{"action_of_activate", 403, func(r *http.Request) *http.Request { r.Header.Set(QuotaRefreshHeader, actionActivate); return r }},
		{"content_type_form", 415, func(r *http.Request) *http.Request {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}},
		{"unknown_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"login_and_token","rev":1,"token":"`+setupToken+`"}`)
		}},
		{"repeated_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"login_and_token","action":"token_only","rev":1}`)
		}},
		{"action_unknown", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"mark_done","rev":1}`)
		}},
		{"action_missing_in_body", 400, func(*http.Request) *http.Request { return request("POST", "/accounts/sample-a/setup", `{"rev":1}`) }},
		{"rev_missing", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"login_and_token"}`)
		}},
		{"rev_stale", 409, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"login_and_token","rev":9}`)
		}},
		{"unknown_account", 404, func(*http.Request) *http.Request {
			return request("POST", "/accounts/nobody/setup", good)
		}},
		{"body_over_8_KiB", 413, func(*http.Request) *http.Request {
			return request("POST", "/accounts/sample-a/setup", `{"action":"`+strings.Repeat("a", 9000)+`"}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := test.alter(request("POST", "/accounts/sample-a/setup", good))
			status, _, body := h.serve(t, r)
			if status != test.status {
				t.Fatalf("status %d, want %d: %s", status, test.status, body)
			}
			if got := h.setupState(t, "sample-a"); got.State != "" {
				t.Fatalf("a refused request left a task: %+v", got)
			}
		})
	}
	// Control: the unaltered request is taken.
	status, answer, body := h.page(t, "sample-a", good)
	block, _ := answer["setup"].(map[string]any)
	if status != 202 || block["state"] != "requested" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(block["request_id"].(string)) || block["action"] != "login_and_token" {
		t.Fatalf("control arm: %d %s", status, body)
	}
	// A second request while one is open, and a request for a retired account, are 409.
	if status, answer, _ := h.page(t, "sample-a", good); status != 409 || answer["error"] != "setup_open" {
		t.Fatalf("second request: %d %v", status, answer)
	}
	if status, _, body := h.serve(t, request("PATCH", "/accounts/sample-spare", `{"rev":1,"status":"retired"}`)); status != 200 {
		t.Fatalf("retire: %d %s", status, body)
	}
	if status, answer, _ := h.page(t, "sample-spare", `{"action":"login_and_token","rev":2}`); status != 409 || answer["error"] != "account_retired" {
		t.Fatalf("retired account: %d %v", status, answer)
	}
}

// TestSetupBrowserRoutesTakeNoToken: the dashboard's mux has no route that takes a token
// or a report, and the request that the page may send has no field for a token.
func TestSetupBrowserRoutesTakeNoToken(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	for _, c := range []struct{ path, body string }{
		{"/accounts/sample-a/setup/token?request_id=" + id + "&run_id=" + setupRun, setupToken},
		{"/accounts/sample-a/setup/report", fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"failed","step":"lock","reason":"account_busy"}`, id, setupRun)},
		{"/setup/next", ""},
	} {
		for _, action := range []string{actionSetup, actionUpdate, actionVerify, actionActivate, "refresh"} {
			r := request("POST", c.path, c.body)
			r.Header.Set(QuotaRefreshHeader, action)
			status, _, body := h.serve(t, r)
			if status != 404 && status != 405 {
				t.Fatalf("%s with %s: %d %s", c.path, action, status, body)
			}
		}
	}
	if got := h.setupState(t, "sample-a"); got.State != "running" || got.Step != "lock" {
		t.Fatalf("a browser route changed the task: %+v", got)
	}
	h.nothingSent(t)
	// The page's body takes no token field.
	if status, _, _ := h.page(t, "sample-new", `{"action":"token_only","rev":1,"token":"`+setupToken+`"}`); status != 400 {
		t.Fatalf("a token field was accepted: %d", status)
	}
}

// TestSetupHandoverCreates: the wizard's path for a new account. The task is requested,
// handed to a login machine, reported on, and the token handed over: the gateway sends
// one quota request with it through the account's exit, creates the item with the one
// field, reloads, and the state block says done with the time and the due date.
func TestSetupHandoverCreates(t *testing.T) {
	h := newSetupHarness(t, true)
	h.created(t)
	row, _ := h.store.Account(t.Context(), "sample-new")
	status, _, body := h.page(t, "sample-new", fmt.Sprintf(`{"action":"login_and_token","rev":%d}`, row.Rev))
	if status != 202 {
		t.Fatalf("request: %d %s", status, body)
	}
	task := h.next(t)
	for key, want := range map[string]any{"alias": "sample-new", "action": "login_and_token", "email": "new@example.test", "device_id": ""} {
		if task[key] != want {
			t.Fatalf("task[%s] = %v, want %v", key, task[key], want)
		}
	}
	proxy := task["proxy"].(map[string]any)
	if proxy["username_ref"] != "op://example-proxies/proxy-account/username" || proxy["password_ref"] != "op://example-proxies/proxy-account/password" || proxy["expected_egress_ip"] != "192.0.2.2" {
		t.Fatalf("task proxy = %v", proxy)
	}
	requestID := task["request_id"].(string)
	if strings.Contains(fmt.Sprint(task), "synthetic-user") || strings.Contains(fmt.Sprint(task), "synthetic-password") {
		t.Fatalf("the task holds a proxy credential: %v", task)
	}
	if again := h.next(t); again != nil {
		t.Fatalf("a second task while one runs: %v", again)
	}
	if status, _, body := h.report(t, "sample-new", requestID, setupRun, "running", "token_read", nil); status != 200 {
		t.Fatalf("report: %d %s", status, body)
	}
	h.clock = h.clock.Add(time.Minute)
	status, answer, body := h.token(t, "sample-new", requestID, setupRun, setupToken)
	if status != 200 || answer["probe"] != "ok" || answer["written"] != true || answer["reload"] != "ok" {
		t.Fatalf("handover: %d %s", status, body)
	}
	block := answer["setup"].(map[string]any)
	if block["state"] != "done" || block["step"] != "complete" || block["reason"] != "complete" || block["token_set_estimated"] != false || block["identity"] != nil {
		t.Fatalf("block = %v", block)
	}
	set, _ := time.Parse(time.RFC3339, block["token_set_at"].(string))
	due, _ := time.Parse(time.RFC3339, block["token_due_at"].(string))
	if !set.Equal(h.clock) || !due.Equal(h.clock.Add(365*24*time.Hour)) {
		t.Fatalf("token_set_at=%v token_due_at=%v", set, due)
	}
	// One quota request, with the new token, through the account's exit.
	h.mu.Lock()
	tokens := append([]string(nil), h.probeTokens...)
	h.mu.Unlock()
	if len(tokens) != 1 || tokens[0] != setupToken {
		t.Fatalf("quota requests carried %v", tokens)
	}
	// The item was created with the one field; nothing else was written.
	if h.secrets.creates != 1 || h.secrets.sets != 0 || len(h.secrets.items) != 2 {
		t.Fatalf("creates=%d sets=%d items=%d", h.secrets.creates, h.secrets.sets, len(h.secrets.items))
	}
	created := h.secrets.items[1]
	if created.info.Title != "token-sample-new" || len(created.fields) != 1 || created.fields[0].title != "credential" || created.fields[0].value != setupToken {
		t.Fatalf("created item = %+v", created)
	}
	if h.reloads != 1 {
		t.Fatalf("reloads = %d", h.reloads)
	}
	// The state block is on the dashboard, in the account's config as well.
	_, view, text := h.dashboardJSON(t)
	if strings.Contains(text, setupToken) {
		t.Fatal("the token is in the dashboard")
	}
	gateway := view["gateway"].(map[string]any)["setup"].(map[string]any)
	if gateway["enabled"] != true || gateway["queued"] != float64(0) || gateway["last_asked_at"] == nil {
		t.Fatalf("gateway.setup = %v", gateway)
	}
	found := false
	for _, a := range view["unserved_accounts"].([]any) {
		if c := a.(map[string]any); c["alias"] == "sample-new" {
			found = c["setup"].(map[string]any)["state"] == "done"
		}
	}
	if !found {
		t.Fatalf("sample-new has no done block in unserved_accounts: %s", text)
	}
	// A second handover for the same task is refused and writes nothing more.
	if status, answer, _ := h.token(t, "sample-new", requestID, setupRun, setupToken); status != 409 || answer["error"] != "no_open_task" {
		t.Fatalf("second handover: %d %v", status, answer)
	}
	h.mu.Lock()
	probes := len(h.probeTokens)
	h.mu.Unlock()
	if probes != 1 || h.secrets.creates != 1 || h.secrets.sets != 0 {
		t.Fatalf("a second handover sent probes=%d creates=%d sets=%d", probes, h.secrets.creates, h.secrets.sets)
	}
}

// TestSetupHandoverSetsExisting: a renewal of a served account sets the one field of the
// item it has, leaves the item's other fields, and the reload serves the new token.
func TestSetupHandoverSetsExisting(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	status, answer, body := h.token(t, "sample-a", id, setupRun, setupToken)
	if status != 200 || answer["written"] != true {
		t.Fatalf("handover: %d %s", status, body)
	}
	if h.secrets.sets != 1 || h.secrets.creates != 0 || len(h.secrets.items) != 1 {
		t.Fatalf("sets=%d creates=%d items=%d", h.secrets.sets, h.secrets.creates, len(h.secrets.items))
	}
	fields := h.secrets.items[0].fields
	if len(fields) != 2 || fields[0].value != setupToken || fields[1].value != "keep me" {
		t.Fatalf("fields = %+v", fields)
	}
	served := h.accounts.set.Load().lookup("sample-a").credential.(claudeCredential).token
	if served != setupToken {
		t.Fatal("the reload did not serve the new token")
	}
	// The probe went out with the new token, not the old one.
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.probeTokens) != 1 || h.probeTokens[0] != setupToken {
		t.Fatalf("probe tokens = %v", h.probeTokens)
	}
}

// TestSetupHandoverRefusals: a handover is taken only for a running task with its request
// ID and the run ID a report set, and a token of the token's shape. Each refusal writes
// and claims nothing, and sends no quota request.
func TestSetupHandoverRefusals(t *testing.T) {
	h := newSetupHarness(t, true)
	// No task at all.
	if status, answer, _ := h.token(t, "sample-a", strings.Repeat("a", 32), setupRun, setupToken); status != 409 || answer["error"] != "no_open_task" {
		t.Fatalf("no task: %d %v", status, answer)
	}
	// Requested, not running.
	row, _ := h.store.Account(t.Context(), "sample-a")
	_, answer, _ := h.page(t, "sample-a", fmt.Sprintf(`{"action":"token_only","rev":%d}`, row.Rev))
	id := answer["setup"].(map[string]any)["request_id"].(string)
	if status, answer, _ := h.token(t, "sample-a", id, setupRun, setupToken); status != 409 || answer["error"] != "no_open_task" {
		t.Fatalf("requested: %d %v", status, answer)
	}
	// Running, no report yet: no run ID to match.
	h.next(t)
	if status, answer, _ := h.token(t, "sample-a", id, setupRun, setupToken); status != 409 || answer["error"] != "ids_mismatch" {
		t.Fatalf("before a report: %d %v", status, answer)
	}
	if status, _, body := h.report(t, "sample-a", id, setupRun, "running", "lock", nil); status != 200 {
		t.Fatalf("report: %d %s", status, body)
	}
	for _, test := range []struct {
		name         string
		alias, id    string
		run          string
		token        string
		status       int
		code         string
		stillRunning bool
	}{
		{"wrong request ID", "sample-a", strings.Repeat("b", 32), setupRun, setupToken, 409, "ids_mismatch", true},
		{"wrong run ID", "sample-a", id, "20261107T041325124Z", setupToken, 409, "ids_mismatch", true},
		{"malformed IDs", "sample-a", "x", "y", setupToken, 400, "bad_request", true},
		{"another account", "sample-spare", id, setupRun, setupToken, 409, "no_open_task", true},
		{"unknown account", "nobody", id, setupRun, setupToken, 404, "unknown_account", true},
		{"empty", "sample-a", id, setupRun, "", 422, "token_shape_invalid", true},
		{"wrong prefix", "sample-a", id, setupRun, "sk-ant-oat02-abc", 422, "token_shape_invalid", true},
		{"trailing newline", "sample-a", id, setupRun, setupToken + "\n", 422, "token_shape_invalid", true},
		{"two lines", "sample-a", id, setupRun, setupToken + "\n" + setupToken, 422, "token_shape_invalid", true},
		{"space inside", "sample-a", id, setupRun, "sk-ant-oat01-abc def", 422, "token_shape_invalid", true},
		{"JSON", "sample-a", id, setupRun, `{"token":"` + setupToken + `"}`, 422, "token_shape_invalid", true},
		{"over 4 KiB", "sample-a", id, setupRun, "sk-ant-oat01-" + strings.Repeat("a", 4084), 422, "token_shape_invalid", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, answer, body := h.token(t, test.alias, test.id, test.run, test.token)
			if status != test.status || answer["error"] != test.code {
				t.Fatalf("%d %s", status, body)
			}
			h.nothingSent(t)
			if got := h.setupState(t, "sample-a"); got.State != "running" || !got.ClaimedAt.IsZero() {
				t.Fatalf("a refusal changed the task: %+v", got)
			}
		})
	}
	// A token of 4096 bytes exactly is of the right shape: control arm of the limit.
	long := "sk-ant-oat01-" + strings.Repeat("a", 4083)
	if len(long) != 4096 {
		t.Fatalf("test token is %d bytes", len(long))
	}
	if status, _, body := h.token(t, "sample-a", id, setupRun, long); status != 200 {
		t.Fatalf("a token of 4096 bytes: %d %s", status, body)
	}
}

// TestSetupHandoverFailures: each step of the handover that fails ends the task as failed
// with its fixed reason, says whether the secret store had the token, and leaves the
// served set as it was; a failed reload comes after the write and leaves the new token in
// the secret store.
func TestSetupHandoverFailures(t *testing.T) {
	cases := []struct {
		name      string
		arrange   func(h *setupHarness)
		status    int
		code      string
		step      string
		written   bool
		probes    int
		wantItems int
	}{
		{"upstream refuses the token (401)", func(h *setupHarness) { h.probeStatus = 401 }, 502, "probe_refused", "probe", false, 1, 1},
		{"upstream refuses the token (403)", func(h *setupHarness) { h.probeStatus = 403 }, 502, "probe_refused", "probe", false, 1, 1},
		{"upstream answers 500", func(h *setupHarness) { h.probeStatus = 500 }, 502, "probe_failed", "probe", false, 1, 1},
		{"upstream answers 429", func(h *setupHarness) { h.probeStatus = 429 }, 502, "probe_failed", "probe", false, 1, 1},
		{"exit is down", func(h *setupHarness) { h.exit.Close() }, 502, "probe_failed", "probe", false, 0, 1},
		{"secret store cannot be opened", func(h *setupHarness) { h.secrets.failOpen = true }, 502, "write_failed", "write", false, 1, 1},
		{"secret store refuses the write", func(h *setupHarness) { h.secrets.failSet = true }, 502, "write_failed", "write", false, 1, 1},
		{"vault is missing", func(h *setupHarness) { h.secrets.vaults = nil }, 500, "write_target_missing", "write", false, 1, 1},
		{"vault matches twice", func(h *setupHarness) {
			h.secrets.vaults = append(h.secrets.vaults, credential.Vault{ID: "v2", Title: "Example-Vault"})
		}, 500, "write_target_ambiguous", "write", false, 1, 1},
		{"item is archived", func(h *setupHarness) { h.secrets.items[0].info.Archived = true }, 500, "write_target_ambiguous", "write", false, 1, 1},
		{"an archived item has the title", func(h *setupHarness) {
			h.secrets.items = append(h.secrets.items, &secretItem{info: credential.ItemInfo{ID: "i-old", Title: "token-sample-a", Archived: true}})
		}, 500, "write_target_ambiguous", "write", false, 1, 2},
		{"two items have the title", func(h *setupHarness) {
			h.secrets.items = append(h.secrets.items, &secretItem{info: credential.ItemInfo{ID: "i-b", Title: "token-sample-a"}})
		}, 500, "write_target_ambiguous", "write", false, 1, 2},
		{"item lacks the field", func(h *setupHarness) { h.secrets.items[0].fields = h.secrets.items[0].fields[1:] }, 500, "write_target_missing", "write", false, 1, 1},
		{"item has the field twice", func(h *setupHarness) {
			h.secrets.items[0].fields = append(h.secrets.items[0].fields, secretField{"credential2", "Credential", "x"})
		}, 500, "write_target_ambiguous", "write", false, 1, 1},
		{"read back differs", func(h *setupHarness) { h.secrets.readback = "sk-ant-oat01-something-else" }, 500, "readback_mismatch", "write", true, 1, 1},
		{"reload fails", func(h *setupHarness) { h.reloadErr = errors.New("synthetic reload failure") }, 500, "reload_failed", "reload", true, 1, 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newSetupHarness(t, true)
			id := h.running(t, "sample-a")
			test.arrange(h)
			h.logs.Reset()
			status, answer, body := h.token(t, "sample-a", id, setupRun, setupToken)
			if status != test.status || answer["error"] != test.code || answer["written"] != test.written {
				t.Fatalf("%d %s", status, body)
			}
			got := h.setupState(t, "sample-a")
			if got.State != "failed" || got.Reason != test.code || got.Step != test.step {
				t.Fatalf("task = %+v", got)
			}
			h.mu.Lock()
			probes := len(h.probeTokens)
			h.mu.Unlock()
			if probes != test.probes {
				t.Fatalf("quota requests = %d, want %d", probes, test.probes)
			}
			if test.written != (h.secrets.sets == 1) && test.code != "write_failed" {
				t.Fatalf("written = %v with sets = %d", test.written, h.secrets.sets)
			}
			if !test.written && (h.secrets.sets != 0 || h.secrets.creates != 0) {
				t.Fatalf("sets=%d creates=%d after a failure before the write", h.secrets.sets, h.secrets.creates)
			}
			if len(h.secrets.items) != test.wantItems {
				t.Fatalf("items = %d, want %d", len(h.secrets.items), test.wantItems)
			}
			// The served set keeps the old token; token_ref is as it was.
			if served := h.accounts.set.Load().lookup("sample-a").credential.(claudeCredential).token; served != oldToken {
				t.Fatalf("the served token changed after a failure")
			}
			row, _ := h.store.Account(t.Context(), "sample-a")
			if row.TokenRef != "op://example-vault/token-sample-a/credential" {
				t.Fatalf("token_ref = %s", row.TokenRef)
			}
			if strings.Contains(h.logs.String(), setupToken) {
				t.Fatalf("the token is in the log: %s", h.logs.String())
			}
			// The task is closed: a second handover is refused, and a new task can be asked for.
			if status, answer, _ := h.token(t, "sample-a", id, setupRun, setupToken); status != 409 || answer["error"] != "no_open_task" {
				t.Fatalf("handover after the failure: %d %v", status, answer)
			}
		})
	}
}

// TestSetupWritesOnlyToTheTemplateReference: a token reference that is not what the
// template gives for the alias is never written to, and an item is never created for it;
// with no template nothing is written. The check comes before the quota request.
func TestSetupWritesOnlyToTheTemplateReference(t *testing.T) {
	for _, test := range []struct {
		name  string
		tweak func(h *setupHarness)
	}{
		{"reference of an existing item that is not the template's", func(h *setupHarness) {
			h.secrets.items = append(h.secrets.items, &secretItem{info: credential.ItemInfo{ID: "i-x", Title: "someone-elses"}, fields: []secretField{{"credential", "credential", "other value"}}})
			h.setTokenRef(t, "sample-a", "op://example-vault/someone-elses/credential")
		}},
		{"reference of a missing item that is not the template's", func(h *setupHarness) {
			h.setTokenRef(t, "sample-a", "op://example-vault/new-title/credential")
		}},
		{"another field of the right item", func(h *setupHarness) { h.setTokenRef(t, "sample-a", "op://example-vault/token-sample-a/notes") }},
		{"another vault", func(h *setupHarness) { h.setTokenRef(t, "sample-a", "op://other-vault/token-sample-a/credential") }},
		{"item of another alias", func(h *setupHarness) { h.setTokenRef(t, "sample-a", "op://example-vault/token-sample-b/credential") }},
		{"an env reference", func(h *setupHarness) { h.setTokenRef(t, "sample-a", "env://SYNTHETIC_TOKEN") }},
		{"no template is set", func(h *setupHarness) { h.writer.cfg.TokenRefTemplate = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSetupHarness(t, true)
			id := h.running(t, "sample-a")
			test.tweak(h)
			before := fmt.Sprint(h.secrets.items[0].fields)
			status, answer, body := h.token(t, "sample-a", id, setupRun, setupToken)
			if status != 422 || answer["error"] != "write_target_not_allowed" || answer["written"] != false {
				t.Fatalf("%d %s", status, body)
			}
			h.nothingSent(t)
			if fmt.Sprint(h.secrets.items[0].fields) != before || len(h.secrets.items) > 2 {
				t.Fatalf("the secret store changed: %+v", h.secrets.items)
			}
			if got := h.setupState(t, "sample-a"); got.State != "failed" || got.Reason != "write_target_not_allowed" {
				t.Fatalf("task = %+v", got)
			}
		})
	}
	// Control: with the template's reference the same handover is taken.
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	if status, _, body := h.token(t, "sample-a", id, setupRun, setupToken); status != 200 {
		t.Fatalf("control arm: %d %s", status, body)
	}
}

// setTokenRef sets an account's token reference in the table, as an operator's import
// could have left it.
func (h *setupHarness) setTokenRef(t *testing.T, alias, reference string) {
	t.Helper()
	row, err := h.store.Account(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateAccountKeepRev(t.Context(), alias, row.Rev, session.AuditEntry{Action: "test"}, func(r *session.AccountRow) { r.TokenRef = reference }); err != nil {
		t.Fatal(err)
	}
}

// TestSetupReports: a report takes one of three states with its step and reason from the
// fixed sets; anything else is refused with its own code and changes nothing, and a task
// that a report closed blocks no new one.
func TestSetupReports(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	other := strings.Repeat("c", 32)
	for _, test := range []struct {
		name         string
		body         string
		status       int
		code         string
		alias        string
		useRawBody   bool
		rawContentTy string
	}{
		{name: "state done", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"done","step":"complete","reason":null}`, id, setupRun), status: 400, code: "state_invalid", alias: "sample-a"},
		{name: "state unknown", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"requested","step":"lock","reason":null}`, id, setupRun), status: 400, code: "state_invalid", alias: "sample-a"},
		{name: "step unknown", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"free text about a page","reason":null}`, id, setupRun), status: 400, code: "step_invalid", alias: "sample-a"},
		{name: "a gateway step", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"write","reason":null}`, id, setupRun), status: 400, code: "step_invalid", alias: "sample-a"},
		{name: "running with a reason", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":"account_busy"}`, id, setupRun), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "failed with free text", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"failed","step":"lock","reason":"%s"}`, id, setupRun, setupToken), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "failed with a gateway reason", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"failed","step":"lock","reason":"complete"}`, id, setupRun), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "failed without a reason", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"failed","step":"lock","reason":null}`, id, setupRun), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "needs_human with another reason", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"needs_human","step":"lock","reason":"account_busy"}`, id, setupRun), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "failed with a human reason", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"failed","step":"email_page","reason":"captcha_challenge"}`, id, setupRun), status: 400, code: "reason_invalid", alias: "sample-a"},
		{name: "unknown key", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":null,"token":"%s"}`, id, setupRun, setupToken), status: 400, code: "bad_request", alias: "sample-a"},
		{name: "repeated key", body: fmt.Sprintf(`{"request_id":"%s","request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":null}`, id, id, setupRun), status: 400, code: "bad_request", alias: "sample-a"},
		{name: "run ID malformed", body: fmt.Sprintf(`{"request_id":"%s","run_id":"x","state":"running","step":"lock","reason":null}`, id), status: 400, code: "bad_request", alias: "sample-a"},
		{name: "over 4 KiB", body: `{"request_id":"` + id + `","pad":"` + strings.Repeat("a", 4100) + `"}`, status: 400, code: "bad_request", alias: "sample-a"},
		{name: "unknown account", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":null}`, id, setupRun), status: 404, code: "unknown_account", alias: "nobody"},
		{name: "another request ID", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":null}`, other, setupRun), status: 409, code: "ids_mismatch", alias: "sample-a"},
		{name: "another run ID", body: fmt.Sprintf(`{"request_id":"%s","run_id":"20261107T041325124Z","state":"running","step":"lock","reason":null}`, id), status: 409, code: "ids_mismatch", alias: "sample-a"},
		{name: "account with no task", body: fmt.Sprintf(`{"request_id":"%s","run_id":"%s","state":"running","step":"lock","reason":null}`, id, setupRun), status: 409, code: "no_open_task", alias: "sample-spare"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := h.setupState(t, "sample-a")
			status, answer, body := h.adminDo(t, "POST", "/accounts/"+test.alias+"/setup/report", "application/json", test.body)
			if status != test.status || answer["error"] != test.code {
				t.Fatalf("%d %s", status, body)
			}
			if after := h.setupState(t, "sample-a"); after != before {
				t.Fatalf("a refused report changed the task: %+v -> %+v", before, after)
			}
		})
	}
	// Accepted: a step, then the three closing states.
	if status, _, body := h.report(t, "sample-a", id, setupRun, "running", "email_sent", nil); status != 200 {
		t.Fatalf("running: %d %s", status, body)
	}
	status, answer, body := h.report(t, "sample-a", id, setupRun, "needs_human", "email_page", "captcha_challenge")
	block, _ := answer["setup"].(map[string]any)
	if status != 200 || block["state"] != "needs_human" || block["reason"] != "captcha_challenge" || block["step"] != "email_page" || block["run_id"] != setupRun {
		t.Fatalf("needs_human: %d %s", status, body)
	}
	// A closed task takes no report, and blocks no new request.
	if status, answer, _ := h.report(t, "sample-a", id, setupRun, "running", "lock", nil); status != 409 || answer["error"] != "no_open_task" {
		t.Fatalf("report to a closed task: %d %v", status, answer)
	}
	row, _ := h.store.Account(t.Context(), "sample-a")
	if status, _, body := h.page(t, "sample-a", fmt.Sprintf(`{"action":"token_only","rev":%d}`, row.Rev)); status != 202 {
		t.Fatalf("a new request after needs_human: %d %s", status, body)
	}
	next := h.next(t)
	if next == nil || next["action"] != "token_only" {
		t.Fatalf("next = %v", next)
	}
	if status, _, _ := h.report(t, "sample-a", next["request_id"].(string), "20261107T050000000Z", "failed", "token_cli", "token_cli_failed"); status != 200 {
		t.Fatal("failed report refused")
	}
}

// TestSetupNothingKeepsTheToken: after a success and a failure, the token is in no log
// line, audit row, answer, setup row, table, database file or snapshot; the references
// and the old token's absence are the control arm.
func TestSetupNothingKeepsTheToken(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	h.probeStatus = 500
	h.token(t, "sample-a", id, setupRun, setupToken) // fails at the probe
	h.probeStatus = 200
	row, _ := h.store.Account(t.Context(), "sample-a")
	h.page(t, "sample-a", fmt.Sprintf(`{"action":"token_only","rev":%d}`, row.Rev))
	next := h.next(t)
	id = next["request_id"].(string)
	h.report(t, "sample-a", id, setupRun, "running", "token_read", nil)
	if status, _, body := h.token(t, "sample-a", id, setupRun, setupToken); status != 200 {
		t.Fatalf("handover: %d %s", status, body)
	}
	_, _, dashboard := h.dashboardJSON(t)
	audit, _ := h.store.AccountAudit(t.Context(), "sample-a")
	encoded, _ := json.Marshal(audit)
	rows, _ := h.store.SetupRows(t.Context())
	setupRows, _ := json.Marshal(rows)
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := h.store.VacuumInto(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for name, path := range map[string]string{"snapshot": snapshot, "database": h.path, "wal": h.path + "-wal"} {
		if data, err := os.ReadFile(path); err == nil {
			files[name] = string(data)
		}
	}
	for name, text := range map[string]string{"log": h.logs.String(), "audit": string(encoded), "setup rows": string(setupRows), "dashboard": dashboard, "snapshot": files["snapshot"], "database": files["database"], "wal": files["wal"]} {
		if strings.Contains(text, setupToken) {
			t.Fatalf("the token is in the %s", name)
		}
	}
	if !strings.Contains(files["snapshot"], "op://example-vault/token-sample-a/credential") || !strings.Contains(h.logs.String(), "setup alias=sample-a") || !strings.Contains(string(encoded), "account-setup-token") {
		t.Fatal("control arm: the snapshot has no reference, or the log or audit has no setup line")
	}
}

// TestSetupSweep: the hourly sweep dates a token the gateway did not write from the
// secret store's item and marks it an estimate, queues a renewal when it is due within 30
// days and /health names the account within 7 days; a token the gateway wrote is not
// re-dated.
func TestSetupSweep(t *testing.T) {
	h := newSetupHarness(t, true)
	setup := h.writer.Setup()
	// The item of sample-a was updated 340 days before the clock.
	h.secrets.items[0].updated = h.clock.Add(-340 * 24 * time.Hour)
	setup.sweep(t.Context())
	_, view, _ := h.dashboardJSON(t)
	var block map[string]any
	for _, a := range view["accounts"].([]any) {
		if c := a.(map[string]any); c["alias"] == "sample-a" {
			block = c["setup"].(map[string]any)
		}
	}
	if block["state"] != "requested" || block["action"] != "token_only" || block["token_set_estimated"] != true || block["token_due_at"] == nil {
		t.Fatalf("block after the sweep = %v", block)
	}
	if got := view["gateway"].(map[string]any)["setup"].(map[string]any)["queued"]; got != float64(1) {
		t.Fatalf("queued = %v", got)
	}
	if problems := setup.Problems(); len(problems) != 0 {
		t.Fatalf("problems 25 days before the due date: %v", problems)
	}
	h.clock = h.clock.Add(19 * 24 * time.Hour)
	want := "sample-a: token due " + h.clock.Add(-340*24*time.Hour).Add(-19*24*time.Hour).Add(365*24*time.Hour).Format("2006-01-02")
	if problems := setup.Problems(); len(problems) != 1 || problems[0] != want {
		t.Fatalf("problems = %v, want [%s]", problems, want)
	}
	// An unreadable secret store dates nothing and queues nothing wrongly.
	h2 := newSetupHarness(t, true)
	h2.secrets.failOpen = true
	h2.writer.Setup().sweep(t.Context())
	if rows, _ := h2.store.SetupRows(t.Context()); len(rows) != 0 {
		t.Fatalf("rows with the secret store down: %v", rows)
	}
}

// TestSetupStart: a handover that was under way when the process stopped ends as
// interrupted at the next start, and an unclaimed task is left.
func TestSetupStart(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	if err := h.writer.Setup().Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.setupState(t, "sample-a"); got.State != "running" {
		t.Fatalf("an unclaimed task was ended: %+v", got)
	}
	if _, err := h.store.ClaimHandover(t.Context(), "sample-a", id, setupRun); err != nil {
		t.Fatal(err)
	}
	if err := h.writer.Setup().Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.setupState(t, "sample-a"); got.State != "failed" || got.Reason != "interrupted" {
		t.Fatalf("a claimed task at start: %+v", got)
	}
}

// TestSetupNextWhenIdle: with nothing queued the answer is {"task": null}, and asking
// notes the time for gateway.setup.
func TestSetupNextWhenIdle(t *testing.T) {
	h := newSetupHarness(t, true)
	status, answer, body := h.adminDo(t, "POST", "/setup/next", "", "")
	if status != 200 || answer["task"] != nil || !strings.Contains(body, `"task":null`) {
		t.Fatalf("%d %s", status, body)
	}
	_, view, _ := h.dashboardJSON(t)
	if got := view["gateway"].(map[string]any)["setup"].(map[string]any)["last_asked_at"]; got == nil {
		t.Fatal("gateway.setup.last_asked_at is null after a login machine asked")
	}
}

// TestSetupTaskEndedBeforeTheWrite: a task that the store ended between the quota request
// and the write (a new process's start during a handoff, or the handover's 5 minutes)
// gets no write and no success answer.
func TestSetupTaskEndedBeforeTheWrite(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	h.cfg.SetupSecrets = func(context.Context) (credential.Store, error) {
		if n, err := h.store.InterruptSetups(t.Context()); err != nil || n != 1 {
			t.Fatalf("interrupt: n=%d err=%v", n, err)
		}
		return h.secrets, nil
	}
	h.writer.cfg.SetupSecrets = h.cfg.SetupSecrets
	status, answer, body := h.token(t, "sample-a", id, setupRun, setupToken)
	if status != 409 || answer["error"] != "no_open_task" || answer["written"] != false {
		t.Fatalf("%d %s", status, body)
	}
	if h.secrets.sets != 0 || h.secrets.creates != 0 {
		t.Fatalf("sets=%d creates=%d for an ended task", h.secrets.sets, h.secrets.creates)
	}
	if got := h.setupState(t, "sample-a"); got.State != "failed" || got.Reason != "interrupted" || !got.TokenSetAt.IsZero() {
		t.Fatalf("task = %+v", got)
	}
}

// TestSetupDatesATokenSetByHand: an item changed by hand after the gateway wrote its
// token dates the token, so no renewal is queued for a token that was just set; the
// gateway's own write is not taken for a hand edit.
func TestSetupDatesATokenSetByHand(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	if status, _, body := h.token(t, "sample-a", id, setupRun, setupToken); status != 200 {
		t.Fatalf("handover: %d %s", status, body)
	}
	written := h.setupState(t, "sample-a").TokenSetAt
	// The store dates the item at the write: nothing changes.
	h.secrets.items[0].updated = written.Add(time.Second)
	h.writer.Setup().sweep(t.Context())
	if got := h.setupState(t, "sample-a"); !got.TokenSetAt.Equal(written) || got.TokenSetEstimated {
		t.Fatalf("the gateway's write was re-dated: %+v", got)
	}
	// 340 days later the operator sets the token by hand.
	h.clock = h.clock.Add(340 * 24 * time.Hour)
	h.secrets.items[0].updated = h.clock
	h.writer.Setup().sweep(t.Context())
	got := h.setupState(t, "sample-a")
	if !got.TokenSetAt.Equal(h.clock) || !got.TokenSetEstimated || got.State == "requested" {
		t.Fatalf("a hand-set token was not dated: %+v", got)
	}
}

// TestSetupTaskEndedAfterTheWrite: a task that the store ended after the token was
// written (a restart during the reload, or the handover's 5 minutes) is not answered with
// success; the answer says the token is in the secret store.
func TestSetupTaskEndedAfterTheWrite(t *testing.T) {
	h := newSetupHarness(t, true)
	id := h.running(t, "sample-a")
	reload := h.writer.cfg.Reload
	h.writer.cfg.Reload = func(ctx context.Context) (Reload, error) {
		if n, err := h.store.InterruptSetups(t.Context()); err != nil || n != 1 {
			t.Fatalf("interrupt: n=%d err=%v", n, err)
		}
		return reload(ctx)
	}
	status, answer, body := h.token(t, "sample-a", id, setupRun, setupToken)
	if status != 409 || answer["error"] != "no_open_task" || answer["written"] != true {
		t.Fatalf("%d %s", status, body)
	}
	if h.secrets.sets != 1 || h.secrets.creates != 0 {
		t.Fatalf("sets=%d creates=%d", h.secrets.sets, h.secrets.creates)
	}
	if got := h.setupState(t, "sample-a"); got.State != "failed" || got.Reason != "interrupted" {
		t.Fatalf("task = %+v", got)
	}
}
