package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// readQuota stores one 7d reading for alias.
func readQuota(t *testing.T, h *harness, alias string, utilization float64, resetAt time.Time) {
	t.Helper()
	if err := h.store.RecordQuota(context.Background(), alias, []session.Reading{{Window: session.Window7d, Utilization: utilization, Status: "allowed_warning", ResetAt: resetAt, Source: "response"}}); err != nil {
		t.Fatal(err)
	}
}

// TestLocalRateLimitWhenAllOverHard (E4.4) checks the gateway's own 429: both accounts
// over hard, a running session's request is answered locally with the API error shape,
// a message that names the gateway, Retry-After to the earliest reset, no forged
// ratelimit headers, zero requests upstream and a quota_exhausted traffic row. Control
// arm: the upstream's own 429 passes through with its different body and a status in
// the row.
func TestLocalRateLimitWhenAllOverHard(t *testing.T) {
	h, exitA, exitB := twoAccounts(t, 429, 429)
	server := h.gateway("http://127.0.0.1:1")
	earliest := h.clock.Add(90 * time.Minute)
	readQuota(t, h, "sample-a", 0.95, h.clock.Add(3*time.Hour))
	readQuota(t, h, "sample-b", 0.9, earliest)
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.clientFor(h.token).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type, Message string
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 429 || body.Type != "error" || body.Error.Type != "rate_limit_error" || !strings.HasPrefix(body.Error.Message, "redcoast: local gateway quota exhausted") {
		t.Fatalf("status=%d body=%+v", res.StatusCode, body)
	}
	if !strings.Contains(body.Error.Message, earliest.Format(time.RFC3339)) {
		t.Fatalf("message lacks the earliest reset: %q", body.Error.Message)
	}
	// Retry-After is measured on the harness clock, like the readings.
	if retry, err := strconv.Atoi(res.Header.Get("Retry-After")); err != nil || retry != 90*60 {
		t.Fatalf("Retry-After=%q err=%v, want %d", res.Header.Get("Retry-After"), err, 90*60)
	}
	for key := range res.Header {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-ratelimit") {
			t.Fatalf("forged header %s", key)
		}
	}
	if len(exitA.seen())+len(exitB.seen()) != 0 {
		t.Fatal("the refused request reached an exit")
	}
	rows := h.trafficRows(1)
	expectTraffic(t, rows, 0, trafficRow{sessionID: h.sessionID, kind: "inference", result: "quota_exhausted", status: 0})
	// Control arm: B drops under hard, the request goes upstream and the upstream's 429 is passed through unchanged.
	readQuota(t, h, "sample-b", 0.5, earliest)
	status, text := post(t, h, server, h.token, `{}`, "", "")
	if status != 429 || text != `{"exit":"B"}` {
		t.Fatalf("upstream 429 arm: status=%d body=%s", status, text)
	}
	rows = h.trafficRows(2)
	expectTraffic(t, rows, 1, trafficRow{sessionID: h.sessionID, alias: "sample-b", kind: "inference", result: "ok", status: 429})
}

// expectTraffic checks row i of rows against want field by field (the byte counts are
// not compared) and reports every field that differs, with the expected value, so that
// a failure names what was off; a missing row is reported before anything reads it.
func expectTraffic(t *testing.T, rows []trafficRow, i int, want trafficRow) {
	t.Helper()
	if i >= len(rows) {
		t.Fatalf("traffic row %d missing: rows=%+v", i, rows)
	}
	got := rows[i]
	var diffs []string
	for _, field := range []struct {
		name      string
		got, want any
	}{{"sessionID", got.sessionID, want.sessionID}, {"alias", got.alias, want.alias}, {"kind", got.kind, want.kind}, {"result", got.result, want.result}, {"status", got.status, want.status}} {
		if field.got != field.want {
			diffs = append(diffs, fmt.Sprintf("%s=%v want %v", field.name, field.got, field.want))
		}
	}
	if len(diffs) != 0 {
		t.Fatalf("traffic row %d: %s (rows=%+v)", i, strings.Join(diffs, "; "), rows)
	}
}

// TestUpstreamRateLimitMovesTheSession (E4.3, E4.6) checks that an upstream 429 on A
// pauses A until the earliest reset the response reports and the session's next
// request, the client's retry, lands on B with the conversation's history in the body.
// A 429 that reports no reset pauses A for the fixed interval. Control arm: a 200 from
// A pauses nothing and the next request stays on A.
func TestUpstreamRateLimitMovesTheSession(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		reset   bool
		wantB   bool
		paused  bool
		wantEnd time.Duration
	}{
		{"429_with_reset", 429, true, true, true, 30 * time.Minute},
		{"429_without_reset", 429, false, true, true, rateLimitPause},
		{"200_control", 200, true, false, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			exitB := startAccountExit(t, "B", 200)
			var exitASeen []string
			exitA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exitASeen = append(exitASeen, r.Header.Get("Authorization"))
				if test.reset {
					w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", map[int]string{429: "1.0", 200: "0.1"}[test.status])
					w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", map[int]string{429: "rejected", 200: "allowed"}[test.status])
					w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(h.clock.Add(test.wantEnd).Unix(), 10))
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"exit":"A"}`))
			}))
			t.Cleanup(exitA.Close)
			exitAURL := exitB.url(t)
			exitAURL.Host = strings.TrimPrefix(exitA.URL, "http://")
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", exitAURL)
			h.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", exitB.url(t))
			h.token, h.sessionID = h.issue()
			// B carries some use so that A is chosen first.
			readQuota(t, h, "sample-b", 0.3, h.clock.Add(6*24*time.Hour))
			server := h.gateway("http://127.0.0.1:1")
			status, _ := post(t, h, server, h.token, `{"messages":[{"role":"user","content":"first"}]}`, "", "")
			if status != test.status || len(exitASeen) != 1 {
				t.Fatalf("first request: status=%d exitA=%d", status, len(exitASeen))
			}
			paused := h.paused()
			if _, isPaused := paused["sample-a"]; isPaused != test.paused || (test.paused && paused["sample-a"] != "upstream_429") {
				t.Fatalf("paused=%v", paused)
			}
			if test.paused {
				var until int64
				if err := h.openFile().QueryRow("SELECT paused_until FROM account_state WHERE alias = 'sample-a'").Scan(&until); err != nil {
					t.Fatal(err)
				}
				if got := time.UnixMilli(until).Sub(h.clock); got != test.wantEnd {
					t.Fatalf("paused for %v, want %v", got, test.wantEnd)
				}
			}
			// The client's retry: the same session, the history in the body.
			status, text := post(t, h, server, h.token, `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"..."},{"role":"user","content":"second"}]}`, "", "")
			if status != 200 || len(exitB.seen()) != map[bool]int{true: 1, false: 0}[test.wantB] || text != map[bool]string{true: `{"exit":"B"}`, false: `{"exit":"A"}`}[test.wantB] {
				t.Fatalf("retry: status=%d body=%s exitB=%d", status, text, len(exitB.seen()))
			}
		})
	}
}

// TestForwardProxyRefusesWhenAllOverHard checks that side traffic shares the inference
// verdict: with every account over hard a CONNECT is answered 429 by the gateway and
// recorded as quota_exhausted, and no tunnel is opened.
func TestForwardProxyRefusesWhenAllOverHard(t *testing.T) {
	h := newHarness(t, "sample-a")
	readQuota(t, h, "sample-a", 0.95, h.clock.Add(time.Hour))
	var records []connectRecord
	forward := newForwardProxy(h.store, h.accounts, func(r connectRecord) error { records = append(records, r); return nil })
	server := httptest.NewServer(forward)
	t.Cleanup(server.Close)
	_, _, res := connect(t, strings.TrimPrefix(server.URL, "http://"), "example.com:443", proxyAuthorization(h.token))
	if res.StatusCode != 429 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if err := forward.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Result != "quota_exhausted" {
		t.Fatalf("records=%+v", records)
	}
	if rows := h.traffic(); len(rows) != 1 || rows[0].result != "quota_exhausted" || rows[0].kind != "connect" || rows[0].sessionID != h.sessionID {
		t.Fatalf("traffic=%+v", rows)
	}
}

// TestLateRateLimitKeepsRefusalPause (P0 of the first review round) runs two in-flight
// requests on A whose responses arrive 401 first, then 429 without a reset. The 401's
// longer pause must survive the 429: a new session after the 429's pause would
// have ended still binds to B, and its refusal is the 503, not the local 429. Control
// arm: the second response is a 200, which pauses nothing and leaves the 401 pause.
func TestLateRateLimitKeepsRefusalPause(t *testing.T) {
	for _, second := range []int{429, 200} {
		t.Run(strconv.Itoa(second), func(t *testing.T) {
			h := newHarness(t)
			// The first request's 401 is held until the second request has reached A, so
			// both are in flight on A; the second response is held until the test has
			// seen the 401's pause on disk.
			secondArrived, release := make(chan struct{}), make(chan struct{})
			var order []int
			var mu sync.Mutex
			exitA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				status := 401
				if len(order) > 0 {
					status = second
				}
				order = append(order, status)
				mu.Unlock()
				if status == 401 {
					<-secondArrived
				} else {
					close(secondArrived)
					<-release
				}
				w.WriteHeader(status)
				if status == http.StatusUnauthorized {
					_, _ = io.WriteString(w, refusedTokenBody("A"))
				}
			}))
			t.Cleanup(exitA.Close)
			exitB := startAccountExit(t, "B", 200)
			exitAURL := exitB.url(t)
			exitAURL.Host = strings.TrimPrefix(exitA.URL, "http://")
			h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", exitAURL)
			h.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", exitB.url(t))
			h.token, h.sessionID = h.issue()
			readQuota(t, h, "sample-b", 0.3, h.clock.Add(6*24*time.Hour))
			server := h.gateway("http://127.0.0.1:1")
			// Both requests of the session go out on A: the second starts before the first answers.
			first := make(chan int)
			go func() { status, _ := post(t, h, server, h.token, `{}`, "", ""); first <- status }()
			for {
				mu.Lock()
				started := len(order)
				mu.Unlock()
				if started == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			done := make(chan int)
			go func() { status, _ := post(t, h, server, h.token, `{}`, "", ""); done <- status }()
			if status := <-first; status != 401 {
				t.Fatalf("first response %d", status)
			}
			if h.paused()["sample-a"] != "upstream_401" {
				t.Fatalf("paused=%v after the 401", h.paused())
			}
			close(release)
			if status := <-done; status != second {
				t.Fatalf("second response %d", status)
			}
			mu.Lock()
			if len(order) != 2 {
				t.Fatalf("A answered %d requests, want 2", len(order))
			}
			mu.Unlock()
			if h.paused()["sample-a"] != "upstream_401" {
				t.Fatalf("paused=%v after the %d", h.paused(), second)
			}
			// Past the 429's pause: A is still refused, and with B paused for a 401 too the answer is the 503.
			h.clock = h.clock.Add(10 * time.Minute)
			token, _ := h.issue()
			if status, _ := post(t, h, server, token, `{}`, "", ""); status != 200 || len(exitB.seen()) != 1 {
				t.Fatalf("new session: status=%d exitB=%d, want 200 on B", status, len(exitB.seen()))
			}
			if err := h.store.Pause(context.Background(), "sample-b", indefinitePause, "upstream_401"); err != nil {
				t.Fatal(err)
			}
			if status, _ := post(t, h, server, token, `{}`, "", ""); status != 503 {
				t.Fatalf("both refused: status=%d, want 503", status)
			}
		})
	}
}

// TestAllPausedByReason (E4.4 control pair) checks the gateway's answer when every
// account is paused: by upstream 429s it is the local 429 with Retry-After to the
// earliest pause end; by upstream 401s it is the 503 for a missing account.
func TestAllPausedByReason(t *testing.T) {
	for _, test := range []struct {
		reason string
		status int
		result string
	}{
		{session.QuotaPauseReason, 429, "quota_exhausted"},
		{"upstream_401", 503, "no_account"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			h, exitA, exitB := twoAccounts(t, 200, 200)
			server := h.gateway("http://127.0.0.1:1")
			earliest := h.clock.Add(10 * time.Minute)
			for alias, until := range map[string]time.Time{"sample-a": earliest.Add(time.Hour), "sample-b": earliest} {
				if err := h.store.Pause(context.Background(), alias, until, test.reason); err != nil {
					t.Fatal(err)
				}
			}
			req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.clientFor(h.token).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != test.status {
				t.Fatalf("status=%d want=%d", res.StatusCode, test.status)
			}
			if test.status == 429 {
				if retry, err := strconv.Atoi(res.Header.Get("Retry-After")); err != nil || retry != 10*60 {
					t.Fatalf("Retry-After=%q err=%v, want %d", res.Header.Get("Retry-After"), err, 10*60)
				}
			}
			if len(exitA.seen())+len(exitB.seen()) != 0 {
				t.Fatal("the refused request reached an exit")
			}
			if rows := h.traffic(); len(rows) != 1 || rows[0].result != test.result {
				t.Fatalf("traffic=%+v", rows)
			}
		})
	}
}
