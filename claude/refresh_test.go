package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// refreshExit is a fake exit in front of a fake upstream: it answers every request with
// status and the 5h and 7d readings at utilization, and keeps what it received.
type refreshExit struct {
	server      *httptest.Server
	mu          sync.Mutex
	status      int
	utilization string // empty sends no readings
	received    []*http.Request
	bodies      []string
	shared      *overlap // when set, each request is held a moment and counted in it
	onRequest   func()   // when set, called with each request before it is answered
}

// overlap counts the requests that a set of exits serve at the same time.
type overlap struct {
	mu        sync.Mutex
	now, most int
}

func (o *overlap) enter() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.now++
	o.most = max(o.most, o.now)
}

func (o *overlap) leave() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.now--
}

// startRefreshExit starts the exit; readings reset an hour after clock.
func startRefreshExit(t *testing.T, clock time.Time) *refreshExit {
	t.Helper()
	e := &refreshExit{status: 200}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.received, e.bodies = append(e.received, r), append(e.bodies, string(body))
		status, utilization, shared, onRequest := e.status, e.utilization, e.shared, e.onRequest
		e.mu.Unlock()
		if onRequest != nil {
			onRequest()
		}
		if shared != nil {
			shared.enter()
			defer shared.leave()
			time.Sleep(50 * time.Millisecond)
		}
		if utilization != "" {
			for _, window := range []string{"5h", "7d"} {
				w.Header().Set("Anthropic-Ratelimit-Unified-"+window+"-Utilization", utilization)
				w.Header().Set("Anthropic-Ratelimit-Unified-"+window+"-Reset", strconv.FormatInt(clock.Add(time.Hour).Unix(), 10))
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"exit":"A"}`)
	}))
	t.Cleanup(e.server.Close)
	return e
}

// answer sets what the exit answers next.
func (e *refreshExit) answer(status int, utilization string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.utilization = status, utilization
}

// request returns the i-th request the exit received and its body.
func (e *refreshExit) request(i int) (*http.Request, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.received[i], e.bodies[i]
}

// requests returns how many requests the exit received.
func (e *refreshExit) requests() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.received)
}

// TestRefreshMakesAccountSelectableAgain checks the refresh of an account the upstream
// rate-limited: one request through the account's own exit with the account's token,
// readings stored under the source refresh and, when they are under hard, the quota
// pause ended so that the session's next request is served by the account. Control
// arms: a refresh that still reads over hard, and a 429 without readings, keep the
// account out; an account paused for another reason gets 409 and nothing is sent.
func TestRefreshMakesAccountSelectableAgain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	exit := startRefreshExit(t, h.clock)
	exitURL, err := url.Parse(exit.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", exitURL)
	h.token, h.sessionID = h.issue()
	rt := h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{})
	gateway := httptest.NewServer(rt)
	t.Cleanup(gateway.Close)
	admin := httptest.NewServer(NewAdmin(h.accounts, h.store, nil).Handler())
	t.Cleanup(admin.Close)
	adminWith := NewAdmin(h.accounts, h.store, nil)
	adminWith.Refresh = rt.refresh
	refresher := httptest.NewServer(adminWith.Handler())
	t.Cleanup(refresher.Close)

	// sample-a was rate-limited and reads over hard until its reset.
	if err := h.store.RecordQuota(ctx, "sample-a", []session.Reading{{Window: session.Window7d, Utilization: 1, Status: "rejected", ResetAt: h.clock.Add(3 * 24 * time.Hour), Source: "429"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Pause(ctx, "sample-a", h.clock.Add(3*24*time.Hour), session.QuotaPauseReason); err != nil {
		t.Fatal(err)
	}
	if status, _ := post(t, h, gateway, h.token, `{}`, "", ""); status != 429 || exit.requests() != 0 {
		t.Fatalf("before the refresh: status=%d exit=%d", status, exit.requests())
	}
	if status, _ := adminCall(t, admin, http.MethodPost, "/accounts/sample-a/refresh", ""); status != 501 {
		t.Fatalf("refresh without a refresher: status=%d", status)
	}
	if status, _ := adminCall(t, refresher, http.MethodPost, "/accounts/sample-z/refresh", ""); status != 404 {
		t.Fatalf("refresh of an unknown alias: status=%d", status)
	}

	// Still over hard: the reading is stored, the pause stays.
	exit.answer(200, "1.02")
	status, body := adminCall(t, refresher, http.MethodPost, "/accounts/sample-a/refresh", "")
	if status != 200 || body["pause_ended"] != false || h.paused()["sample-a"] != session.QuotaPauseReason {
		t.Fatalf("over hard: status=%d body=%v paused=%v", status, body, h.paused())
	}
	// A 429 with no readings keeps the account out.
	exit.answer(429, "")
	if status, body = adminCall(t, refresher, http.MethodPost, "/accounts/sample-a/refresh", ""); status != 200 || body["upstream_status"] != float64(429) || h.paused()["sample-a"] != session.QuotaPauseReason {
		t.Fatalf("429: status=%d body=%v paused=%v", status, body, h.paused())
	}
	if status, _ := post(t, h, gateway, h.token, `{}`, "", ""); status != 429 || exit.requests() != 2 {
		t.Fatalf("after the refused refreshes: status=%d exit=%d", status, exit.requests())
	}

	// The plan went up upstream: the reading is low and the account is chosen again.
	exit.answer(200, "0.1")
	status, body = adminCall(t, refresher, http.MethodPost, "/accounts/sample-a/refresh", "")
	if status != 200 || body["pause_ended"] != true || len(h.paused()) != 0 {
		t.Fatalf("under hard: status=%d body=%v paused=%v", status, body, h.paused())
	}
	q, err := h.store.LatestQuota(ctx, "sample-a")
	if err != nil || q["5h"].Utilization != 0.1 || q["7d"].Utilization != 0.1 || q["7d"].Source != "refresh" {
		t.Fatalf("stored readings %+v err=%v", q, err)
	}
	refresh, refreshBody := exit.request(2)
	var sent struct {
		MaxTokens int    `json:"max_tokens"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal([]byte(refreshBody), &sent); err != nil {
		t.Fatal(err)
	}
	if refresh.Method != "POST" || refresh.URL.Path != "/v1/messages" || refresh.Header.Get("Authorization") != "Bearer sk-ant-oat01-synthetic-token-a" ||
		refresh.Header.Get("Proxy-Authorization") == "" || !hasOAuthBeta(refresh.Header) || sent.MaxTokens != 1 || sent.Model == "" {
		t.Fatalf("refresh request %s %s headers=%v body=%s", refresh.Method, refresh.URL, refresh.Header, refreshBody)
	}
	if status, text := post(t, h, gateway, h.token, `{}`, "", ""); status != 200 || text != `{"exit":"A"}` {
		t.Fatalf("after the refresh: status=%d body=%s", status, text)
	}

	// An account paused for another reason is not refreshed: its token stays home.
	for _, reason := range []string{"upstream_401", "manual", EgressMismatchReason} {
		if err := h.store.Pause(ctx, "sample-a", indefinitePause, reason); err != nil {
			t.Fatal(err)
		}
		sent := exit.requests()
		if status, body := adminCall(t, refresher, http.MethodPost, "/accounts/sample-a/refresh", ""); status != 409 || body["error"] != "account paused: "+reason || exit.requests() != sent || h.paused()["sample-a"] != reason {
			t.Fatalf("%s: status=%d body=%v exit=%d (was %d) paused=%v", reason, status, body, exit.requests(), sent, h.paused())
		}
		if err := h.store.Resume(ctx, "sample-a"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRefreshAll checks POST /refresh over accounts that each have their own exit: one
// request through the exit of each account that is not paused or is paused for quota,
// with that account's token, and nothing through the exit of an account paused for
// another reason, which the answer lists as skipped. The refreshes go out one after
// another: no two exits serve at the same time. An account whose exit cannot be
// reached fails without stopping the accounts after it.
func TestRefreshAll(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	exits := map[string]*refreshExit{}
	shared := &overlap{}
	for _, alias := range []string{"sample-a", "sample-b", "sample-c", "sample-d", "sample-e"} {
		exit := startRefreshExit(t, h.clock)
		exit.answer(200, "0.1")
		exit.shared = shared
		exitURL, err := url.Parse(exit.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		h.addWithExit(alias, "sk-ant-oat01-synthetic-token-"+alias, exitURL)
		exits[alias] = exit
	}
	exits["sample-a"].server.Close()
	for alias, reason := range map[string]string{"sample-c": session.QuotaPauseReason, "sample-d": "upstream_401", "sample-e": EgressMismatchReason} {
		if err := h.store.Pause(ctx, alias, h.clock.Add(time.Hour), reason); err != nil {
			t.Fatal(err)
		}
	}
	admin := NewAdmin(h.accounts, h.store, nil)
	admin.RefreshAll = h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{}).refreshAll
	server := httptest.NewServer(admin.Handler())
	t.Cleanup(server.Close)

	res, err := server.Client().Post(server.URL+"/refresh", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var results []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&results); err != nil || res.StatusCode != 200 {
		t.Fatalf("status=%d err=%v", res.StatusCode, err)
	}
	want := []string{
		"sample-a error=upstream not reached through the account's exit",
		"sample-b upstream_status=200 pause_ended=false",
		"sample-c upstream_status=200 pause_ended=true",
		"sample-d skipped=account paused: upstream_401",
		"sample-e skipped=account paused: " + EgressMismatchReason,
	}
	var got []string
	for _, result := range results {
		switch {
		case result["error"] != nil:
			got = append(got, fmt.Sprintf("%v error=%v", result["alias"], result["error"]))
		case result["skipped"] != nil:
			got = append(got, fmt.Sprintf("%v skipped=%v", result["alias"], result["skipped"]))
		default:
			got = append(got, fmt.Sprintf("%v upstream_status=%v pause_ended=%v", result["alias"], result["upstream_status"], result["pause_ended"]))
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("results %q, want %q", got, want)
	}
	for alias, sent := range map[string]int{"sample-b": 1, "sample-c": 1, "sample-d": 0, "sample-e": 0} {
		if exits[alias].requests() != sent {
			t.Fatalf("exit of %s received %d requests, want %d", alias, exits[alias].requests(), sent)
		}
		if sent == 1 {
			if req, _ := exits[alias].request(0); req.Header.Get("Authorization") != "Bearer sk-ant-oat01-synthetic-token-"+alias {
				t.Fatalf("exit of %s received another account's token", alias)
			}
		}
	}
	if shared.most != 1 {
		t.Fatalf("exits served up to %d refreshes at the same time, want 1", shared.most)
	}
	if paused := h.paused(); len(paused) != 2 || paused["sample-d"] != "upstream_401" || paused["sample-e"] != EgressMismatchReason {
		t.Fatalf("paused %v", paused)
	}
}

// TestQuotaRecheck checks one round of the quota recheck over accounts that each have
// their own exit: it refreshes an account whose reading at or over soft has passed its
// reset, and one paused for quota whose rejected reading has, ending that pause; it
// sends nothing for an account over soft whose reset is still ahead or unknown, one
// under soft whose reset passed, or one paused for another reason, and logs nothing
// for the last.
func TestQuotaRecheck(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	var logged strings.Builder
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)
	past, ahead := h.clock.Add(-time.Minute), h.clock.Add(time.Hour)
	stored := map[string]session.Reading{
		"sample-a": {Window: session.Window5h, Utilization: 0.92, ResetAt: past},
		"sample-b": {Window: session.Window5h, Utilization: 0.97, ResetAt: ahead},
		"sample-c": {Window: session.Window5h, Utilization: 0.5, ResetAt: past},
		"sample-d": {Window: session.Window5h, Utilization: 0.97, ResetAt: past},
		"sample-e": {Window: session.Window7d, Utilization: 0.5, Status: "rejected", ResetAt: past},
		"sample-f": {Window: session.Window5h, Utilization: 0.97, ResetAt: past},
		"sample-g": {Window: session.Window5h, Utilization: 0.97},
	}
	exits := map[string]*refreshExit{}
	for _, alias := range []string{"sample-a", "sample-b", "sample-c", "sample-d", "sample-e", "sample-f", "sample-g"} {
		exit := startRefreshExit(t, h.clock)
		exit.answer(200, "0.1")
		exitURL, err := url.Parse(exit.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		h.addWithExit(alias, "sk-ant-oat01-synthetic-token-"+alias, exitURL)
		exits[alias] = exit
		reading := stored[alias]
		reading.Source = "response"
		if err := h.store.RecordQuota(ctx, alias, []session.Reading{reading}); err != nil {
			t.Fatal(err)
		}
	}
	for alias, reason := range map[string]string{"sample-d": "upstream_401", "sample-e": session.QuotaPauseReason, "sample-f": "manual"} {
		if err := h.store.Pause(ctx, alias, ahead, reason); err != nil {
			t.Fatal(err)
		}
	}

	h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{}).recheckQuota(ctx)

	for alias, sent := range map[string]int{"sample-a": 1, "sample-b": 0, "sample-c": 0, "sample-d": 0, "sample-e": 1, "sample-f": 0, "sample-g": 0} {
		if exits[alias].requests() != sent {
			t.Errorf("exit of %s received %d requests, want %d", alias, exits[alias].requests(), sent)
		}
		window := stored[alias].Window
		q, err := h.store.LatestQuota(ctx, alias)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[int]string{0: "response", 1: "refresh"}[sent]; q[window].Source != want {
			t.Errorf("%s: %s reading %+v, want source %s", alias, window, q[window], want)
		}
	}
	if paused := h.paused(); len(paused) != 2 || paused["sample-d"] != "upstream_401" || paused["sample-f"] != "manual" {
		t.Errorf("paused %v", paused)
	}
	for _, alias := range []string{"sample-d", "sample-f"} {
		if strings.Contains(logged.String(), alias) {
			t.Errorf("log names %s, paused for another reason:\n%s", alias, logged.String())
		}
	}
}

// TestQuotaRecheckStopsOnShutdown checks that a round of the quota recheck whose context
// ends, before it starts or during a refresh, sends nothing more and logs nothing for
// the accounts it did not reach.
func TestQuotaRecheckStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newHarness(t)
	var logged strings.Builder
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)
	exits := map[string]*refreshExit{}
	for _, alias := range []string{"sample-a", "sample-b"} {
		exit := startRefreshExit(t, h.clock)
		exit.answer(200, "0.1")
		exitURL, err := url.Parse(exit.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		h.addWithExit(alias, "sk-ant-oat01-synthetic-token-"+alias, exitURL)
		exits[alias] = exit
		if err := h.store.RecordQuota(ctx, alias, []session.Reading{{Window: session.Window5h, Utilization: 0.97, ResetAt: h.clock.Add(-time.Minute), Source: "response"}}); err != nil {
			t.Fatal(err)
		}
	}
	rt := h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{})

	canceled, stop := context.WithCancel(ctx)
	stop()
	rt.recheckQuota(canceled)
	if exits["sample-a"].requests() != 0 || logged.Len() != 0 {
		t.Fatalf("round canceled before it started: exit of sample-a received %d requests, log:\n%s", exits["sample-a"].requests(), logged.String())
	}

	exits["sample-a"].mu.Lock()
	exits["sample-a"].onRequest = cancel
	exits["sample-a"].mu.Unlock()
	rt.recheckQuota(ctx)
	if exits["sample-a"].requests() != 1 || exits["sample-b"].requests() != 0 || strings.Contains(logged.String(), "sample-b") {
		t.Fatalf("round canceled during sample-a: exits received %d and %d requests, log:\n%s", exits["sample-a"].requests(), exits["sample-b"].requests(), logged.String())
	}
}

// quotaRefreshFixture is a dashboard with a quota refresh over accounts that each have
// their own exit, its rounds running until the test ends.
type quotaRefreshFixture struct {
	h       *harness
	rt      *router
	refresh *QuotaRefresh
	server  *httptest.Server
	exits   map[string]*refreshExit
}

// newQuotaRefreshFixture starts the dashboard over the aliases, each exit answering 200
// with low readings.
func newQuotaRefreshFixture(t *testing.T, aliases ...string) *quotaRefreshFixture {
	t.Helper()
	h := newHarness(t)
	f := &quotaRefreshFixture{h: h, exits: map[string]*refreshExit{}}
	for _, alias := range aliases {
		exit := startRefreshExit(t, h.clock)
		exit.answer(200, "0.1")
		exitURL, err := url.Parse(exit.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		h.addWithExit(alias, "sk-ant-oat01-synthetic-token-"+alias, exitURL)
		f.exits[alias] = exit
	}
	f.rt = h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{})
	f.refresh = NewQuotaRefresh(&Entrypoint{router: f.rt})
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	d := NewDashboard(NewEgress(h.accounts, h.store, testUpstream), DashboardConfig{Provider: "claude", Reader: reader, Store: h.store, DBPath: h.path, QuotaRefresh: f.refresh})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.RunQuotaRefresh(ctx)
	}()
	f.server = httptest.NewServer(d.Handler())
	t.Cleanup(func() {
		f.server.Close()
		cancel()
		<-done
	})
	return f
}

// post sends POST /quota/refresh as the page does and returns the status, the
// Retry-After header and the body.
func (f *quotaRefreshFixture) post(t *testing.T) (int, string, map[string]any) {
	t.Helper()
	status, retryAfter, body, err := f.tryPost(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return status, retryAfter, body
}

// tryPost is post for a goroutine other than the test's, which must not call t.Fatal.
func (f *quotaRefreshFixture) tryPost(ctx context.Context) (int, string, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/quota/refresh", nil)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("Origin", f.server.URL)
	req.Header.Set(QuotaRefreshHeader, "refresh")
	res, err := f.server.Client().Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return 0, "", nil, err
	}
	return res.StatusCode, res.Header.Get("Retry-After"), body, nil
}

// settle waits until no dashboard round is pending or running.
func (f *quotaRefreshFixture) settle(t *testing.T) QuotaRefreshState {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if state := f.refresh.State(); !state.Running {
			return state
		}
	}
	t.Fatal("the dashboard round did not end")
	return QuotaRefreshState{}
}

// TestQuotaRefreshRounds checks the dashboard's refresh: a POST answers 202 at once
// while its round still waits on an exit; a second POST during the round joins it,
// so each account gets one request, through its own exit with its own token, and an
// account paused for another reason gets none; after the round a POST gets 429 with
// Retry-After until QuotaRefreshWait has passed since the round ended, and then starts
// a new round.
func TestQuotaRefreshRounds(t *testing.T) {
	f := newQuotaRefreshFixture(t, "sample-a", "sample-b", "sample-c")
	if err := f.h.store.Pause(t.Context(), "sample-c", indefinitePause, "upstream_401"); err != nil {
		t.Fatal(err)
	}
	// The first request to sample-a's exit waits for free, which a failed test also
	// calls, before the fixture closes the exit.
	reached, release := make(chan struct{}), make(chan struct{})
	var first, freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	f.exits["sample-a"].mu.Lock()
	f.exits["sample-a"].onRequest = func() {
		first.Do(func() {
			close(reached)
			<-release
		})
	}
	f.exits["sample-a"].mu.Unlock()

	if status, _, body := f.post(t); status != http.StatusAccepted || body["started"] != true {
		t.Fatalf("first POST: status=%d body=%v", status, body)
	}
	<-reached
	if state := f.refresh.State(); !state.Running || !state.LastStarted.Equal(f.h.clock) || !state.WaitUntil.IsZero() {
		t.Fatalf("state during the round %+v", state)
	}
	if status, _, body := f.post(t); status != http.StatusAccepted || body["started"] != false {
		t.Fatalf("POST during the round: status=%d body=%v", status, body)
	}
	free()
	state := f.settle(t)
	for alias, sent := range map[string]int{"sample-a": 1, "sample-b": 1, "sample-c": 0} {
		if f.exits[alias].requests() != sent {
			t.Fatalf("exit of %s received %d requests, want %d", alias, f.exits[alias].requests(), sent)
		}
		if sent == 1 {
			if req, _ := f.exits[alias].request(0); req.Header.Get("Authorization") != "Bearer sk-ant-oat01-synthetic-token-"+alias {
				t.Fatalf("exit of %s received another account's token", alias)
			}
		}
	}
	if !state.LastFinished.Equal(f.h.clock) || !state.WaitUntil.Equal(f.h.clock.Add(QuotaRefreshWait)) {
		t.Fatalf("state after the round %+v", state)
	}

	if status, retryAfter, _ := f.post(t); status != http.StatusTooManyRequests || retryAfter != "300" {
		t.Fatalf("POST right after the round: status=%d Retry-After=%q", status, retryAfter)
	}
	f.h.clock = f.h.clock.Add(QuotaRefreshWait - time.Second)
	if status, retryAfter, _ := f.post(t); status != http.StatusTooManyRequests || retryAfter != "1" {
		t.Fatalf("POST a second before the wait ends: status=%d Retry-After=%q", status, retryAfter)
	}
	if f.exits["sample-a"].requests() != 1 || f.exits["sample-b"].requests() != 1 {
		t.Fatal("a refused POST sent requests")
	}
	f.h.clock = f.h.clock.Add(time.Second)
	if status, _, body := f.post(t); status != http.StatusAccepted || body["started"] != true {
		t.Fatalf("POST after the wait: status=%d body=%v", status, body)
	}
	f.settle(t)
	if f.exits["sample-a"].requests() != 2 || f.exits["sample-b"].requests() != 2 || f.exits["sample-c"].requests() != 0 {
		t.Fatalf("second round: exits received %d, %d and %d requests", f.exits["sample-a"].requests(), f.exits["sample-b"].requests(), f.exits["sample-c"].requests())
	}
}

// TestRefreshRoundsTakeTurns checks that a dashboard round, a refresh of all accounts
// over the management interface and a quota recheck, started together, never send two
// requests at the same time, and that each of them still refreshes every account.
func TestRefreshRoundsTakeTurns(t *testing.T) {
	f := newQuotaRefreshFixture(t, "sample-a", "sample-b")
	shared := &overlap{}
	for alias, exit := range f.exits {
		// The exits send no readings, so the stored one stays due for the recheck,
		// whichever round goes first: over soft, its reset passed.
		exit.answer(200, "")
		exit.mu.Lock()
		exit.shared = shared
		exit.mu.Unlock()
		if err := f.h.store.RecordQuota(t.Context(), alias, []session.Reading{{Window: session.Window5h, Utilization: 0.97, ResetAt: f.h.clock.Add(-time.Minute), Source: "response"}}); err != nil {
			t.Fatal(err)
		}
	}
	admin := NewAdmin(f.h.accounts, f.h.store, nil)
	admin.RefreshAll = f.rt.refreshAll
	adminServer := httptest.NewServer(admin.Handler())
	t.Cleanup(adminServer.Close)

	var wg sync.WaitGroup
	wg.Go(func() {
		res, err := adminServer.Client().Post(adminServer.URL+"/refresh", "", nil)
		if err != nil {
			t.Error(err)
			return
		}
		_ = res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("POST /refresh: status=%d", res.StatusCode)
		}
	})
	wg.Go(func() {
		if status, _, _, err := f.tryPost(t.Context()); err != nil || status != http.StatusAccepted {
			t.Errorf("POST /quota/refresh: status=%d err=%v", status, err)
		}
	})
	wg.Go(func() { f.rt.recheckQuota(t.Context()) })
	wg.Wait()
	f.settle(t)
	if shared.most != 1 {
		t.Fatalf("exits served up to %d refreshes at the same time, want 1", shared.most)
	}
	if f.exits["sample-a"].requests() != 3 || f.exits["sample-b"].requests() != 3 {
		t.Fatalf("exits received %d and %d requests, want 3 each", f.exits["sample-a"].requests(), f.exits["sample-b"].requests())
	}
}
