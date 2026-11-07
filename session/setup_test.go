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
	row, _, err := store.RequestSetup(context.Background(), alias, "login_and_token", nil, nextID(), Source{})
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
	if _, _, err := store.RequestSetup(ctx, setupAlias, "login_and_token", &rev, nextID(), Source{}); !errors.Is(err, ErrSetupOpen) {
		t.Fatalf("a second request while one is requested: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "sample-b", "login_and_token", &stale, nextID(), Source{}); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("stale rev: %v", err)
	}
	if _, _, err := store.RequestSetup(ctx, "nobody", "login_and_token", nil, nextID(), Source{}); !errors.Is(err, ErrAccountNotFound) {
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
		if _, _, err := store.RequestSetup(ctx, "sample-b", "login_and_token", nil, nextID(), Source{}); !errors.Is(err, test.want) {
			t.Fatalf("%s direct=%v: %v, want %v", test.status, test.direct, err, test.want)
		}
	}
	// Control arm: a testing account and an active one with a proxy are taken.
	row, _ := store.Account(ctx, "sample-b")
	if _, err := store.UpdateAccount(ctx, "sample-b", row.Rev, AuditEntry{}, func(r *AccountRow) error { r.Status, r.Direct = "active", false; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"sample-b", "sample-c"} {
		if _, _, err := store.RequestSetup(ctx, alias, "login_and_token", nil, nextID(), Source{}); err != nil {
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
		if _, err := store.ReportSetup(ctx, task.Alias, task.RequestID, runA, SourceDashboard, SetupFailed, "lock", "account_busy", nil); err != nil {
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
	if _, err := store.ReportSetup(ctx, setupAlias, nextID(), runA, SourceDashboard, SetupRunning, "lock", "", nil); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("wrong request ID: %v", err)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("a handover before any report: %v", err)
	}
	row, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "lock", "", nil)
	if err != nil || row.RunID != runA || row.Step != "lock" || row.State != SetupRunning {
		t.Fatalf("first report: %+v %v", row, err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, "20261107T041325124Z", SourceDashboard, SetupRunning, "browser", "", nil); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("another run ID: %v", err)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, "20261107T041325124Z", SourceDashboard); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("handover with another run ID: %v", err)
	}
	if err := store.CheckHandover(ctx, "sample-b", task.RequestID, runA, SourceDashboard); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("handover for an account with no task: %v", err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a second handover: %v", err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "browser", "", nil); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a report after the claim: %v", err)
	}
	if _, err := store.FinishSetup(ctx, setupAlias, task.RequestID, SetupDone, "complete", "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "browser", "", nil); !errors.Is(err, ErrSetupNoOpenTask) {
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
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "lock", "", nil); err != nil {
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
			if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "cli", "", nil); err != nil {
				t.Fatal(err)
			}
		}
		// 55 minutes in, the last report 0 minutes ago.
		c.Advance(5*time.Minute - time.Second)
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "cli", "", nil); err != nil {
			t.Fatalf("59:59 in: %v", err)
		}
		c.Advance(time.Second)
		if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "cli", "", nil); !errors.Is(err, ErrSetupNoOpenTask) {
			t.Fatalf("60:00 in: %v", err)
		}
		if got := state(store, setupAlias); got.State != SetupFailed || got.Reason != "timeout" {
			t.Fatalf("after 60 minutes = %+v", got)
		}
	})
	t.Run("handover over 5 minutes after the claim", func(t *testing.T) {
		store, c := setupStore(t)
		task := running(store, c)
		if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); err != nil {
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
				if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.UpdateAccount(ctx, setupAlias, 1, AuditEntry{}, func(row *AccountRow) error { test.apply(row); return nil }); err != nil {
				t.Fatal(err)
			}
			err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard)
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
		if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); err != nil {
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
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "lock", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteTokenWritten(ctx, setupAlias, task.RequestID); !errors.Is(err, ErrSetupNoOpenTask) {
		t.Fatalf("a write noted for a task no handover claimed: %v", err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); err != nil {
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

// stateOf returns the latest task of alias.
func stateOf(t *testing.T, store *Store, alias string) SetupRow {
	t.Helper()
	rows, err := store.SetupRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows[alias]
}

var slackSource = Source{Kind: SourceSlack, Thread: "C0TESTCHAN1/1700000000.000100", TS: "1700000000.000200"}

// requestFrom asks for a task of alias from source.
func requestFrom(t *testing.T, store *Store, alias string, source Source) SetupRow {
	t.Helper()
	row, _, err := store.RequestSetup(context.Background(), alias, "login_and_token", nil, nextID(), source)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestSetupSources: a task keeps where it was asked for; the next task for the login
// machine at the desk is a dashboard one and never a Slack one, and the task by ID is a
// Slack one that waits and is alone. The control arms are the tasks that are handed out.
func TestSetupSources(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	slack := requestFrom(t, store, "sample-a", slackSource)
	c.Advance(time.Second)
	dashboard := requestFrom(t, store, "sample-b", Source{})
	if slack.Source != slackSource || dashboard.Source.Kind != SourceDashboard || dashboard.Source.Thread != "" {
		t.Fatalf("sources = %+v, %+v", slack.Source, dashboard.Source)
	}
	// The Slack task is older, and still the desk gets the dashboard one.
	task, err := store.NextSetup(ctx)
	if err != nil || task == nil || task.RequestID != dashboard.RequestID || task.Source.Kind != SourceDashboard {
		t.Fatalf("NextSetup gave %+v, %v", task, err)
	}
	if again, err := store.NextSetup(ctx); err != nil || again != nil {
		t.Fatalf("NextSetup with only a Slack task waiting and another running: %+v %v", again, err)
	}
	// By ID: not found, not Slack, running elsewhere (nil), then handed out once.
	if _, err := store.NextSetupByID(ctx, nextID()); !errors.Is(err, ErrSetupTaskNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
	if _, err := store.NextSetupByID(ctx, dashboard.RequestID); !errors.Is(err, ErrSetupNotSlack) {
		t.Fatalf("dashboard task by ID: %v", err)
	}
	if got, err := store.NextSetupByID(ctx, slack.RequestID); err != nil || got != nil {
		t.Fatalf("while another task runs: %+v %v", got, err)
	}
	if _, err := store.ReportSetup(ctx, "sample-b", dashboard.RequestID, runA, SourceDashboard, SetupFailed, "lock", "account_busy", nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.NextSetupByID(ctx, slack.RequestID)
	if err != nil || got == nil || got.Alias != "sample-a" || got.State != SetupRunning || got.Source != slackSource {
		t.Fatalf("by ID: %+v %v", got, err)
	}
	if _, err := store.NextSetupByID(ctx, slack.RequestID); !errors.Is(err, ErrSetupNotRequested) {
		t.Fatalf("a second time: %v", err)
	}
	// The source stays in the row through the report and the table read.
	rows, _ := store.SetupRows(ctx)
	if rows["sample-a"].Source != slackSource {
		t.Fatalf("row source = %+v", rows["sample-a"].Source)
	}
	// A newer task of the account takes the place of the old one: its ID is gone.
	if _, err := store.ReportSetup(ctx, "sample-a", slack.RequestID, runA, SourceSlack, SetupFailed, "lock", "account_busy", nil); err != nil {
		t.Fatal(err)
	}
	requestFrom(t, store, "sample-a", slackSource)
	if _, err := store.NextSetupByID(ctx, slack.RequestID); !errors.Is(err, ErrSetupTaskNotFound) {
		t.Fatalf("the replaced task's ID: %v", err)
	}
}

// TestSetupSourceMustMatch: a report, a handover check and a claim that say the task came
// from another source are refused after the IDs are checked, and change nothing; the
// right source is taken.
func TestSetupSourceMustMatch(t *testing.T) {
	ctx := context.Background()
	store, _ := setupStore(t)
	task := requestFrom(t, store, setupAlias, slackSource)
	if got, err := store.NextSetupByID(ctx, task.RequestID); err != nil || got == nil {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, SetupRunning, "lock", "", nil); !errors.Is(err, ErrSetupSourceMismatch) {
		t.Fatalf("report from the wrong source: %v", err)
	}
	if got := stateOf(t, store, setupAlias); got.RunID != "" || got.Step != "" {
		t.Fatalf("a refused report changed the task: %+v", got)
	}
	// The IDs are checked first.
	if _, err := store.ReportSetup(ctx, setupAlias, nextID(), runA, SourceDashboard, SetupRunning, "lock", "", nil); !errors.Is(err, ErrSetupIDsMismatch) {
		t.Fatalf("wrong request ID and wrong source: %v", err)
	}
	if _, err := store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceSlack, SetupRunning, "lock", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); !errors.Is(err, ErrSetupSourceMismatch) {
		t.Fatalf("check from the wrong source: %v", err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceDashboard); !errors.Is(err, ErrSetupSourceMismatch) {
		t.Fatalf("claim from the wrong source: %v", err)
	}
	if got := stateOf(t, store, setupAlias); !got.ClaimedAt.IsZero() {
		t.Fatalf("a refused claim claimed: %+v", got)
	}
	if err := store.CheckHandover(ctx, setupAlias, task.RequestID, runA, SourceSlack); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimHandover(ctx, setupAlias, task.RequestID, runA, SourceSlack); err != nil {
		t.Fatal(err)
	}
}

// TestSetupWaitUntilStore: a wait is for the running state at the captcha step, over
// the next 20 minutes at most, once per task whatever happens to it after; another step
// clears it, a report without one leaves it.
func TestSetupWaitUntilStore(t *testing.T) {
	ctx := context.Background()
	store, c := setupStore(t)
	task := request(t, store, setupAlias)
	if _, err := store.NextSetup(ctx); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) *time.Time { w := c.Now().Add(d); return &w }
	report := func(state, step string, wait *time.Time) (SetupRow, error) {
		reason := ""
		if state == SetupFailed {
			reason = "account_busy"
		}
		return store.ReportSetup(ctx, setupAlias, task.RequestID, runA, SourceDashboard, state, step, reason, wait)
	}
	for _, test := range []struct {
		name        string
		state, step string
		wait        *time.Time
	}{
		{"other step", SetupRunning, "lock", at(time.Minute)},
		{"other state", SetupFailed, SetupStepCaptcha, at(time.Minute)},
		{"now", SetupRunning, SetupStepCaptcha, at(0)},
		{"past", SetupRunning, SetupStepCaptcha, at(-time.Second)},
		{"21 minutes", SetupRunning, SetupStepCaptcha, at(20*time.Minute + time.Millisecond)},
	} {
		if _, err := report(test.state, test.step, test.wait); !errors.Is(err, ErrSetupWaitUntil) {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
	if got := stateOf(t, store, setupAlias); got.RunID != "" || got.WaitUntilSet {
		t.Fatalf("refused waits changed the task: %+v", got)
	}
	row, err := report(SetupRunning, SetupStepCaptcha, at(20*time.Minute))
	if err != nil || !row.WaitUntil.Equal(c.Now().Add(20*time.Minute)) || !row.WaitUntilSet {
		t.Fatalf("20 minutes: %+v %v", row, err)
	}
	if _, err := report(SetupRunning, SetupStepCaptcha, at(time.Minute)); !errors.Is(err, ErrSetupWaitUntil) {
		t.Fatalf("a second wait: %v", err)
	}
	if row, err := report(SetupRunning, SetupStepCaptcha, nil); err != nil || row.WaitUntil.IsZero() {
		t.Fatalf("a report without one: %+v %v", row, err)
	}
	row, err = report(SetupRunning, "email_sent", nil)
	if err != nil || !row.WaitUntil.IsZero() || !row.WaitUntilSet {
		t.Fatalf("another step: %+v %v", row, err)
	}
	if _, err := report(SetupRunning, SetupStepCaptcha, at(time.Minute)); !errors.Is(err, ErrSetupWaitUntil) {
		t.Fatalf("asking again after it was cleared: %v", err)
	}
}

// TestAuditSourceAndHistory: an audit row keeps its source, and the last rows of an
// account come back oldest first with whether older ones were left out; the creation row
// is found whatever the cut. The control arm is a row with no source.
func TestAuditSourceAndHistory(t *testing.T) {
	ctx := context.Background()
	store, _ := setupStore(t)
	row := AccountRow{Alias: "sample-new", Email: "n@example.test", Access: "gateway", Status: "testing", TokenRef: "op://example-vault/token-n/credential",
		ProxyHost: "proxy.example.test", ProxyPort: 8001, ExpectedIP: "192.0.2.5", UsernameRef: "op://p/u/username", PasswordRef: "op://p/u/password"}
	if _, err := store.CreateAccount(ctx, row, AuditEntry{SourceIP: "admin-socket", Action: "account-create", Source: slackSource}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		entry := AuditEntry{SourceIP: "192.0.2.50", Action: "note"}
		if err := store.RecordAudit(ctx, "sample-new", entry, map[string]FieldChange{"n": {Old: fmt.Sprint(i), New: fmt.Sprint(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	created, err := store.AccountCreated(ctx, "sample-new")
	if err != nil || created == nil || created.Source != slackSource || created.SourceIP != "admin-socket" {
		t.Fatalf("created = %+v %v", created, err)
	}
	if none, err := store.AccountCreated(ctx, "sample-a"); err != nil || none != nil {
		t.Fatalf("an imported account: %+v %v", none, err)
	}
	last, truncated, err := store.AccountAuditLast(ctx, "sample-new", 3)
	if err != nil || !truncated || len(last) != 3 || last[0].Changes["n"].New != "3" || last[2].Changes["n"].New != "5" || last[2].Source != (Source{}) {
		t.Fatalf("last 3 = %+v, truncated %v, %v", last, truncated, err)
	}
	all, truncated, err := store.AccountAuditLast(ctx, "sample-new", 6)
	if err != nil || truncated || len(all) != 6 || all[0].Action != "account-create" || all[0].Source != slackSource {
		t.Fatalf("last 6 = %+v, truncated %v, %v", all, truncated, err)
	}
	if every, err := store.AccountAudit(ctx, "sample-new"); err != nil || len(every) != 6 {
		t.Fatalf("all = %d %v", len(every), err)
	}
}

// TestMigrateToV12: a version-11 file gets the source and wait columns, its rows are the
// dashboard's with no wait, and it can then be written with them. Control arm: the
// version-11 file has none of the columns before the migration.
func TestMigrateToV12(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	if _, err := store.ImportAccounts(ctx, []AccountRow{{Alias: setupAlias, Email: "a@example.test", Access: "gateway", Status: "active", TokenRef: "op://example-vault/t/credential",
		ProxyHost: "proxy.example.test", ProxyPort: 8001, ExpectedIP: "192.0.2.1", UsernameRef: "op://p/u/username", PasswordRef: "op://p/u/password"}}); err != nil {
		t.Fatal(err)
	}
	request(t, store, setupAlias)
	if err := store.RecordAudit(ctx, setupAlias, AuditEntry{SourceIP: "192.0.2.50", Action: "note"}, map[string]FieldChange{"n": {Old: "0", New: "1"}}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE account_audit DROP COLUMN source_kind", "ALTER TABLE account_audit DROP COLUMN source_thread", "ALTER TABLE account_audit DROP COLUMN source_ts",
		"ALTER TABLE account_setup DROP COLUMN source_kind", "ALTER TABLE account_setup DROP COLUMN source_thread", "ALTER TABLE account_setup DROP COLUMN source_ts",
		"ALTER TABLE account_setup DROP COLUMN wait_until", "ALTER TABLE account_setup DROP COLUMN wait_until_set", "PRAGMA user_version = 11",
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("SELECT source_kind FROM account_setup"); err == nil {
		t.Fatal("control arm: the version-11 file has the column")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	rows, _ := migrated.SetupRows(ctx)
	if got := rows[setupAlias]; got.Source.Kind != SourceDashboard || got.Source.Thread != "" || !got.WaitUntil.IsZero() || got.WaitUntilSet || got.State != SetupRequested {
		t.Fatalf("migrated row = %+v", got)
	}
	audit, err := migrated.AccountAudit(ctx, setupAlias)
	if err != nil || len(audit) != 1 || audit[0].Source != (Source{}) {
		t.Fatalf("migrated audit = %+v %v", audit, err)
	}
	if _, _, err := migrated.RequestSetup(ctx, "nobody", "login_and_token", nil, nextID(), slackSource); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("a write after the migration: %v", err)
	}
}
