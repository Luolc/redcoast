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
func request(t *testing.T, store *Store, alias, kind string) SetupRow {
	t.Helper()
	row, _, err := store.RequestSetup(context.Background(), alias, "login_and_token", kind, nil, nextID())
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
	first := request(t, store, setupAlias, SetupKindPage)
	rev := int64(1)
	stale := int64(7)
	if _, _, err := store.RequestSetup(ctx, setupAlias, "token_only", SetupKindPage, &rev, nextID()); !errors.Is(err, ErrSetupOpen) {
		t.Fatalf("a second request while one is requested: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "sample-b", "token_only", SetupKindPage, &stale, nextID()); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("stale rev: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "nobody", "token_only", SetupKindPage, nil, nextID()); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := store.UpdateAccount(ctx, "sample-b", 1, AuditEntry{}, func(row *AccountRow) error { row.Status = "retired"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RequestSetup(ctx, "sample-b", "token_only", SetupKindPage, nil, nextID()); !errors.Is(err, ErrSetupRetired) {
		t.Fatalf("retired account: %v", err)
	}
	rows, err := store.SetupRows(ctx)
	if err != nil || len(rows) != 1 || rows[setupAlias].RequestID != first.RequestID || rows[setupAlias].State != SetupRequested {
		t.Fatalf("refused requests changed the rows: %+v %v", rows, err)
	}
}

// TestSetupQueueOrder: one task runs at a time, the page's tasks go before renewals and
// each kind in the order requested.
func TestSetupQueueOrder(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	request(t, store, "sample-a", SetupKindRenewal)
	c.Advance(time.Second)
	request(t, store, "sample-b", SetupKindPage)
	c.Advance(time.Second)
	request(t, store, "sample-c", SetupKindPage)
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
	if fmt.Sprint(order) != "[sample-b sample-c sample-a]" {
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
		request(t, store, setupAlias, SetupKindPage)
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
	request(t, store, setupAlias, SetupKindPage)
}

// TestSetupTimeouts: a running task fails after 30 minutes without a report or 60 minutes
// after it was handed out, a claimed one 5 minutes after the claim, a requested one waits
// as long as it takes and ends only when its account is retired; each just before its
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
		request(t, store, setupAlias, SetupKindPage)
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
	t.Run("requested waits, and ends when the account is retired", func(t *testing.T) {
		store, c := setupStore(t)
		request(t, store, setupAlias, SetupKindPage)
		c.Advance(30 * 24 * time.Hour)
		_ = store.ExpireSetups(ctx)
		if got := state(store, setupAlias); got.State != SetupRequested {
			t.Fatalf("after a month = %s", got.State)
		}
		if _, err := store.UpdateAccount(ctx, setupAlias, 1, AuditEntry{}, func(row *AccountRow) error { row.Status = "retired"; return nil }); err != nil {
			t.Fatal(err)
		}
		if task, err := store.NextSetup(ctx); err != nil || task != nil {
			t.Fatalf("a retired account's task was handed out: %v %v", task, err)
		}
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "account_retired" {
			t.Fatalf("retired = %+v", got)
		}
	})
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

// TestSetupRenewals: a renewal is queued for an active or paused account whose token is
// due within 30 days and that has no open task and no renewal that failed in the last 24
// hours, and for no other; /health lists the accounts due within 7 days.
func TestSetupRenewals(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	// sample-a was set 340 days ago (due in 25 days), sample-b 300 days ago (due in 65),
	// sample-c, which is testing, 400 days ago.
	for alias, age := range map[string]time.Duration{"sample-a": 340 * 24 * time.Hour, "sample-b": 300 * 24 * time.Hour, "sample-c": 400 * 24 * time.Hour} {
		if err := store.SetTokenEstimate(ctx, alias, c.Now().Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := store.QueueRenewals(ctx, nextID)
	if err != nil || fmt.Sprint(queued) != "[sample-a]" {
		t.Fatalf("queued = %v %v", queued, err)
	}
	if again, err := store.QueueRenewals(ctx, nextID); err != nil || len(again) != 0 {
		t.Fatalf("a second sweep queued %v %v", again, err)
	}
	rows, _ := store.SetupRows(ctx)
	if got := rows["sample-a"]; got.State != SetupRequested || got.Action != "token_only" || got.Kind != SetupKindRenewal || got.TokenSetAt.IsZero() {
		t.Fatalf("renewal row = %+v", got)
	}
	due, err := store.TokensDue(ctx, 7*24*time.Hour)
	if err != nil || len(due) != 0 {
		t.Fatalf("due in 7 days: %v %v", due, err)
	}
	// The renewal fails: no new one for 24 hours, and then one.
	task, _ := store.NextSetup(ctx)
	if _, err := store.ReportSetup(ctx, "sample-a", task.RequestID, runA, SetupFailed, "lock", "account_busy"); err != nil {
		t.Fatal(err)
	}
	c.Advance(24*time.Hour - time.Second)
	if q, _ := store.QueueRenewals(ctx, nextID); len(q) != 0 {
		t.Fatalf("a renewal queued 23:59:59 after a failure: %v", q)
	}
	c.Advance(time.Second)
	if q, _ := store.QueueRenewals(ctx, nextID); fmt.Sprint(q) != "[sample-a]" {
		t.Fatalf("no renewal 24 hours after a failure: %v", q)
	}
	// A page task that failed does not hold a renewal back.
	task, _ = store.NextSetup(ctx)
	if _, err := store.ReportSetup(ctx, "sample-a", task.RequestID, runA, SetupNeedsHuman, "lock", "captcha_challenge"); err != nil {
		t.Fatal(err)
	}
	if q, _ := store.QueueRenewals(ctx, nextID); fmt.Sprint(q) != "[sample-a]" {
		t.Fatalf("needs_human held a renewal back: %v", q)
	}
	// 20 days later sample-a is overdue, sample-b is due in 45.
	c.Advance(20 * 24 * time.Hour)
	due, _ = store.TokensDue(ctx, 7*24*time.Hour)
	if len(due) != 1 || due[0].Alias != "sample-a" {
		t.Fatalf("due = %v", due)
	}
	// A token written now is not due.
	task, _ = store.NextSetup(ctx)
	if _, err := store.ReportSetup(ctx, "sample-a", task.RequestID, runA, SetupRunning, "lock", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimHandover(ctx, "sample-a", task.RequestID, runA); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteTokenWritten(ctx, "sample-a", task.RequestID); err != nil {
		t.Fatal(err)
	}
	if due, _ := store.TokensDue(ctx, 7*24*time.Hour); len(due) != 0 {
		t.Fatalf("a token written now is due: %v", due)
	}
	// An estimate does not replace the time of a token the gateway wrote.
	if err := store.SetTokenEstimate(ctx, "sample-a", c.Now().Add(-400*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, _ = store.SetupRows(ctx)
	if got := rows["sample-a"]; got.TokenSetEstimated || !got.TokenSetAt.Equal(c.Now()) {
		t.Fatalf("estimate replaced the written time: %+v", got)
	}
}
