package session

import (
	"context"
	"testing"
)

// TestMigrateBindings checks the reload migration: every live session bound to a
// departing account is rebound at once to a candidate with the reason in its history,
// in one transaction; with no candidate left the sessions lose their binding and are
// counted, and the control arm (nothing departing) changes nothing.
func TestMigrateBindings(t *testing.T) {
	store, _ := open(t, newClock())
	// Selection balances active sessions, so the three sessions spread over both
	// accounts; the test groups them by where they landed.
	onA := map[string]bool{}
	for range 3 {
		_, b, err := newSession(t, store)
		if err != nil {
			t.Fatal(err)
		}
		if b.Alias == "sample-a" {
			onA[b.SessionID] = true
		}
	}
	if len(onA) == 0 || len(onA) == 3 {
		t.Fatalf("%d of 3 sessions on sample-a, want both accounts in use", len(onA))
	}
	if m, err := store.MigrateBindings(context.Background(), nil, []string{"sample-b"}, "reload"); err != nil || m != (Migration{}) {
		t.Fatalf("control arm changed something: %+v %v", m, err)
	}
	m, err := store.MigrateBindings(context.Background(), []string{"sample-a"}, []string{"sample-b"}, "reload")
	if err != nil || m != (Migration{Moved: len(onA)}) {
		t.Fatalf("got %+v %v, want %d sessions moved", m, err, len(onA))
	}
	for id := range onA {
		if alias, _ := binding(t, store, id); alias != "sample-b" {
			t.Fatalf("session %s bound to %s after the migration", id, alias)
		}
		if count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND reason = 'reload' AND to_ts IS NULL", id) != 1 ||
			count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND to_ts IS NULL", id) != 1 {
			t.Fatalf("session %s: history not closed and reopened with reason reload", id)
		}
	}
	// Nothing left to go to: the bindings go, the history closes, the count says so.
	m, err = store.MigrateBindings(context.Background(), []string{"sample-b"}, nil, "reload")
	if err != nil || m != (Migration{Unplaced: 3}) {
		t.Fatalf("got %+v %v, want three sessions unplaced", m, err)
	}
	if count(t, store, "SELECT COUNT(*) FROM bindings") != 0 || count(t, store, "SELECT COUNT(*) FROM binding_history WHERE to_ts IS NULL") != 0 {
		t.Fatal("unplaced sessions keep a binding or an open history row")
	}
	// The unplaced sessions are still live and select anew on their next request.
	if count(t, store, "SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL") != 3 {
		t.Fatal("migration ended sessions")
	}
}
