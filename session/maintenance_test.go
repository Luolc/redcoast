package session

import (
	"context"
	"testing"
	"time"
)

const day = 24 * time.Hour

// TestPrune deletes rows past their retention and keeps the rest: traffic by age,
// closed binding_history by age, and ended sessions by age unless traffic still refers
// to them.
func TestPrune(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	start := c.Now()
	bound := func() Issued {
		t.Helper()
		issued, err := store.Issue(ctx, "m1", LocalSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Bind(ctx, issued.Token, accounts); err != nil {
			t.Fatal(err)
		}
		return issued
	}
	traffic := func(sessionID string, at time.Time) {
		t.Helper()
		if err := store.RecordTraffic(ctx, Traffic{At: at, SessionID: sessionID, Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	revoke := func(issued Issued) {
		t.Helper()
		if err := store.Revoke(ctx, issued.Token, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// a and b end at the start; only b has traffic.
	a, b := bound(), bound()
	traffic(b.ID, start)
	traffic("", start)
	revoke(a)
	revoke(b)
	// d ends two days later; live never ends.
	c.Advance(2 * day)
	d := bound()
	revoke(d)
	live := bound()
	c.Advance(364 * day)
	traffic(live.ID, c.Now().Add(-179*day))

	// Traffic kept longer than sessions: b is still referred to.
	pruned, err := store.Prune(ctx, Retention{Traffic: 400 * day, Sessions: 365 * day})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != (Pruned{Traffic: 0, History: 2, Sessions: 1}) {
		t.Fatalf("first prune: %+v", pruned)
	}
	for id, want := range map[string]int{a.ID: 0, b.ID: 1, d.ID: 1, live.ID: 1} {
		if got := count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ?", id); got != want {
			t.Errorf("session rows of %s = %d, want %d", id, got, want)
		}
	}
	if got := count(t, store, "SELECT COUNT(*) FROM bindings WHERE session_id = ?", a.ID); got != 0 {
		t.Errorf("bindings row of the pruned session survived: %d", got)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id IN (?, ?)", d.ID, live.ID); got != 2 {
		t.Errorf("binding_history within retention = %d, want 2", got)
	}

	// The defaults: the traffic of the start goes, then b with it; the -179 day row stays.
	pruned, err = store.Prune(ctx, Retention{Traffic: DefaultTrafficRetention, Sessions: DefaultSessionRetention})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != (Pruned{Traffic: 2, History: 0, Sessions: 1}) {
		t.Fatalf("second prune: %+v", pruned)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM traffic WHERE session_id = ?", live.ID); got != 1 {
		t.Errorf("traffic within retention = %d, want 1", got)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Errorf("sessions left = %d, want d and live", got)
	}
}

// TestPruneInBatches deletes a backlog larger than one batch.
func TestPruneInBatches(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	for range pruneBatch*2 + 1 {
		if err := store.RecordTraffic(ctx, Traffic{At: c.Now(), Kind: "connect", Host: "h", Port: 443, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	c.Advance(DefaultTrafficRetention + day)
	pruned, err := store.Prune(ctx, Retention{Traffic: DefaultTrafficRetention, Sessions: DefaultSessionRetention})
	if err != nil || pruned.Traffic != pruneBatch*2+1 {
		t.Fatalf("pruned=%+v err=%v", pruned, err)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM traffic"); got != 0 {
		t.Fatalf("traffic left = %d", got)
	}
}

// TestVacuumIfSparse rebuilds the file only after deletions freed a quarter of it.
func TestVacuumIfSparse(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	if vacuumed, err := store.VacuumIfSparse(ctx); err != nil || vacuumed {
		t.Fatalf("fresh file: vacuumed=%t err=%v", vacuumed, err)
	}
	for range 2000 {
		if err := store.RecordTraffic(ctx, Traffic{At: c.Now(), Kind: "connect", Host: "example.invalid", Port: 443, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("DELETE FROM traffic"); err != nil {
		t.Fatal(err)
	}
	if vacuumed, err := store.VacuumIfSparse(ctx); err != nil || !vacuumed {
		t.Fatalf("after deleting: vacuumed=%t err=%v", vacuumed, err)
	}
	if got := count(t, store, "SELECT freelist_count FROM pragma_freelist_count"); got != 0 {
		t.Fatalf("free pages after vacuum = %d", got)
	}
}

// TestRunRetentionKeepsLastRun checks that the first run's outcome is kept in memory.
func TestRunRetentionKeepsLastRun(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	if !store.LastRetention().At.IsZero() {
		t.Fatal("a retention run before any ran")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.RunRetention(ctx, Retention{Traffic: time.Hour, Sessions: time.Hour}) }()
	for store.LastRetention().At.IsZero() {
		select {
		case err := <-done:
			t.Fatalf("RunRetention returned %v before its first run", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if run := store.LastRetention(); !run.At.Equal(c.Now()) || run.Error != "" {
		t.Fatalf("last run = %+v", run)
	}
}
