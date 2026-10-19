package claude

import (
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
