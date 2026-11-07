package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const (
	setupAlias = "sample-a"
	runA       = "20261107T041325123Z"
)

// setupStore opens a store whose accounts table holds sample-a and sample-b (active) and
// sample-c (testing), and returns it with the clock.
func setupStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	rows := []AccountRow{}
	for _, a := range []struct{ alias, status string }{{"sample-a", "active"}, {"sample-b", "active"}, {"sample-c", "testing"}} {
		rows = append(rows, AccountRow{Alias: a.alias, Email: a.alias + "@example.test", Access: "gateway", Status: a.status, TokenRef: "op://example-vault/token-" + a.alias + "/credential",
			ProxyHost: "proxy.example.test", ProxyPort: 8001, ExpectedIP: "192.0.2.1", UsernameRef: "op://p/u/username", PasswordRef: "op://p/u/password"})
	}
	if _, err := store.ImportAccounts(ctx, rows); err != nil {
		t.Fatal(err)
	}
	return store, c
}

var seq int

func nextID() string {
	seq++
	return fmt.Sprintf("%032x", seq)
}

// request asks for a task of the page for alias and fails the test when it is refused.
func request(t *testing.T, store *Store, alias string) SetupRow {
	t.Helper()
	row, _, err := store.RequestSetup(context.Background(), alias, "login_and_token", nil, nextID())
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestSetupRequestRefusals: a task is refused with the error that says why, and the
// account's task is not changed.
func TestSetupRequestRefusals(t *testing.T) {
	ctx := context.Background()
	store, _ := setupStore(t)
	first := request(t, store, setupAlias)
	rev := int64(1)
	stale := int64(7)
	if _, _, err := store.RequestSetup(ctx, setupAlias, "login_and_token", &rev, nextID()); !errors.Is(err, ErrSetupOpen) {
		t.Fatalf("a second request while one is requested: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "sample-b", "login_and_token", &stale, nextID()); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("stale rev: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "nobody", "login_and_token", nil, nextID()); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	// An account is eligible when it is testing or active and not direct; each other
	// status and a direct exit is refused with its own error.
	for _, test := range []struct {
		status string
		direct bool
		want   error
	}{{"retired", false, ErrSetupStatus}, {"paused", false, ErrSetupStatus}, {"suspended", false, ErrSetupStatus}, {"active", true, ErrSetupDirect}, {"testing", true, ErrSetupDirect}, {"retired", true, ErrSetupStatus}} {
		row, err := store.Account(ctx, "sample-b")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateAccount(ctx, "sample-b", row.Rev, AuditEntry{}, func(r *AccountRow) error { r.Status, r.Direct = test.status, test.direct; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RequestSetup(ctx, "sample-b", "login_and_token", nil, nextID()); !errors.Is(err, test.want) {
			t.Fatalf("%s direct=%v: %v, want %v", test.status, test.direct, err, test.want)
		}
	}
	// Control arm: a testing account and an active one with a proxy are taken.
	row, _ := store.Account(ctx, "sample-b")
	if _, err := store.UpdateAccount(ctx, "sample-b", row.Rev, AuditEntry{}, func(r *AccountRow) error { r.Status, r.Direct = "active", false; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"sample-b", "sample-c"} {
		if _, _, err := store.RequestSetup(ctx, alias, "login_and_token", nil, nextID()); err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
	}
	rows, err := store.SetupRows(ctx)
	if err != nil || rows[setupAlias].RequestID != first.RequestID {
		t.Fatalf("refused requests changed the rows: %+v %v", rows, err)
	}
}

// TestSetupQueueOrder: one task runs at a time, in the order requested.
func TestSetupQueueOrder(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	request(t, store, "sample-a")
	c.Advance(time.Second)
	request(t, store, "sample-b")
	c.Advance(time.Second)
	request(t, store, "sample-c")
	var order []string
	for range 3 {
		task, err := store.NextSetup(ctx)
		if err != nil || task == nil {
			t.Fatalf("task=%v err=%v", task, err)
		}
		order = append(order, task.Alias)
		if again, err := store.NextSetup(ctx); err != nil || again != nil {
			t.Fatalf("a second task while one runs: %v %v", again, err)
		}
		if _, err := store.ReportSetup(ctx, task.Alias, task.RequestID, runA, SetupFailed, "lock", "account_busy"); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(order) != "[sample-a sample-b sample-c]" {
		t.Fatalf("order = %v", order)
	}
	if task, err := store.NextSetup(ctx); err != nil || task != nil {
		t.Fatalf("an empty queue gave %v %v", task, err)
	}
}

// TestSetupReports: the first report sets the run ID that every later report and the
// handover must carry; a report of another task, of a closed one or of a claimed one is
// refused and changes nothing.
func TestSetupReports(t *testing.T) {
	ctx := context.Background()
	store, _ := setupStore(t)
	task := func() *SetupTask {
		request(t, store, setupAlias)
		got, err := store.NextSetup(ctx)
		if err != nil || got == nil {
			t.Fatal(err)
		}
		return got
	}()
	if _, err := store.ReportSetup(ctx, setupAlias, nextID(), runA, SetupRunning, "lock", ""); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("wrong request ID: %v", err)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("a handover before any report: %v", err)
	}
	row, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "lock", "")
	if err != nil || row.RunID != runA || row.Step != "lock" || row.State != SetupRunning {
		t.Fatalf("first report: %+v %v", row, err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, "20261107T041325124Z", SetupRunning, "browser", ""); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("another run ID: %v", err)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, "20261107T041325124Z"); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("handover with another run ID: %v", err)
	}
	if err := store.CheckHandover(ctx, "sample-b", task.RequestID, runA); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("handover for an account with no task: %v", err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a second handover: %v", err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "browser", ""); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a report after the claim: %v", err)
	}
	if _, err := store.FinishSetup(ctx, setupAlias, task.RequestID, SetupDone, "complete", "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "browser", ""); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a report to a closed task: %v", err)
	}
	if _, err := store.FinishSetup(ctx, setupAlias, task.RequestID, SetupDone, "complete", "complete"); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a second finish: %v", err)
	}
	// A closed task blocks no new one.
	request(t, store, setupAlias)
}

// TestSetupTimeouts: a running task fails after 30 minutes without a report or 60 minutes
// after it was handed out, a claimed one 5 minutes after the claim, a requested one waits
// as long as it takes and ends only when its account cannot have a setup any more; each just before its
// time still stands.
func TestSetupTimeouts(t *testing.T) {
	ctx := context.Background()
	state := func(store *Store, alias string) SetupRow {
		rows, err := store.SetupRows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return rows[alias]
	}
	running := func(store *Store, c *clock) *SetupTask {
		request(t, store, setupAlias)
		task, err := store.NextSetup(ctx)
		if err != nil || task == nil {
			t.Fatal(err)
		}
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "lock", ""); err != nil {
			t.Fatal(err)
		}
		return task
	}
	t.Run("no report for 30 minutes", func(t *testing.T) {
		store, c := setupStore(t)
		running(store, c)
		c.Advance(30*time.Minute - time.Second)
		_ = store.ExpireSetups(ctx)
		if got := state(store, setupAlias); got.State != SetupRunning {
			t.Fatalf("29:59 = %s", got.State)
		}
		c.Advance(time.Second)
		_ = store.ExpireSetups(ctx)
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "timeout" || got.Step != "lock" {
			t.Fatalf("30:00 = %+v", got)
		}
	})
	t.Run("reports keep it alive up to 60 minutes", func(t *testing.T) {
		store, c := setupStore(t)
		task := running(store, c)
		for range 5 {
			c.Advance(11 * time.Minute)
			if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "cli", ""); err != nil {
				t.Fatal(err)
			}
		}
		// 55 minutes in, the last report 0 minutes ago.
		c.Advance(5*time.Minute - time.Second)
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "cli", ""); err != nil {
			t.Fatalf("59:59 in: %v", err)
		}
		c.Advance(time.Second)
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "cli", ""); !errors.Is(err, ErrSetupNoOpenTask) {
			t.Fatalf("60:00 in: %v", err)
		}
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "timeout" {
			t.Fatalf("after 60 minutes = %+v", got)
		}
	})
	t.Run("handover over 5 minutes after the claim", func(t *testing.T) {
		store, c := setupStore(t)
		task := running(store, c)
		if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); err != nil {
			t.Fatal(err)
		}
		// The 30 and 60 minute limits no longer apply to it, the 5 minutes do.
		c.Advance(5*time.Minute - time.Second)
		_ = store.ExpireSetups(ctx)
		if got := state(store, setupAlias); got.State != SetupRunning {
			t.Fatalf("4:59 = %s", got.State)
		}
		c.Advance(time.Second)
		_ = store.ExpireSetups(ctx)
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "timeout" {
			t.Fatalf("5:00 = %+v", got)
		}
		if err := store.SetupStep(ctx, setupAlias, task.RequestID, "write"); !errors.Is(err, ErrSetupNoOpenTask) {
			t.Fatalf("a step after the timeout: %v", err)
		}
	})
	for _, test := range []struct {
		name   string
		apply  func(row *AccountRow)
		reason string
	}{
		{"retired", func(row *AccountRow) { row.Status = "retired" }, "account_status"},
		{"paused", func(row *AccountRow) { row.Status = "paused" }, "account_status"},
		{"direct", func(row *AccountRow) { row.Direct = true }, "direct_account"},
		{"active", func(row *AccountRow) { row.Status = "active" }, ""},
	} {
		t.Run("requested waits, and ends when the account is "+test.name, func(t *testing.T) {
			store, c := setupStore(t)
			request(t, store, setupAlias)
			c.Advance(30 * 24 * time.Hour)
			_ = store.ExpireSetups(ctx)
			if got := state(store, setupAlias); got.State != SetupRequested {
				t.Fatalf("after a month = %s", got.State)
			}
			if _, err := store.UpdateAccount(ctx, setupAlias, 1, AuditEntry{}, func(row *AccountRow) error { test.apply(row); return nil }); err != nil {
				t.Fatal(err)
			}
			task, err := store.NextSetup(ctx)
			if test.reason == "" {
				// Control arm: an account that stays eligible gets its task.
				if err != nil || task == nil {
					t.Fatalf("an eligible account's task was not handed out: %v %v", task, err)
				}
				return
			}
			if err != nil || task != nil {
				t.Fatalf("an ineligible account's task was handed out: %v %v", task, err)
			}
			if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != test.reason {
				t.Fatalf("task = %+v, want failed with %s", got, test.reason)
			}
		})
	}
	for _, test := range []struct {
		name   string
		apply  func(row *AccountRow)
		reason string
		claim  bool
	}{
		{"retired", func(row *AccountRow) { row.Status = "retired" }, "account_status", false},
		{"direct", func(row *AccountRow) { row.Direct = true }, "direct_account", false},
		{"active", func(row *AccountRow) { row.Status = "active" }, "", false},
		{"retired after the claim", func(row *AccountRow) { row.Status = "retired" }, "", true},
	} {
		t.Run("running task takes its handover only while the account is eligible: "+test.name, func(t *testing.T) {
			store, c := setupStore(t)
			task := running(store, c)
			if test.claim {
				if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.UpdateAccount(ctx, setupAlias, 1, AuditEntry{}, func(row *AccountRow) error { test.apply(row); return nil }); err != nil {
				t.Fatal(err)
			}
			err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA)
			switch {
			case test.claim:
				// A handover under way is not cut off by the account's change.
				if !errors.Is(err, ErrSetupNoOpenTask) || state(store, setupAlias).State != SetupRunning {
					t.Fatalf("claimed task: err=%v state=%s", err, state(store, setupAlias).State)
				}
			case test.reason == "":
				if err != nil {
					t.Fatalf("an eligible account's handover was refused: %v", err)
				}
			default:
				if !errors.Is(err, ErrSetupNoOpenTask) {
					t.Fatalf("an ineligible account's handover was taken: %v", err)
				}
				if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != test.reason {
					t.Fatalf("task = %+v, want failed with %s", got, test.reason)
				}
			}
		})
	}
	t.Run("a claimed task at start is interrupted, an unclaimed one is not", func(t *testing.T) {
		store, c := setupStore(t)
		task := running(store, c)
		if n, err := store.InterruptSetups(ctx); err != nil || n != 0 {
			t.Fatalf("unclaimed: n=%d err=%v", n, err)
		}
		if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); err != nil {
			t.Fatal(err)
		}
		if n, err := store.InterruptSetups(ctx); err != nil || n != 1 {
			t.Fatalf("claimed: n=%d err=%v", n, err)
		}
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "interrupted" {
			t.Fatalf("after a restart = %+v", got)
		}
	})
}

// TestSetupTokenDate: the gateway notes when it wrote a token, which stays through later
// tasks; an account the gateway wrote none for has none.
func TestSetupTokenDate(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	rows, _ := store.SetupRows(ctx)
	if got := rows[setupAlias]; !got.TokenSetAt.IsZero() {
		t.Fatalf("a date before any write: %+v", got)
	}
	request(t, store, setupAlias)
	task, _ := store.NextSetup(ctx)
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SetupRunning, "lock", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteTokenWritten(ctx, setupAlias, task.RequestID); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a write noted for a task no handover claimed: %v", err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if err := store.NoteTokenWritten(ctx, setupAlias, task.RequestID); err != nil {
		t.Fatal(err)
	}
	written := c.Now()
	if _, err := store.FinishSetup(ctx, setupAlias, task.RequestID, SetupDone, "complete", "complete"); err != nil {
		t.Fatal(err)
	}
	c.Advance(24 * time.Hour)
	request(t, store, setupAlias)
	rows, _ = store.SetupRows(ctx)
	if got := rows[setupAlias]; !got.TokenSetAt.Equal(written) || got.State != SetupRequested {
		t.Fatalf("after a new request: %+v", got)
	}
	if got := rows["sample-b"]; !got.TokenSetAt.IsZero() {
		t.Fatalf("another account has a date: %+v", got)
	}
}
