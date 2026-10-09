package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// read stores one window's reading for alias at the store's clock.
func read(t *testing.T, store *Store, alias, window string, utilization float64, status string, resetAt time.Time) {
	t.Helper()
	if err := store.RecordQuota(context.Background(), alias, []Reading{{Window: window, Utilization: utilization, Status: status, ResetAt: resetAt, Source: "response"}}); err != nil {
		t.Fatal(err)
	}
}

// newSession issues a session and binds it once, returning the credential and binding.
func newSession(t *testing.T, store *Store) (string, Binding, error) {
	t.Helper()
	issued, err := store.Issue(context.Background(), "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Bind(context.Background(), issued.Token, accounts)
	return issued.Token, b, err
}

// TestSelectionOrder (E4.5) runs each ordering factor as a pair: the arm and a control
// arm with the factor swapped, which must swap the chosen account.
func TestSelectionOrder(t *testing.T) {
	day := 24 * time.Hour
	for _, test := range []struct {
		name  string
		setup func(store *Store, now time.Time, control bool)
		want  map[bool]string // by control
	}{
		{"earlier_reset_first", func(store *Store, now time.Time, control bool) {
			resetA, resetB := now.Add(day), now.Add(6*day)
			if control {
				resetA, resetB = resetB, resetA
			}
			read(t, store, "sample-a", Window7d, 0.5, "allowed", resetA)
			read(t, store, "sample-b", Window7d, 0.5, "allowed", resetB)
		}, map[bool]string{false: "sample-a", true: "sample-b"}},
		{"larger_plan_first", func(store *Store, now time.Time, control bool) {
			planA, planB := "max-20x", "pro"
			if control {
				planA, planB = planB, planA
			}
			schedule(t, store, "sample-a", planA)
			schedule(t, store, "sample-b", planB)
			read(t, store, "sample-a", Window7d, 0.5, "allowed", now.Add(day))
			read(t, store, "sample-b", Window7d, 0.5, "allowed", now.Add(day))
		}, map[bool]string{false: "sample-a", true: "sample-b"}},
		{"tier_before_score", func(store *Store, now time.Time, control bool) {
			// B scores higher (max-20x, resetting soon) but is over soft; A is under soft
			// with a low score. Control: A is over soft too, so the score decides.
			schedule(t, store, "sample-b", "max-20x")
			utilizationA := 0.5
			if control {
				utilizationA = 0.85
			}
			read(t, store, "sample-a", Window7d, utilizationA, "allowed", now.Add(6*day))
			read(t, store, "sample-b", Window7d, 0.85, "allowed_warning", now.Add(day))
		}, map[bool]string{false: "sample-a", true: "sample-b"}},
		{"no_reading_counts_as_zero_over_a_week", func(store *Store, now time.Time, control bool) {
			// A has no reading: 1 × 10 ÷ 7 days. B at 50% resetting in a day: 0.5 × unit ÷ 1 day.
			schedule(t, store, "sample-a", "max-20x")
			if control {
				schedule(t, store, "sample-b", "max-20x")
			}
			read(t, store, "sample-b", Window7d, 0.5, "allowed", now.Add(day))
		}, map[bool]string{false: "sample-a", true: "sample-b"}},
		{"soft_threshold_read_per_request", func(store *Store, now time.Time, control bool) {
			// A scores far higher (max-20x, 60%, resetting in a day) than B (pro, 40%,
			// six days). Under the default soft of 80% both are open and A wins. The
			// control arm changes one thing: soft lowered to 50% in the settings, which
			// puts A in the soft tier, so B wins.
			schedule(t, store, "sample-a", "max-20x")
			read(t, store, "sample-a", Window7d, 0.6, "allowed", now.Add(day))
			read(t, store, "sample-b", Window7d, 0.4, "allowed", now.Add(6*day))
			if control {
				if _, err := store.db.Exec("UPDATE settings SET value = '0.5' WHERE key = 'soft'"); err != nil {
					t.Fatal(err)
				}
			}
		}, map[bool]string{false: "sample-a", true: "sample-b"}},
	} {
		for _, control := range []bool{false, true} {
			t.Run(test.name, func(t *testing.T) {
				c := newClock()
				store, _ := open(t, c)
				test.setup(store, c.Now(), control)
				_, b, err := newSession(t, store)
				if err != nil || b.Alias != test.want[control] {
					t.Fatalf("control=%v: got %q %v, want %q", control, b.Alias, err, test.want[control])
				}
			})
		}
	}
}

// TestExpiredReadingIsNoReading (E4.5, reset passed) checks that a reading whose reset
// time has passed no longer counts: an account read at 95% is bindable once its window
// has reset. Control arm: the same reading with the reset still ahead exhausts it.
func TestExpiredReadingIsNoReading(t *testing.T) {
	for _, control := range []bool{false, true} {
		c := newClock()
		store, _ := open(t, c)
		reset := c.Now().Add(-time.Minute)
		if control {
			reset = c.Now().Add(time.Hour)
		}
		read(t, store, "sample-a", Window7d, 0.95, "allowed_warning", reset)
		issued, err := store.Issue(context.Background(), "machine-a", LocalSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := store.Bind(context.Background(), issued.Token, []string{"sample-a"})
		var exhausted *QuotaExhausted
		switch {
		case !control && (err != nil || b.Alias != "sample-a"):
			t.Fatalf("expired reading still counted: %q %v", b.Alias, err)
		case control && !errors.As(err, &exhausted):
			t.Fatalf("control arm: live reading ignored: %q %v", b.Alias, err)
		case control && !exhausted.ResetAt.Equal(reset):
			t.Fatalf("control arm: reset %v, want %v", exhausted.ResetAt, reset)
		}
	}
}

// TestSoftThresholdKeepsLeaseOnly (E4.2) checks the soft rule: once A crosses soft, a
// session bound to A stays within its lease, a new session goes to B, and A's session
// moves to B when its lease expires. Control arm: A under soft is renewed at expiry.
func TestSoftThresholdKeepsLeaseOnly(t *testing.T) {
	for _, control := range []bool{false, true} {
		c := newClock()
		store, _ := open(t, c)
		ctx := context.Background()
		// A is preferred at first: B carries some use with a distant reset, A none (1 ÷ 7 days beats 0.7 ÷ 6 days).
		read(t, store, "sample-b", Window7d, 0.3, "allowed", c.Now().Add(6*24*time.Hour))
		token, first, err := newSession(t, store)
		if err != nil || first.Alias != "sample-a" {
			t.Fatalf("first binding %q %v", first.Alias, err)
		}
		utilization := 0.85
		if control {
			utilization = 0.5
		}
		read(t, store, "sample-a", Window7d, utilization, "allowed", c.Now().Add(3*24*time.Hour))
		c.Advance(5 * time.Second)
		within, err := store.Bind(ctx, token, accounts)
		if err != nil || within.Alias != "sample-a" {
			t.Fatalf("control=%v: within the lease got %q %v, want sample-a", control, within.Alias, err)
		}
		_, fresh, err := newSession(t, store)
		if want := map[bool]string{false: "sample-b", true: "sample-a"}[control]; err != nil || fresh.Alias != want {
			t.Fatalf("control=%v: new session got %q %v, want %s", control, fresh.Alias, err, want)
		}
		c.Advance(6 * time.Second) // past the 10 s lease
		expired, err := store.Bind(ctx, token, accounts)
		if err != nil {
			t.Fatal(err)
		}
		if control && (expired.Alias != "sample-a" || expired.Reason != "new") {
			t.Fatalf("control arm: lease not renewed on A: %+v", expired)
		}
		if !control && (expired.Alias != "sample-b" || expired.Reason != "lease_expired") {
			t.Fatalf("session stayed on A after the lease: %+v", expired)
		}
	}
}

// TestHardThresholdRebindsAtOnce (E4.3) checks that a bound account crossing hard in
// either window, or reported rejected, loses the session on its very next request,
// inside the lease. Control arm: 85% is over soft only and keeps the session.
func TestHardThresholdRebindsAtOnce(t *testing.T) {
	for _, test := range []struct {
		name        string
		window      string
		utilization float64
		status      string
		want        string
	}{
		{"7d_over_hard", Window7d, 0.95, "allowed_warning", "sample-b"},
		{"5h_over_hard", Window5h, 0.9, "allowed_warning", "sample-b"},
		{"rejected_status", Window5h, 0.5, "rejected", "sample-b"},
		{"over_soft_only", Window7d, 0.85, "allowed_warning", "sample-a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newClock()
			store, _ := open(t, c)
			read(t, store, "sample-b", Window7d, 0.3, "allowed", c.Now().Add(6*24*time.Hour))
			token, first, err := newSession(t, store)
			if err != nil || first.Alias != "sample-a" {
				t.Fatalf("first binding %q %v", first.Alias, err)
			}
			read(t, store, "sample-a", test.window, test.utilization, test.status, c.Now().Add(time.Hour))
			c.Advance(time.Second)
			next, err := store.Bind(context.Background(), token, accounts)
			if err != nil || next.Alias != test.want {
				t.Fatalf("got %q %v, want %s", next.Alias, err, test.want)
			}
			if test.want == "sample-b" && next.Reason != "quota_hard" {
				t.Fatalf("reason %q, want quota_hard", next.Reason)
			}
			if got := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND reason = 'quota_hard'", next.SessionID); got != map[string]int{"sample-b": 1, "sample-a": 0}[test.want] {
				t.Fatalf("quota_hard history rows: %d", got)
			}
		})
	}
}

// TestAllOverHard (E4.4, store side) checks that with every account at or over hard,
// Bind returns QuotaExhausted with the earliest reset, both for a new session and for a
// bound one, and the request still counts as activity. Control arm: one account over
// soft only is still chosen.
func TestAllOverHard(t *testing.T) {
	for _, control := range []bool{false, true} {
		c := newClock()
		store, _ := open(t, c)
		ctx := context.Background()
		token, first, err := newSession(t, store)
		if err != nil {
			t.Fatal(err)
		}
		earliest := c.Now().Add(2 * time.Hour)
		read(t, store, "sample-a", Window5h, 0.92, "allowed_warning", earliest)
		read(t, store, "sample-a", Window7d, 0.4, "allowed", c.Now().Add(5*24*time.Hour))
		utilizationB := 0.9
		if control {
			utilizationB = 0.85
		}
		read(t, store, "sample-b", Window7d, utilizationB, "allowed_warning", c.Now().Add(3*24*time.Hour))
		c.Advance(time.Second)
		var exhausted *QuotaExhausted
		for _, arm := range []string{"bound", "new"} {
			var b Binding
			if arm == "bound" {
				b, err = store.Bind(ctx, token, accounts)
			} else {
				_, b, err = newSession(t, store)
			}
			switch {
			case control && (err != nil || b.Alias != "sample-b"):
				t.Fatalf("control arm %s: got %q %v, want sample-b", arm, b.Alias, err)
			case !control && !errors.As(err, &exhausted):
				t.Fatalf("%s: got %q %v, want QuotaExhausted", arm, b.Alias, err)
			case !control && !exhausted.ResetAt.Equal(earliest):
				t.Fatalf("%s: reset %v, want %v", arm, exhausted.ResetAt, earliest)
			}
		}
		if seen := count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ? AND last_seen_at = ?", first.SessionID, c.Now().UnixMilli()); seen != 1 {
			t.Fatalf("control=%v: the refused request did not count as activity", control)
		}
	}
}

// TestNoPlanNoCandidate checks that an account without a plan at this time is not a
// candidate: alone it yields ErrNoAccount, beside a planned account it is never
// chosen. Control arm: scheduling the plan makes it the chosen account.
func TestNoPlanNoCandidate(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	if _, err := store.db.Exec("DELETE FROM plan_schedule WHERE alias = 'sample-a'"); err != nil {
		t.Fatal(err)
	}
	issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, issued.Token, []string{"sample-a"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("unplanned account alone: %v", err)
	}
	b, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil || b.Alias != "sample-b" {
		t.Fatalf("beside a planned account: %q %v", b.Alias, err)
	}
	schedule(t, store, "sample-a", "max-20x")
	_, fresh, err := newSession(t, store)
	if err != nil || fresh.Alias != "sample-a" {
		t.Fatalf("control arm: planned max-20x not chosen: %q %v", fresh.Alias, err)
	}
}

// TestExhaustionByPauseReason checks what Bind returns when nothing is left to choose:
// candidates all paused by an upstream 429 are a quota exhaustion with the earliest
// pause end, like readings at or over hard, and the two kinds combine; a candidate out
// for another reason (a 401 pause, no plan) makes it ErrNoAccount instead.
func TestExhaustionByPauseReason(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(store *Store, now time.Time)
		reset time.Duration // the expected QuotaExhausted.ResetAt offset; 0 means ErrNoAccount
	}{
		{"all_paused_by_429", func(store *Store, now time.Time) {
			pause(t, store, "sample-a", now.Add(20*time.Minute), QuotaPauseReason)
			pause(t, store, "sample-b", now.Add(5*time.Minute), QuotaPauseReason)
		}, 5 * time.Minute},
		{"all_paused_by_401", func(store *Store, now time.Time) {
			pause(t, store, "sample-a", now.Add(20*time.Minute), "upstream_401")
			pause(t, store, "sample-b", now.Add(5*time.Minute), "upstream_401")
		}, 0},
		{"429_pause_and_hard_reading", func(store *Store, now time.Time) {
			pause(t, store, "sample-a", now.Add(20*time.Minute), QuotaPauseReason)
			read(t, store, "sample-b", Window5h, 0.95, "allowed_warning", now.Add(10*time.Minute))
		}, 10 * time.Minute},
		{"429_pause_and_401_pause", func(store *Store, now time.Time) {
			pause(t, store, "sample-a", now.Add(20*time.Minute), QuotaPauseReason)
			pause(t, store, "sample-b", now.Add(5*time.Minute), "upstream_401")
		}, 0},
		{"429_pause_without_plan", func(store *Store, now time.Time) {
			pause(t, store, "sample-a", now.Add(20*time.Minute), QuotaPauseReason)
			pause(t, store, "sample-b", now.Add(5*time.Minute), QuotaPauseReason)
			if _, err := store.db.Exec("DELETE FROM plan_schedule WHERE alias = 'sample-b'"); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"hard_reading_and_no_plan", func(store *Store, now time.Time) {
			read(t, store, "sample-a", Window5h, 0.95, "allowed_warning", now.Add(10*time.Minute))
			if _, err := store.db.Exec("DELETE FROM plan_schedule WHERE alias = 'sample-b'"); err != nil {
				t.Fatal(err)
			}
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newClock()
			store, _ := open(t, c)
			test.setup(store, c.Now())
			_, b, err := newSession(t, store)
			var exhausted *QuotaExhausted
			switch {
			case test.reset == 0 && !errors.Is(err, ErrNoAccount):
				t.Fatalf("got %q %v, want ErrNoAccount", b.Alias, err)
			case test.reset != 0 && !errors.As(err, &exhausted):
				t.Fatalf("got %q %v, want QuotaExhausted", b.Alias, err)
			case test.reset != 0 && !exhausted.ResetAt.Equal(c.Now().Add(test.reset)):
				t.Fatalf("reset %v, want %v", exhausted.ResetAt, c.Now().Add(test.reset))
			}
		})
	}
}

// pause pauses alias until the given time with the given reason.
func pause(t *testing.T, store *Store, alias string, until time.Time, reason string) {
	t.Helper()
	if err := store.Pause(context.Background(), alias, until, reason); err != nil {
		t.Fatal(err)
	}
}

// TestPauseNeverShortens checks that a later, shorter pause does not replace one still
// in effect: after a 401's indefinite pause, a 429's pause leaves the account paused
// indefinitely with the 401 reason, and the account is still not bindable once the
// 429's pause would have ended. Control arm: the same 429 pause on an account paused
// only until an earlier time extends it, and Resume ends either.
func TestPauseNeverShortens(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	pause(t, store, "sample-a", far, "upstream_401")
	pause(t, store, "sample-a", c.Now().Add(5*time.Minute), QuotaPauseReason)
	pause(t, store, "sample-b", c.Now().Add(time.Minute), QuotaPauseReason)
	pause(t, store, "sample-b", c.Now().Add(5*time.Minute), QuotaPauseReason)
	state := func(alias string) (int64, string) {
		var until int64
		var reason string
		if err := store.db.QueryRow("SELECT paused_until, pause_reason FROM account_state WHERE alias = ?", alias).Scan(&until, &reason); err != nil {
			t.Fatal(err)
		}
		return until, reason
	}
	if until, reason := state("sample-a"); until != far.UnixMilli() || reason != "upstream_401" {
		t.Fatalf("401 pause replaced: until=%d reason=%s", until, reason)
	}
	if until, reason := state("sample-b"); until != c.Now().Add(5*time.Minute).UnixMilli() || reason != QuotaPauseReason {
		t.Fatalf("control arm: pause not extended: until=%d reason=%s", until, reason)
	}
	c.Advance(6 * time.Minute)
	_, b, err := newSession(t, store)
	if err != nil || b.Alias != "sample-b" {
		t.Fatalf("after the 429 pause: got %q %v, want sample-b (A stays refused)", b.Alias, err)
	}
	if err := store.Resume(ctx, "sample-a"); err != nil {
		t.Fatal(err)
	}
	if _, b, err := newSession(t, store); err != nil || b.Alias != "sample-a" {
		t.Fatalf("after Resume: got %q %v, want sample-a", b.Alias, err)
	}
}
