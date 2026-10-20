package claude

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestRenewalQueue checks the queue of forced renewals: an account queued twice is
// renewed once, a failed renewal is retried after a minute, then two, doubling up to an
// hour, and a finished one leaves the queue.
func TestRenewalQueue(t *testing.T) {
	var q renewals
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	q.queue("sample-a", start)
	q.queue("sample-a", start.Add(-time.Hour))
	if due, _ := q.due(start); len(due) != 1 || !due["sample-a"].since.Equal(start) {
		t.Fatalf("due %v, want sample-a once", due)
	}
	now := start
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		q.retry("sample-a", now, false)
		due, next := q.due(now.Add(want - time.Second))
		if len(due) != 0 || !next.Equal(now.Add(want)) {
			t.Fatalf("failure %d: due %v, next %v, want after %v", i+1, due, next, want)
		}
		now = next
	}
	q.retry("sample-a", now, true)
	if due, next := q.due(now.Add(24 * time.Hour)); len(due) != 0 || !next.IsZero() || q.queued("sample-a") {
		t.Fatalf("after success: due %v, next %v", due, next)
	}
}

// TestRenewalRetry checks a queued renewal that fails other than refused: its retry
// reaches the provider as a refusal at the time of that failure, so a provider whose
// renewal that settled after a refusal stands for it (as Renew's contract says) sends
// a new request, and its success ends the upstream_401 pause. The renewal settles a
// second after the 401, so a retry that kept the 401's time would take the failure.
func TestRenewalRetry(t *testing.T) {
	h := newHarness(t, "sample-a")
	rt := h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:9"}, accountTransport{})
	var settled time.Time
	var outcome error
	var sent int
	rt.provider.Renew = func(_ context.Context, _ string, _ *http.Client, refusedAt time.Time) error {
		if settled.After(refusedAt) {
			return outcome
		}
		sent++
		outcome, settled = nil, h.clock
		if sent == 1 {
			outcome = errors.New("the token endpoint answered 503")
		}
		return outcome
	}
	if err := h.store.Pause(t.Context(), "sample-a", indefinitePause, "upstream_401"); err != nil {
		t.Fatal(err)
	}
	rt.renewals.queue("sample-a", h.clock)
	h.clock = h.clock.Add(time.Second)
	rt.renewQueued(t.Context(), h.clock)
	_, next := rt.renewals.due(h.clock)
	if sent != 1 || h.paused()["sample-a"] != "upstream_401" || !next.Equal(h.clock.Add(time.Minute)) {
		t.Fatalf("first renewal: %d requests, paused %v, next %v", sent, h.paused(), next)
	}
	h.clock = next
	rt.renewQueued(t.Context(), h.clock)
	if sent != 2 || len(h.paused()) != 0 || rt.renewals.queued("sample-a") {
		t.Fatalf("retry: %d requests, paused %v, queued %v; want 2, none, false", sent, h.paused(), rt.renewals.queued("sample-a"))
	}
}
