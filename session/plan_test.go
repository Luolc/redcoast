package session

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestPlanUnitsOverTime (E4.7, plans arm) checks the seeded units and that a new unit
// for a plan changes the quota unit from its start on, ending the period before it
// there. Control arm: without the new unit the unit stays.
func TestPlanUnitsOverTime(t *testing.T) {
	c := newClock()
	ctx := context.Background()
	t0 := c.Now()
	for _, control := range []bool{true, false} {
		store, _ := open(t, c)
		if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-a", Plan: "max-5x", From: t0.Add(-time.Hour), Source: "manual"}); err != nil {
			t.Fatal(err)
		}
		for plan, want := range map[string]float64{"pro": 1, "max-5x": 5, "max-20x": 10} {
			if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "seed-" + plan, Plan: plan, From: t0.Add(-time.Hour), Source: "manual"}); err != nil {
				t.Fatal(err)
			}
			if unit, err := store.QuotaUnit(ctx, "seed-"+plan, t0); err != nil || unit != want {
				t.Fatalf("%s: unit=%v err=%v", plan, unit, err)
			}
		}
		if !control {
			if err := store.SetPlanUnit(ctx, PlanUnit{Plan: "max-5x", QuotaUnit: 7, MonthlyUSD: 100, From: t0.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := store.QuotaUnit(ctx, "synthetic-a", t0.Add(time.Hour-time.Millisecond))
		if err != nil || before != 5 {
			t.Fatalf("control=%v before=%v err=%v", control, before, err)
		}
		after, err := store.QuotaUnit(ctx, "synthetic-a", t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]float64{true: 5, false: 7}[control]; after != want {
			t.Fatalf("control=%v after=%v want=%v", control, after, want)
		}
	}
}

// TestSetPlanSchedule checks that an account's plan is a step function of time: each
// period set from a time on ends the one in effect then, whether that time is the
// present, in the future or in the past, and deletes the periods that start later.
// Earlier periods and other accounts stay as they were, and a period set at the start
// of a stored one replaces it.
func TestSetPlanSchedule(t *testing.T) {
	c := newClock()
	ctx := context.Background()
	t0 := c.Now()
	store, _ := open(t, c)
	set := func(plan string, from time.Time, note string) {
		t.Helper()
		if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-a", Plan: plan, From: from, Source: "manual", Note: note}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(step string, want ...PlanPeriod) {
		t.Helper()
		for i := range want {
			want[i].Alias, want[i].Source = "synthetic-a", "manual"
		}
		if periods, err := store.PlanSchedule(ctx, "synthetic-a"); err != nil || !slices.Equal(periods, want) {
			t.Fatalf("%s: periods=%+v err=%v, want %+v", step, periods, err, want)
		}
	}
	others, err := store.PlanSchedule(ctx, "sample-a")
	if err != nil || len(others) != 1 {
		t.Fatalf("other account: periods=%+v err=%v", others, err)
	}
	set("pro", t0.Add(-48*time.Hour), "past")
	set("max-5x", t0.Add(-24*time.Hour), "current")
	set("pro", t0.Add(24*time.Hour), "later")
	check("each period ends the one before",
		PlanPeriod{Plan: "pro", From: t0.Add(-48 * time.Hour), To: t0.Add(-24 * time.Hour), Note: "past"},
		PlanPeriod{Plan: "max-5x", From: t0.Add(-24 * time.Hour), To: t0.Add(24 * time.Hour), Note: "current"},
		PlanPeriod{Plan: "pro", From: t0.Add(24 * time.Hour), Note: "later"})

	set("max-20x", t0, "upgrade")
	check("a switch now deletes the later period",
		PlanPeriod{Plan: "pro", From: t0.Add(-48 * time.Hour), To: t0.Add(-24 * time.Hour), Note: "past"},
		PlanPeriod{Plan: "max-5x", From: t0.Add(-24 * time.Hour), To: t0, Note: "current"},
		PlanPeriod{Plan: "max-20x", From: t0, Note: "upgrade"})
	for at, unit := range map[time.Time]float64{t0.Add(-time.Millisecond): 5, t0: 10, t0.Add(48 * time.Hour): 10} {
		if got, err := store.QuotaUnit(ctx, "synthetic-a", at); err != nil || got != unit {
			t.Fatalf("unit at %s: %v err=%v, want %v", at, got, err, unit)
		}
	}

	set("pro", t0.Add(12*time.Hour), "downgrade")
	check("a switch in the future",
		PlanPeriod{Plan: "pro", From: t0.Add(-48 * time.Hour), To: t0.Add(-24 * time.Hour), Note: "past"},
		PlanPeriod{Plan: "max-5x", From: t0.Add(-24 * time.Hour), To: t0, Note: "current"},
		PlanPeriod{Plan: "max-20x", From: t0, To: t0.Add(12 * time.Hour), Note: "upgrade"},
		PlanPeriod{Plan: "pro", From: t0.Add(12 * time.Hour), Note: "downgrade"})
	if got, err := store.QuotaUnit(ctx, "synthetic-a", t0); err != nil || got != 10 {
		t.Fatalf("unit now after a future switch: %v err=%v", got, err)
	}

	set("max-5x", t0.Add(-36*time.Hour), "correction")
	check("a switch in the past",
		PlanPeriod{Plan: "pro", From: t0.Add(-48 * time.Hour), To: t0.Add(-36 * time.Hour), Note: "past"},
		PlanPeriod{Plan: "max-5x", From: t0.Add(-36 * time.Hour), Note: "correction"})

	set("pro", t0.Add(-36*time.Hour), "")
	check("a switch at a period's start",
		PlanPeriod{Plan: "pro", From: t0.Add(-48 * time.Hour), To: t0.Add(-36 * time.Hour), Note: "past"},
		PlanPeriod{Plan: "pro", From: t0.Add(-36 * time.Hour)})

	if periods, err := store.PlanSchedule(ctx, "sample-a"); err != nil || !slices.Equal(periods, others) {
		t.Fatalf("other account: periods=%+v err=%v, want %+v", periods, err, others)
	}
	for _, period := range []PlanPeriod{
		{Alias: "synthetic-a", Plan: "pro", From: t0, To: t0.Add(time.Hour), Source: "manual"},
		{Alias: "synthetic-a", Plan: "pro", Source: "manual"},
		{Alias: "synthetic-a", Plan: "pro", From: t0, Source: "guess"},
	} {
		if err := store.SetPlanSchedule(ctx, period); err == nil {
			t.Fatalf("accepted: %+v", period)
		}
	}
}

// TestPlanWritesKeepCoverage checks the refusals that keep an account from losing its
// plan: a schedule onto a plan that has no quota unit at some time from its start on,
// whether at the start or later, and a write that leaves an account without a plan
// with a unit at a time it had one. Each leaves the store as it was.
func TestPlanWritesKeepCoverage(t *testing.T) {
	c := newClock()
	ctx := context.Background()
	t0 := c.Now()
	store, _ := open(t, c)
	// A plan with a gap in its units, as bounded unit periods could leave it.
	for _, unit := range []PlanUnit{
		{Plan: "legacy", QuotaUnit: 3, MonthlyUSD: 60, From: t0.Add(-time.Hour), To: t0.Add(time.Hour)},
		{Plan: "legacy", QuotaUnit: 3, MonthlyUSD: 60, From: t0.Add(2 * time.Hour)},
	} {
		if _, err := store.db.Exec("INSERT INTO plans (plan, effective_from, effective_to, quota_unit, monthly_usd) VALUES (?, ?, ?, ?, ?)",
			unit.Plan, unit.From.UnixMilli(), nullableTime(unit.To), unit.QuotaUnit, unit.MonthlyUSD); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() map[string][]PlanPeriod {
		t.Helper()
		out := map[string][]PlanPeriod{}
		for _, alias := range append([]string{"synthetic-c"}, accounts...) {
			periods, err := store.PlanSchedule(ctx, alias)
			if err != nil {
				t.Fatal(err)
			}
			out[alias] = periods
		}
		return out
	}
	before := snapshot()
	for _, period := range []PlanPeriod{
		{Alias: "synthetic-c", Plan: "team", From: t0, Source: "manual"},
		{Alias: "synthetic-c", Plan: "legacy", From: t0, Source: "manual"},
		{Alias: "synthetic-c", Plan: "legacy", From: t0.Add(-2 * time.Hour), Source: "manual"},
	} {
		if err := store.SetPlanSchedule(ctx, period); !errors.Is(err, ErrNoPlan) {
			t.Fatalf("schedule onto a plan with a gap from its start: %+v err=%v", period, err)
		}
	}
	if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-c", Plan: "legacy", From: t0.Add(2 * time.Hour), Source: "manual"}); err != nil {
		t.Fatalf("control: schedule onto the plan where it has a unit throughout: %v", err)
	}
	if err := store.EndPlan(ctx, "synthetic-c", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !equalSchedules(after, before) {
		t.Fatalf("refused writes changed the store: %+v, want %+v", after, before)
	}
	// No plan write the commands make drops a period on its own, so the check is fed one.
	err := store.transact(ctx, func(tx *sql.Tx) error {
		return keepCoverage(ctx, tx, func() error {
			_, err := tx.ExecContext(ctx, "UPDATE plan_schedule SET effective_to = ? WHERE alias = 'sample-a'", t0.UnixMilli())
			return err
		})
	})
	if !errors.Is(err, ErrPlanGap) {
		t.Fatalf("a write that drops coverage: %v", err)
	}
	if after := snapshot(); !equalSchedules(after, before) {
		t.Fatalf("a refused write changed the store: %+v, want %+v", after, before)
	}
	err = store.transact(ctx, func(tx *sql.Tx) error {
		return keepCoverage(ctx, tx, func() error {
			_, err := tx.ExecContext(ctx, "UPDATE plan_schedule SET plan = 'max-5x' WHERE alias = 'sample-a'")
			return err
		})
	})
	if err != nil {
		t.Fatalf("control: a write that keeps coverage: %v", err)
	}
}

// equalSchedules reports whether two schedules by account are the same.
func equalSchedules(a, b map[string][]PlanPeriod) bool {
	if len(a) != len(b) {
		return false
	}
	for alias, periods := range a {
		if !slices.Equal(periods, b[alias]) {
			return false
		}
	}
	return true
}

// TestEndPlan checks that ending a plan, now or in the future, ends the period in
// effect then and deletes the later ones, and that an account with nothing in effect
// at or after that time is refused.
func TestEndPlan(t *testing.T) {
	c := newClock()
	ctx := context.Background()
	t0 := c.Now()
	store, _ := open(t, c)
	for _, period := range []PlanPeriod{
		{Alias: "synthetic-a", Plan: "max-5x", From: t0.Add(-24 * time.Hour), Source: "manual"},
		{Alias: "synthetic-a", Plan: "pro", From: t0.Add(24 * time.Hour), Source: "manual"},
	} {
		if err := store.SetPlanSchedule(ctx, period); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndPlan(ctx, "synthetic-a", t0); err != nil {
		t.Fatal(err)
	}
	want := []PlanPeriod{{Alias: "synthetic-a", Plan: "max-5x", From: t0.Add(-24 * time.Hour), To: t0, Source: "manual"}}
	if periods, err := store.PlanSchedule(ctx, "synthetic-a"); err != nil || !slices.Equal(periods, want) {
		t.Fatalf("after the end: periods=%+v err=%v", periods, err)
	}
	if _, err := store.QuotaUnit(ctx, "synthetic-a", t0); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("unit after the end: %v", err)
	}
	if err := store.EndPlan(ctx, "sample-a", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QuotaUnit(ctx, "sample-a", t0); err != nil {
		t.Fatalf("unit before a future end: %v", err)
	}
	if _, err := store.QuotaUnit(ctx, "sample-a", t0.Add(time.Hour)); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("unit after a future end: %v", err)
	}
	for _, alias := range []string{"synthetic-a", "synthetic-b"} {
		if err := store.EndPlan(ctx, alias, t0); !errors.Is(err, ErrNothingToEnd) {
			t.Fatalf("%s: nothing to end: %v", alias, err)
		}
	}
	if periods, err := store.PlanSchedule(ctx, "synthetic-a"); err != nil || !slices.Equal(periods, want) {
		t.Fatalf("after a refused end: periods=%+v err=%v", periods, err)
	}
}
