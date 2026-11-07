package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// writeResolve resolves the references the write tests use to made-up values: the proxy
// login of the fixture's exits, and a token per alias; any other reference fails.
func writeResolve(_ context.Context, reference string) (string, error) {
	switch reference {
	case "op://example-proxies/proxy-account/username":
		return "synthetic-user-1", nil
	case "op://example-proxies/proxy-account/password":
		return "synthetic-password-1", nil
	}
	if alias, ok := strings.CutPrefix(reference, "op://example-vault/token-"); ok {
		if alias, ok := strings.CutSuffix(alias, "/credential"); ok {
			return "sk-ant-oat01-synthetic-value-" + alias, nil
		}
	}
	return "", errors.New("synthetic resolver: unknown reference")
}

// writeValues are values the resolver gives out; none may reach a log, an audit row, a
// stored row or an answer.
var writeValues = []string{"synthetic-user-1", "synthetic-password-1", "sk-ant-oat01-synthetic-value-"}

// writeHarness is a gateway with one served account, a fake exit that tunnels to the
// echo service and answers the quota probe itself, and the dashboard's write routes
// over them behind the Host guard.
// logBuffer is the log sink of the tests; server goroutines write to it while the
// test reads, so every access takes the lock.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *logBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

type writeHarness struct {
	*harness
	echo    *echoService
	dir     string
	writer  *AccountWriter
	egress  *Egress
	handler http.Handler
	logs    *logBuffer
	cfg     AccountWriterConfig

	mu          sync.Mutex
	probeStatus int
	probeTokens []string
	exit        *httptest.Server
}

const (
	writeHost   = "gateway.example.test"
	writeOrigin = "http://" + writeHost
)

// newWriteHarness imports a directory of sample-a (served, at the fake exit, expected
// IP 192.0.2.1) and sample-spare (testing, with a proxy login nothing can resolve),
// serves sample-a on the pro plan and lets writes from 192.0.2.0/24 through.
func newWriteHarness(t *testing.T, allow ...netip.Prefix) *writeHarness {
	t.Helper()
	w := &writeHarness{harness: newHarness(t), echo: newEchoService(t, "192.0.2.1"), probeStatus: http.StatusOK, logs: &logBuffer{}}
	log.SetOutput(w.logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	w.exit = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.echo.proxy.Config.Handler.ServeHTTP(rw, r)
			return
		}
		if user, _, ok := basicUser(r.Header.Get("Proxy-Authorization")); !ok || user != "synthetic-user-1" {
			http.Error(rw, "proxy credentials", http.StatusProxyAuthRequired)
			return
		}
		w.mu.Lock()
		w.probeTokens = append(w.probeTokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		status := w.probeStatus
		w.mu.Unlock()
		rw.WriteHeader(status)
		_, _ = io.WriteString(rw, `{}`)
	}))
	t.Cleanup(w.exit.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(w.exit.URL, "http://"))
	w.dir = writeInventory(t, map[string]string{
		"sample-a.yaml": strings.Replace(inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", port), "host: proxy.example.test", "host: "+host, 1),
		"sample-spare.yaml": strings.NewReplacer("op://example-proxies/proxy-account/", "op://example-proxies/spare-account/", "192.0.2.1", "192.0.2.77").
			Replace(inventoryYAML("sample-spare", "testing", "gateway", "op://example-vault/token-sample-spare/credential", "9") + "device_id: " + strings.Repeat("a", 64) + "\n"),
	})
	importFixture(t, w.harness, w.dir)
	set, err := LoadAccountsFrom(t.Context(), StoreSource(w.store, w.dir), Claude, writeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	w.accounts.set.Store(set)
	if err := w.store.SetPlanSchedule(t.Context(), session.PlanPeriod{Alias: "sample-a", Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	w.egress = NewEgress(w.accounts, w.store, testUpstream)
	w.egress.rootCAs = w.echo.roots
	upstream, _ := url.Parse("http://upstream.example.test")
	entry, err := NewHandler(upstream, Claude, "", w.store, w.accounts, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(allow) == 0 {
		allow = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	}
	w.cfg = AccountWriterConfig{
		Store: w.store, Accounts: w.accounts, Egress: w.egress, Entrypoint: entry,
		Reload: func(ctx context.Context) (Reload, error) {
			return w.accounts.ReloadFrom(ctx, StoreSource(w.store, w.dir), writeResolve, w.egress.Check)
		},
		Resolve:          func(context.Context) (Resolver, error) { return writeResolve, nil },
		AllowFrom:        allow,
		TokenRefTemplate: "op://example-vault/token-{alias}/credential",
	}
	w.build()
	return w
}

// build makes the write path and the dashboard over w.cfg.
func (w *writeHarness) build() {
	t := w.t
	w.writer = NewAccountWriter(w.cfg)
	reader, err := session.OpenReadOnly(w.path, session.Config{Now: func() time.Time { return w.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	d := NewDashboard(w.egress, DashboardConfig{Provider: "claude", Reader: reader, Store: w.store, DBPath: w.path, Started: w.clock, Write: w.writer})
	w.handler = HostGuard([]string{writeHost}, d.Handler())
}

// actionOf is the action name a route's requests carry.
func actionOf(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/accounts":
		return actionCreate
	case method == http.MethodPatch:
		return actionUpdate
	case strings.HasSuffix(path, "/verify"):
		return actionVerify
	case strings.HasSuffix(path, "/setup"):
		return actionSetup
	}
	return actionActivate
}

// request builds a request that passes every check of a write: from an address on the
// list, to the gateway's host, with its own origin, the action header and a JSON body.
func request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr, r.Host = "192.0.2.50:4000", writeHost
	r.Header.Set("Origin", writeOrigin)
	r.Header.Set(QuotaRefreshHeader, actionOf(method, path))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// serve sends r and returns the status and the answer, whose body is also checked to
// hold no credential value.
func (w *writeHarness) serve(t *testing.T, r *http.Request) (int, map[string]any, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	w.handler.ServeHTTP(recorder, r)
	body := recorder.Body.String()
	for _, value := range writeValues {
		if strings.Contains(body, value) {
			t.Fatalf("the answer holds a credential value: %s", body)
		}
	}
	var answer map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return recorder.Code, answer, body
}

// state is the accounts table and the audit rows of the fixture's aliases, as text.
func (w *writeHarness) state(t *testing.T) string {
	t.Helper()
	rows, _, err := w.store.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(rows)
	audits := ""
	for _, alias := range []string{"sample-a", "sample-spare", "sample-new"} {
		rows, err := w.store.AccountAudit(t.Context(), alias)
		if err != nil {
			t.Fatal(err)
		}
		audits += alias + ":" + string(rune('0'+len(rows))) + " "
	}
	return string(data) + audits
}

// exitAddress is the host and port of the fake exit.
func (w *writeHarness) exitAddress() (host, port string) {
	host, port, _ = net.SplitHostPort(strings.TrimPrefix(w.exit.URL, "http://"))
	return host, port
}

// testDeviceID is a made-up device ID; testDeviceID2 is another.
const (
	testDeviceID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDeviceID2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// newBody is a create request that passes: a made-up account behind the fixture's exit,
// with the fixture's proxy login.
func (w *writeHarness) newBody() string {
	host, port := w.exitAddress()
	return `{"alias":"sample-new","email":"new@example.test","access":"gateway","device_id":"` + testDeviceID + `","plan":"pro","proxy":{"host":"` + host + `","port":` + port + `,"expected_egress_ip":"192.0.2.2","username_ref":"op://example-proxies/proxy-account/username","password_ref":"op://example-proxies/proxy-account/password"}}`
}

// bodyFor is newBody for another alias and another device ID.
func (w *writeHarness) bodyFor(alias string) string {
	return strings.Replace(strings.Replace(w.newBody(), "sample-new", alias, 1), testDeviceID, testDeviceID2, 1)
}

// TestWriteRouteChecks checks each check the write routes run before they read a field:
// the request of newBody passes them all, and the same request with one thing wrong is
// refused, with the status of that check, and writes nothing. Credential values, even
// pasted into the request, are in no answer and no log line.
func TestWriteRouteChecks(t *testing.T) {
	w := newWriteHarness(t)
	_, port := w.exitAddress()
	const pasted = "sk-ant-oat01-PASTED-VALUE"
	bigBody := `{"alias":"sample-new","email":"` + strings.Repeat("a", 9000) + `"}`
	for _, test := range []struct {
		name   string
		status int
		alter  func(r *http.Request) *http.Request
	}{
		{"source_outside_the_list", 403, func(r *http.Request) *http.Request { r.RemoteAddr = "198.51.100.7:4000"; return r }},
		{"forwarded_for_is_not_trusted", 403, func(r *http.Request) *http.Request {
			r.RemoteAddr = "198.51.100.7:4000"
			r.Header.Set("X-Forwarded-For", "192.0.2.50")
			r.Header.Set("Forwarded", "for=192.0.2.50")
			return r
		}},
		{"host_not_the_gateways", 403, func(r *http.Request) *http.Request { r.Host = "rebound.example.test"; return r }},
		{"origin_missing", 403, func(r *http.Request) *http.Request { r.Header.Del("Origin"); return r }},
		{"origin_foreign", 403, func(r *http.Request) *http.Request { r.Header.Set("Origin", "http://other.example.test"); return r }},
		{"origin_null", 403, func(r *http.Request) *http.Request { r.Header.Set("Origin", "null"); return r }},
		{"origin_twice", 403, func(r *http.Request) *http.Request { r.Header.Add("Origin", writeOrigin); return r }},
		{"action_missing", 403, func(r *http.Request) *http.Request { r.Header.Del(QuotaRefreshHeader); return r }},
		{"action_of_another_route", 403, func(r *http.Request) *http.Request { r.Header.Set(QuotaRefreshHeader, actionUpdate); return r }},
		{"action_twice", 403, func(r *http.Request) *http.Request { r.Header.Add(QuotaRefreshHeader, actionCreate); return r }},
		{"content_type_missing", 415, func(r *http.Request) *http.Request { r.Header.Del("Content-Type"); return r }},
		{"content_type_form", 415, func(r *http.Request) *http.Request {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}},
		{"body_over_8_KiB", 413, func(*http.Request) *http.Request { return request("POST", "/accounts", bigBody) }},
		{"unknown_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"alias"`, `"token":"`+pasted+`","alias"`, 1))
		}},
		{"unknown_nested_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"host"`, `"`+pasted+`":1,"host"`, 1))
		}},
		{"repeated_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"plan":"pro"`, `"plan":"pro","plan":"pro"`, 1))
		}},
		{"repeated_nested_key", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"port":`+port, `"port":`+port+`,"port":`+port, 1))
		}},
		{"repeated_key_other_case", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"plan":"pro"`, `"plan":"pro","PLAN":"pro"`, 1))
		}},
		{"repeated_nested_key_other_case", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), `"port":`+port, `"port":`+port+`,"PORT":`+port, 1))
		}},
		{"content_after_the_object", 400, func(*http.Request) *http.Request { return request("POST", "/accounts", w.newBody()+w.newBody()) }},
		{"not_an_object", 400, func(*http.Request) *http.Request { return request("POST", "/accounts", `["x"]`) }},
		{"value_where_an_email_goes", 400, func(*http.Request) *http.Request {
			return request("POST", "/accounts", strings.Replace(w.newBody(), "new@example.test", pasted, 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w.logs.Reset()
			before := w.state(t)
			status, _, body := w.serve(t, test.alter(request("POST", "/accounts", w.newBody())))
			if status != test.status {
				t.Fatalf("status %d, want %d: %s", status, test.status, body)
			}
			if after := w.state(t); after != before {
				t.Fatal("a refused write changed the table or the audit")
			}
			if strings.Contains(body, "PASTED") || strings.Contains(w.logs.String(), "PASTED") {
				t.Fatalf("the pasted value is in the answer or the log: %s", body)
			}
			// A refusal of a listed source is logged by the write path; one of another source
			// is not, and the Host guard's own refusals are not either.
			logged := strings.Contains(w.logs.String(), "account write refused ip=")
			notLogged := strings.HasPrefix(test.name, "source_") || strings.HasPrefix(test.name, "forwarded_") || strings.HasPrefix(test.name, "host_") ||
				test.name == "origin_foreign" || test.name == "origin_null" || test.name == "origin_twice"
			if notLogged == logged {
				t.Fatalf("logged = %v for a refusal of the %s case: %q", logged, test.name, w.logs.String())
			}
		})
	}
	// Control: the unaltered request passes every check and creates the account.
	if status, _, body := w.serve(t, request("POST", "/accounts", w.newBody())); status != 200 {
		t.Fatalf("control arm: status %d: %s", status, body)
	}
}

// TestWritesAreOffByDefault checks that with no source address listed every write route
// answers 403 "writes disabled" and writes nothing, and that the same request with an
// address listed passes.
func TestWritesAreOffByDefault(t *testing.T) {
	w := newWriteHarness(t)
	w.writer.cfg.AllowFrom = nil
	before := w.state(t)
	for _, r := range []*http.Request{
		request("POST", "/accounts", w.newBody()),
		request("PATCH", "/accounts/sample-a", `{"rev":1,"email":"x@example.test"}`),
		request("POST", "/accounts/sample-a/verify", `{}`),
		request("POST", "/accounts/sample-spare/activate", `{"rev":1}`),
	} {
		status, answer, _ := w.serve(t, r)
		if status != 403 || answer["error"] != "writes disabled" {
			t.Fatalf("%s %s: %d %v", r.Method, r.URL.Path, status, answer)
		}
	}
	if w.state(t) != before {
		t.Fatal("a disabled write changed something")
	}
	w.writer.cfg.AllowFrom = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	if status, _, body := w.serve(t, request("POST", "/accounts", w.newBody())); status != 200 {
		t.Fatalf("control arm: %d %s", status, body)
	}
}

// TestCreateAccount checks a create: the account is stored as testing with a token
// reference built from the template, its plan is set, the audit row holds the new
// values, the log line only the field names, and nothing is served yet. Refused: an
// alias that is no alias, a taken one, a proxy login no account uses, an unknown plan,
// a body without the template configured.
func TestCreateAccount(t *testing.T) {
	w := newWriteHarness(t)
	host, port := w.exitAddress()
	status, answer, body := w.serve(t, request("POST", "/accounts", w.newBody()))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	account := answer["account"].(map[string]any)
	if account["status"] != "testing" || account["token_ref"] != "op://example-vault/token-sample-new/credential" || account["rev"] != float64(1) {
		t.Fatalf("account = %v", account)
	}
	if _, err := w.store.QuotaUnit(t.Context(), "sample-new", time.Now()); err != nil {
		t.Fatalf("plan not set: %v", err)
	}
	if slices.Contains(w.accounts.Candidates(), "sample-new") {
		t.Fatal("a testing account is served")
	}
	audit, err := w.store.AccountAudit(t.Context(), "sample-new")
	if err != nil || len(audit) != 2 || audit[1].Changes["plan"] != (session.FieldChange{Old: "", New: "pro"}) || audit[0].Action != actionCreate || audit[0].SourceIP != "192.0.2.50" ||
		audit[0].Changes["email"] != (session.FieldChange{Old: "", New: "new@example.test"}) || audit[0].Changes["status"].New != "testing" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if !strings.Contains(w.logs.String(), "account write ip=192.0.2.50 action=account-create alias=sample-new result=ok fields=") || strings.Contains(w.logs.String(), "new@example.test") {
		t.Fatalf("log = %q", w.logs.String())
	}

	for _, test := range []struct {
		name, body, field string
		status            int
	}{
		{"taken", w.newBody(), "alias", 409},
		{"missing_device_id", strings.Replace(w.bodyFor("sample-two"), `"device_id":"`+testDeviceID2+`",`, "", 1), "device_id", 400},
		{"bad_device_id", strings.Replace(w.bodyFor("sample-two"), testDeviceID2, "ABC", 1), "device_id", 400},
		{"empty_device_id", strings.Replace(w.bodyFor("sample-two"), testDeviceID2, "", 1), "device_id", 400},
		{"device_id_taken", strings.Replace(w.newBody(), "sample-new", "sample-two", 1), "device_id", 409},
		{"not_an_alias", w.bodyFor("Not An Alias"), "alias", 400},
		{"login_nobody_uses", strings.Replace(w.bodyFor("sample-two"), "proxy-account/username", "other/username", 1), "proxy.username_ref", 400},
		{"unknown_plan", strings.Replace(w.bodyFor("sample-two"), `"pro"`, `"no-such-plan"`, 1), "plan", 400},
		{"missing_proxy", `{"alias":"sample-two","email":"two@example.test","access":"gateway","plan":"pro"}`, "proxy", 400},
		{"bad_port", strings.Replace(w.bodyFor("sample-two"), port, "70000", 1), "proxy.port", 400},
		{"bad_ip", strings.Replace(w.bodyFor("sample-two"), "192.0.2.2", "not-an-ip", 1), "proxy.expected_egress_ip", 400},
		{"bad_host", strings.Replace(w.bodyFor("sample-two"), host, "a/b", 1), "proxy.host", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := w.state(t)
			status, answer, body := w.serve(t, request("POST", "/accounts", test.body))
			if status != test.status || answer["field"] != test.field {
				t.Fatalf("%d %s", status, body)
			}
			if w.state(t) != before {
				t.Fatal("a refused create wrote")
			}
		})
	}
	// A template whose result is no reference for some alias is refused for that alias.
	w.writer.cfg.TokenRefTemplate = "env://TOKEN_{alias}"
	status, answer, _ = w.serve(t, request("POST", "/accounts", w.bodyFor("sample-x")))
	if status != 400 || answer["field"] != "alias" {
		t.Fatalf("alias that makes no reference: %d %v", status, answer)
	}
	if status, _, body := w.serve(t, request("POST", "/accounts", w.bodyFor("samplex"))); status != 200 {
		t.Fatalf("control arm, an alias that makes one: %d %s", status, body)
	}
	// Without a template no account can be created; with one the same body passes (above).
	w.writer.cfg.TokenRefTemplate = ""
	status, answer, _ = w.serve(t, request("POST", "/accounts", w.bodyFor("sample-two")))
	if status != 409 || answer["field"] != "token_ref" {
		t.Fatalf("create without a template: %d %v", status, answer)
	}
}

// TestUpdateAccount checks an edit: the revision it names must be the current one (409
// otherwise, with nothing written), an email change bumps the revision and writes the
// old and new value to the audit without a reload, a change of the exit of a served
// account reloads and voids the checks, and an edit that would not load changes
// nothing.
func TestUpdateAccount(t *testing.T) {
	w := newWriteHarness(t)
	patch := func(alias, body string) (int, map[string]any, string) {
		return w.serve(t, request("PATCH", "/accounts/"+alias, body))
	}
	before := w.state(t)
	// The revision is checked first, and a stale one writes nothing.
	for _, body := range []string{`{"rev":7,"email":"x@example.test"}`, `{"email":"x@example.test"}`} {
		if status, _, _ := patch("sample-a", body); status != 409 && status != 400 {
			t.Fatalf("%s: status %d", body, status)
		}
	}
	if w.state(t) != before {
		t.Fatal("a refused edit wrote")
	}
	status, answer, body := patch("sample-a", `{"rev":1,"email":"changed@example.test"}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if answer["account"].(map[string]any)["rev"] != float64(2) || answer["reload"] != nil {
		t.Fatalf("answer = %v", answer)
	}
	audit, _ := w.store.AccountAudit(t.Context(), "sample-a")
	if len(audit) != 1 || audit[0].Changes["email"] != (session.FieldChange{Old: "sample-a@example.test", New: "changed@example.test"}) || len(audit[0].Changes) != 1 {
		t.Fatalf("audit = %+v", audit)
	}
	// The same revision again is stale now.
	if status, _, _ := patch("sample-a", `{"rev":1,"email":"again@example.test"}`); status != 409 {
		t.Fatalf("second edit at the old revision: %d", status)
	}
	// An edit that changes nothing writes nothing.
	if status, answer, _ := patch("sample-a", `{"rev":2,"email":"changed@example.test"}`); status != 200 || answer["account"].(map[string]any)["rev"] != float64(2) {
		t.Fatalf("no-op edit: %d %v", status, answer)
	}
	if audit, _ := w.store.AccountAudit(t.Context(), "sample-a"); len(audit) != 1 {
		t.Fatalf("a no-op edit wrote %d audit rows", len(audit))
	}

	if status, _, body := w.serve(t, request("POST", "/accounts", w.newBody())); status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
	// Refused edits, each with the field it names.
	for _, test := range []struct {
		name, body, field string
		status            int
	}{
		{"active_is_for_activate", `{"rev":2,"status":"active"}`, "status", 400},
		{"unknown_status", `{"rev":2,"status":"gone"}`, "status", 400},
		{"bad_device_id", `{"rev":2,"device_id":"ABC"}`, "device_id", 400},
		{"device_id_taken", `{"rev":2,"device_id":"` + testDeviceID + `"}`, "device_id", 409},
		{"bad_uuid", `{"rev":2,"account_uuid":"x"}`, "account_uuid", 400},
		{"bad_email", `{"rev":2,"email":"sk-ant-oat01-PASTED"}`, "email", 400},
		{"login_nobody_uses", `{"rev":2,"proxy":{"username_ref":"op://example-proxies/proxy-account/password"}}`, "proxy.username_ref", 400},
		{"alias_is_not_editable", `{"rev":2,"alias":"sample-b"}`, "", 400},
		{"token_ref_is_not_editable", `{"rev":2,"token_ref":"op://example-vault/x/y"}`, "", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := w.state(t)
			status, answer, body := patch("sample-a", test.body)
			if status != test.status || (test.field != "" && answer["field"] != test.field) || strings.Contains(body, "PASTED") {
				t.Fatalf("%d %s", status, body)
			}
			if w.state(t) != before {
				t.Fatal("a refused edit wrote")
			}
		})
	}
	// A proxy login that exists but cannot be resolved is refused before anything is stored.
	before = w.state(t)
	status, answer, _ = patch("sample-a", `{"rev":2,"proxy":{"username_ref":"op://example-proxies/spare-account/username","password_ref":"op://example-proxies/spare-account/password"}}`)
	if status != 422 || w.state(t) != before {
		t.Fatalf("edit to an unresolvable login: %d %v", status, answer)
	}
	if status, _, _ := patch("sample-nobody", `{"rev":1,"email":"x@example.test"}`); status != 404 {
		t.Fatalf("edit of no account: %d", status)
	}

	// Moving a served account's expected IP reloads: the checks (run next) are voided by it.
	if status, _, body := w.serve(t, request("POST", "/accounts/sample-a/verify", `{"rev":2}`)); status != 200 {
		t.Fatalf("verify: %d %s", status, body)
	}
	row, _ := w.store.Account(t.Context(), "sample-a")
	if row.Verify.TokenOK == nil || !*row.Verify.TokenOK {
		t.Fatalf("verification not stored: %+v", row.Verify)
	}
	status, answer, body = patch("sample-a", `{"rev":2,"proxy":{"expected_egress_ip":"192.0.2.99"}}`)
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	reload, _ := answer["reload"].(map[string]any)
	// The exit now reads another IP than the expected one, so the reload sets nothing
	// changed and rejects the account instead.
	if reload == nil || len(reload["rejected"].([]any)) != 1 {
		t.Fatalf("reload = %v", answer["reload"])
	}
	row, _ = w.store.Account(t.Context(), "sample-a")
	if row.Verify.TokenOK != nil || row.Verify.EgressOK != nil || row.Verify.ProbeOK != nil || !row.Verify.At.IsZero() {
		t.Fatalf("the change of the expected IP left the checks: %+v", row.Verify)
	}
}

// TestVerify checks the three checks: all pass for a good account, the token check
// fails for a reference that does not resolve and the other two are skipped without a
// request, the egress check fails for an exit that reads another IP, the probe for a
// refused token, and the probe waits for the lock of the quota rounds. The probe goes
// out with the account's own token, through its own exit.
func TestVerify(t *testing.T) {
	w := newWriteHarness(t)
	verify := func(alias string) (int, map[string]any, string) {
		return w.serve(t, request("POST", "/accounts/"+alias+"/verify", `{}`))
	}
	checks := func(answer map[string]any) []string {
		report := answer["verify"].(map[string]any)
		var out []string
		for _, name := range []string{"token_ok", "egress_ok", "probe_ok"} {
			out = append(out, report[name].(map[string]any)["result"].(string))
		}
		return out
	}
	status, answer, body := verify("sample-a")
	if status != 200 || !slices.Equal(checks(answer), []string{"pass", "pass", "pass"}) {
		t.Fatalf("good account: %d %s", status, body)
	}
	if tokens := w.probeTokens; !slices.Equal(tokens, []string{"sk-ant-oat01-synthetic-value-sample-a"}) {
		t.Fatalf("the probe carried %v", tokens)
	}
	if audit, _ := w.store.AccountAudit(t.Context(), "sample-a"); len(audit) != 1 || audit[0].Action != actionVerify || audit[0].Changes["probe_ok"].New != "pass" {
		t.Fatalf("audit = %+v", audit)
	}
	if len(w.egress.last) != 0 && w.egress.lastCheck(w.accounts.lookup("sample-new")) != (EgressResult{}) {
		t.Fatal("egress keeps the candidate")
	}

	// A token reference that does not resolve: the token check fails and nothing else runs.
	w.serve(t, request("POST", "/accounts", w.newBody()))
	w.mu.Lock()
	w.probeTokens = nil
	w.mu.Unlock()
	w.echo.mu.Lock()
	w.echo.ip = "192.0.2.2"
	w.echo.mu.Unlock()
	if status, answer, body := verify("sample-new"); status != 200 || !slices.Equal(checks(answer), []string{"pass", "pass", "pass"}) {
		t.Fatalf("new account: %d %s", status, body)
	}
	status, answer, body = verify("sample-spare")
	if status != 200 || !slices.Equal(checks(answer), []string{"pass", "fail", "skipped"}) {
		t.Fatalf("account with an unresolvable proxy login: %d %s", status, body)
	}

	// An exit that reads another IP: the egress check fails and the probe is skipped.
	w.mu.Lock()
	w.probeTokens = nil
	w.mu.Unlock()
	w.echo.mu.Lock()
	w.echo.ip = "198.51.100.9"
	w.echo.mu.Unlock()
	status, answer, body = verify("sample-a")
	if status != 200 || !slices.Equal(checks(answer), []string{"pass", "fail", "skipped"}) || len(w.probeTokens) != 0 {
		t.Fatalf("exit with another IP: %d %s, probe tokens %v", status, body, w.probeTokens)
	}
	w.echo.mu.Lock()
	w.echo.ip = "192.0.2.1"
	w.echo.mu.Unlock()

	// A token the upstream refuses: the probe fails.
	w.mu.Lock()
	w.probeStatus = http.StatusUnauthorized
	w.mu.Unlock()
	status, answer, body = verify("sample-a")
	if status != 200 || !slices.Equal(checks(answer), []string{"pass", "pass", "fail"}) {
		t.Fatalf("refused token: %d %s", status, body)
	}
	w.mu.Lock()
	w.probeStatus = http.StatusOK
	w.mu.Unlock()

	// A quota round holds the lock: the probe waits for it and fails when it does not come.
	old := probeWait
	probeWait = 50 * time.Millisecond
	t.Cleanup(func() { probeWait = old })
	if !w.writer.cfg.Entrypoint.router.rounds.acquire(t.Context()) {
		t.Fatal("lock not free")
	}
	status, answer, body = verify("sample-a")
	w.writer.cfg.Entrypoint.router.rounds.release()
	if status != 200 || !slices.Equal(checks(answer), []string{"pass", "pass", "fail"}) || !strings.Contains(body, "quota refresh was running") {
		t.Fatalf("probe during a quota round: %d %s", status, body)
	}
	if status, answer, _ := verify("sample-a"); status != 200 || !slices.Equal(checks(answer), []string{"pass", "pass", "pass"}) {
		t.Fatalf("probe after the round: %d %v", status, answer)
	}

	// A stale revision is refused.
	if status, _, _ := w.serve(t, request("POST", "/accounts/sample-a/verify", `{"rev":99}`)); status != 409 {
		t.Fatalf("verify at a stale revision: %d", status)
	}
	// An account not on the gateway path is not verified.
	if status, _, _ := verify("sample-nobody"); status != 404 {
		t.Fatalf("verify of no account: %d", status)
	}
}

// TestActivate checks the activation: with a plan, an exit of its own and the three
// checks passing it stores active and reloads, so the account is served; each missing
// condition is a 409 that names it and leaves the account as it was.
func TestActivate(t *testing.T) {
	w := newWriteHarness(t)
	activate := func(alias string, rev int) (int, map[string]any, string) {
		return w.serve(t, request("POST", "/accounts/"+alias+"/activate", `{"rev":`+string(rune('0'+rev))+`}`))
	}
	w.serve(t, request("POST", "/accounts", w.newBody()))
	w.echo.mu.Lock()
	w.echo.ip = "192.0.2.2"
	w.echo.mu.Unlock()

	for _, test := range []struct {
		name, alias string
		rev         int
		check       string
		prepare     func()
	}{
		{"stale_revision", "sample-new", 5, "", nil},
		{"no_plan", "sample-spare", 1, "plan", nil},
		{"exit_taken", "sample-new", 2, "exit", func() {
			if _, err := w.store.UpdateAccount(t.Context(), "sample-new", 1, session.AuditEntry{Action: "test"}, func(row *session.AccountRow) error { row.ExpectedIP = "192.0.2.1"; return nil }); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare()
			}
			before := w.state(t)
			status, answer, body := activate(test.alias, test.rev)
			if status != 409 || (test.check != "" && answer["check"] != test.check) {
				t.Fatalf("%d %s", status, body)
			}
			if w.state(t) != before {
				t.Fatal("a refused activation wrote")
			}
		})
	}
	// Put the exit right again (revision 2 after the edit above).
	if _, err := w.store.UpdateAccount(t.Context(), "sample-new", 2, session.AuditEntry{Action: "test"}, func(row *session.AccountRow) error { row.ExpectedIP = "192.0.2.2"; return nil }); err != nil {
		t.Fatal(err)
	}

	// Each check that fails is named, and the account stays testing.
	for _, test := range []struct {
		name, check string
		break_      func()
		repair      func()
	}{
		{"token", "token_ok", func() {
			w.writer.cfg.Resolve = func(context.Context) (Resolver, error) {
				return func(ctx context.Context, ref string) (string, error) {
					if strings.Contains(ref, "token-sample-new") {
						return "", errors.New("no")
					}
					return writeResolve(ctx, ref)
				}, nil
			}
		}, func() { w.writer.cfg.Resolve = func(context.Context) (Resolver, error) { return writeResolve, nil } }},
		{"egress", "egress_ok", func() { w.echo.mu.Lock(); w.echo.ip = "198.51.100.9"; w.echo.mu.Unlock() }, func() { w.echo.mu.Lock(); w.echo.ip = "192.0.2.2"; w.echo.mu.Unlock() }},
		{"probe", "probe_ok", func() { w.mu.Lock(); w.probeStatus = 401; w.mu.Unlock() }, func() { w.mu.Lock(); w.probeStatus = 200; w.mu.Unlock() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.break_()
			defer test.repair()
			status, answer, body := activate("sample-new", 3)
			if status != 409 || answer["check"] != test.check {
				t.Fatalf("%d %s", status, body)
			}
			row, _ := w.store.Account(t.Context(), "sample-new")
			if row.Status != "testing" || slices.Contains(w.accounts.Candidates(), "sample-new") {
				t.Fatalf("the account is %s and served %v", row.Status, w.accounts.Candidates())
			}
			// What the failed activation learned is audited as the activation.
			if audit, _ := w.store.AccountAudit(t.Context(), "sample-new"); len(audit) == 0 || audit[len(audit)-1].Action != actionActivate {
				t.Fatalf("audit of a failed activation = %+v", audit)
			}
		})
	}

	status, answer, body := activate("sample-new", 3)
	if status != 200 {
		t.Fatalf("activation: %d %s", status, body)
	}
	if account := answer["account"].(map[string]any); account["status"] != "active" {
		t.Fatalf("account = %v", account)
	}
	if !slices.Contains(w.accounts.Candidates(), "sample-new") {
		t.Fatalf("the activated account is not served: %v", w.accounts.Candidates())
	}
	if status, _, _ := activate("sample-new", 4); status != 409 {
		t.Fatalf("activation of an active account: %d", status)
	}
}

// TestActivateNeedsDeviceID checks that an account without a device ID, as an older one
// may be, is not activated, whatever else is ready.
func TestActivateNeedsDeviceID(t *testing.T) {
	w := newWriteHarness(t)
	w.serve(t, request("POST", "/accounts", w.newBody()))
	if _, err := w.store.UpdateAccount(t.Context(), "sample-new", 1, session.AuditEntry{Action: "test"}, func(row *session.AccountRow) error { row.DeviceID = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	before := w.state(t)
	status, answer, body := w.serve(t, request("POST", "/accounts/sample-new/activate", `{"rev":2}`))
	if status != 409 || answer["check"] != "device_id" || w.state(t) != before {
		t.Fatalf("%d %s", status, body)
	}
}

// TestNoCredentialValueIsKept checks, after a run of writes, that no credential value
// the resolver gave out is in the log, an audit row, the table or a snapshot of the
// store; a reference is (the control arm of the search).
func TestNoCredentialValueIsKept(t *testing.T) {
	w := newWriteHarness(t)
	w.serve(t, request("POST", "/accounts", w.newBody()))
	w.serve(t, request("POST", "/accounts/sample-a/verify", `{}`))
	w.serve(t, request("PATCH", "/accounts/sample-a", `{"rev":1,"email":"changed@example.test"}`))
	w.serve(t, request("POST", "/accounts", w.bodyFor("sample-new"))) // refused, taken
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := w.store.VacuumInto(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "op://example-vault/token-sample-new/credential") {
		t.Fatal("control arm: the snapshot has no reference")
	}
	for _, value := range writeValues {
		if strings.Contains(string(data), value) || strings.Contains(w.logs.String(), value) {
			t.Fatalf("a credential value %q is in the snapshot or the log", value)
		}
	}
	for _, alias := range []string{"sample-a", "sample-new"} {
		audit, _ := w.store.AccountAudit(t.Context(), alias)
		encoded, _ := json.Marshal(audit)
		for _, value := range writeValues {
			if strings.Contains(string(encoded), value) {
				t.Fatalf("a credential value is in the audit of %s", alias)
			}
		}
	}
}

// TestDashboardWriteBlock checks what /dashboard.json says of the writes: for a source
// on the list they are enabled and the proxy logins in use are listed as strings; for
// another source, or with no list, they are off with the reason; a gateway without a
// write path says its accounts are not editable.
func TestDashboardWriteBlock(t *testing.T) {
	w := newWriteHarness(t)
	read := func(remote string) map[string]any {
		r := httptest.NewRequest("GET", "/dashboard.json", nil)
		r.Host, r.RemoteAddr = writeHost, remote
		status, answer, body := w.serve(t, r)
		if status != 200 {
			t.Fatalf("%d %s", status, body)
		}
		return answer
	}
	on := read("192.0.2.50:1")
	if write := on["write"].(map[string]any); write["enabled"] != true || write["can_create"] != true || on["accounts_editable"] != true {
		t.Fatalf("answer for a listed source: %v", on["write"])
	}
	options := on["proxy_ref_options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["username_ref"] != "op://example-proxies/proxy-account/username" {
		t.Fatalf("proxy_ref_options = %v", options)
	}
	if write := read("198.51.100.7:1")["write"].(map[string]any); write["enabled"] != false || write["reason"] == "" {
		t.Fatalf("answer for another source: %v", write)
	}
	w.writer.cfg.AllowFrom = nil
	if write := read("192.0.2.50:1")["write"].(map[string]any); write["enabled"] != false || !strings.Contains(write["reason"].(string), "no source address") {
		t.Fatalf("answer with no list: %v", write)
	}
	w.writer.cfg.TokenRefTemplate = ""
	if write := read("192.0.2.50:1")["write"].(map[string]any); write["can_create"] != false {
		t.Fatalf("can_create without a template: %v", write)
	}

	// A provider without a write path: 501 on every route, before any other check, and not editable.
	reader, err := session.OpenReadOnly(w.path, session.Config{}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	codex := HostGuard([]string{writeHost}, NewDashboard(w.egress, DashboardConfig{Provider: "codex", Reader: reader, Store: w.store, DBPath: w.path, Started: w.clock}).Handler())
	for _, r := range []*http.Request{
		request("POST", "/accounts", w.newBody()), request("PATCH", "/accounts/sample-a", `{}`),
		request("POST", "/accounts/sample-a/verify", `{}`), request("POST", "/accounts/sample-a/activate", `{}`),
		func() *http.Request {
			r := request("POST", "/accounts", w.newBody())
			r.RemoteAddr = "198.51.100.7:1"
			return r
		}(),
	} {
		recorder := httptest.NewRecorder()
		codex.ServeHTTP(recorder, r)
		if recorder.Code != 501 {
			t.Fatalf("%s %s: %d", r.Method, r.URL.Path, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dashboard.json", nil)
	r.Host = writeHost
	codex.ServeHTTP(recorder, r)
	var view map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil || view["accounts_editable"] != false {
		t.Fatalf("codex view: %v %v", view["accounts_editable"], err)
	}
}

// TestUpdatePlan checks that an edit can change the plan: through the plan schedule, as
// the plan command does, with the old and new plan in the audit; a raised quota unit
// reads the quota of a served account once, a lowered or an unchanged one does not; an
// unknown plan changes nothing.
func TestUpdatePlan(t *testing.T) {
	w := newWriteHarness(t)
	patch := func(alias, body string) (int, map[string]any, string) {
		return w.serve(t, request("PATCH", "/accounts/"+alias, body))
	}
	plan := func(alias string) string {
		current, err := w.writer.currentPlan(t.Context(), alias, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	probes := func() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.probeTokens) }

	// Raised: pro to max-5x on a served account reads its quota once.
	status, answer, body := patch("sample-a", `{"rev":1,"plan":"max-5x"}`)
	if status != 200 || answer["plan_refresh"] != "refreshed" || plan("sample-a") != "max-5x" || probes() != 1 {
		t.Fatalf("raise: %d %s, plan %q, probes %d", status, body, plan("sample-a"), probes())
	}
	audit, _ := w.store.AccountAudit(t.Context(), "sample-a")
	if len(audit) != 1 || audit[0].Changes["plan"] != (session.FieldChange{Old: "pro", New: "max-5x"}) || audit[0].Action != actionUpdate {
		t.Fatalf("audit = %+v", audit)
	}
	// The account row is untouched by a plan-only edit.
	if row, _ := w.store.Account(t.Context(), "sample-a"); row.Rev != 1 {
		t.Fatalf("a plan-only edit bumped the revision to %d", row.Rev)
	}
	// Lowered: no refresh.
	status, answer, body = patch("sample-a", `{"rev":1,"plan":"pro"}`)
	if status != 200 || answer["plan_refresh"] != nil || plan("sample-a") != "pro" || probes() != 1 {
		t.Fatalf("lower: %d %s, plan %q, probes %d", status, body, plan("sample-a"), probes())
	}
	// Unchanged: nothing written.
	status, _, _ = patch("sample-a", `{"rev":1,"plan":"pro"}`)
	if audit, _ := w.store.AccountAudit(t.Context(), "sample-a"); status != 200 || len(audit) != 2 {
		t.Fatalf("same plan: %d, %d audit rows", status, len(audit))
	}
	// A plan with no unit, and a name that is no plan name, change nothing.
	before := w.state(t)
	for _, plan := range []string{`"no-such-plan"`, `"Not A Plan"`} {
		if status, answer, _ := patch("sample-a", `{"rev":1,"plan":`+plan+`}`); status != 400 || answer["field"] != "plan" {
			t.Fatalf("plan %s: %d %v", plan, status, answer)
		}
	}
	if w.state(t) != before || plan("sample-a") != "pro" {
		t.Fatal("a refused plan edit wrote")
	}
	// An account that is not served, with no plan yet, gets one and no refresh.
	status, answer, body = patch("sample-spare", `{"rev":1,"plan":"max-20x"}`)
	if status != 200 || answer["plan_refresh"] != nil || plan("sample-spare") != "max-20x" || probes() != 1 {
		t.Fatalf("spare: %d %s", status, body)
	}
	if audit, _ := w.store.AccountAudit(t.Context(), "sample-spare"); len(audit) != 1 || audit[0].Changes["plan"] != (session.FieldChange{Old: "", New: "max-20x"}) {
		t.Fatalf("spare audit = %+v", audit)
	}
}

// TestReceiverEmailAndProxyKinds checks the receiver email and the proxy's provider and
// type: all three are optional on create and empty when left out, a create and an edit
// set them, they are audited like the email, a bare address is the only receiver email
// that passes, and an edit of them reloads nothing.
func TestReceiverEmailAndProxyKinds(t *testing.T) {
	w := newWriteHarness(t)
	configOfAccount := func(answer map[string]any) (receiver, provider, kind any) {
		account := answer["account"].(map[string]any)
		proxy := account["proxy"].(map[string]any)
		return account["receiver_email"], proxy["provider"], proxy["type"]
	}
	// Left out, they are empty.
	status, answer, body := w.serve(t, request("POST", "/accounts", w.newBody()))
	if receiver, provider, kind := configOfAccount(answer); status != 200 || receiver != "" || provider != "" || kind != "" {
		t.Fatalf("create without them: %d %s", status, body)
	}
	// A create sets them.
	withThem := strings.Replace(w.bodyFor("sample-two"), `"proxy":{`, `"receiver_email":"receiver@example.test","proxy":{"provider":"example-provider","type":"dedicated-isp",`, 1)
	status, answer, body = w.serve(t, request("POST", "/accounts", withThem))
	if receiver, provider, kind := configOfAccount(answer); status != 200 || receiver != "receiver@example.test" || provider != "example-provider" || kind != "dedicated-isp" {
		t.Fatalf("create with them: %d %s", status, body)
	}
	audit, err := w.store.AccountAudit(t.Context(), "sample-two")
	if err != nil || len(audit) == 0 || audit[0].Changes["receiver_email"].New != "receiver@example.test" || audit[0].Changes["proxy_provider"].New != "example-provider" || audit[0].Changes["proxy_type"].New != "dedicated-isp" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if strings.Contains(w.logs.String(), "receiver@example.test") {
		t.Fatalf("log holds the receiver email: %q", w.logs.String())
	}
	// Refused values, each naming its field and writing nothing.
	for _, test := range []struct{ name, body, field string }{
		{"display_name", `{"rev":1,"receiver_email":"Someone <receiver@example.test>"}`, "receiver_email"},
		{"quoted_name", `{"rev":1,"receiver_email":"\"Someone\" <receiver@example.test>"}`, "receiver_email"},
		{"not_an_address", `{"rev":1,"receiver_email":"receiver"}`, "receiver_email"},
		{"two_addresses", `{"rev":1,"receiver_email":"a@example.test, b@example.test"}`, "receiver_email"},
		{"bad_provider", `{"rev":1,"proxy":{"provider":"a b"}}`, "proxy.provider"},
		{"bad_type", `{"rev":1,"proxy":{"type":"sk-ant-oat01-PASTED value"}}`, "proxy.type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := w.state(t)
			status, answer, body := w.serve(t, request("PATCH", "/accounts/sample-a", test.body))
			if status != 400 || answer["field"] != test.field || strings.Contains(body, "PASTED") || w.state(t) != before {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
	badCreate := strings.Replace(w.bodyFor("sample-three"), `"proxy":{`, `"receiver_email":"Someone <receiver@example.test>","proxy":{`, 1)
	if status, answer, _ := w.serve(t, request("POST", "/accounts", badCreate)); status != 400 || answer["field"] != "receiver_email" {
		t.Fatalf("create with a display name: %d %v", status, answer)
	}
	// An edit of a served account sets them and reloads nothing, and clearing them works.
	status, answer, body = w.serve(t, request("PATCH", "/accounts/sample-a", `{"rev":1,"receiver_email":"r2@example.test","proxy":{"provider":"p","type":"t"}}`))
	if receiver, provider, kind := configOfAccount(answer); status != 200 || answer["reload"] != nil || receiver != "r2@example.test" || provider != "p" || kind != "t" {
		t.Fatalf("edit: %d %s", status, body)
	}
	status, answer, body = w.serve(t, request("PATCH", "/accounts/sample-a", `{"rev":2,"receiver_email":"","proxy":{"provider":"","type":""}}`))
	if receiver, provider, kind := configOfAccount(answer); status != 200 || receiver != "" || provider != "" || kind != "" {
		t.Fatalf("clearing: %d %s", status, body)
	}
}
