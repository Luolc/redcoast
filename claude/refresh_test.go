package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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
		status, utilization, shared := e.status, e.utilization, e.shared
		e.mu.Unlock()
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
	admin.Refresh = h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{}).refresh
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
