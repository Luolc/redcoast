package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
}

// startRefreshExit starts the exit; readings reset an hour after clock.
func startRefreshExit(t *testing.T, clock time.Time) *refreshExit {
	t.Helper()
	e := &refreshExit{status: 200}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.received, e.bodies = append(e.received, r), append(e.bodies, string(body))
		status, utilization := e.status, e.utilization
		e.mu.Unlock()
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
