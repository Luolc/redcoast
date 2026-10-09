package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPlanUnitsOverTime (E4.7, plans arm) checks the seeded units and that a new unit
// period for a plan changes the quota unit from its start on. Control arm: without the
// new period the unit stays.
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
			// The open-ended seed period must be closed before a new one can start.
			if err := store.SetPlanUnit(ctx, PlanUnit{Plan: "max-5x", QuotaUnit: 5, MonthlyUSD: 100, From: time.UnixMilli(0), To: t0.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetPlanUnit(ctx, PlanUnit{Plan: "max-5x", QuotaUnit: 7, MonthlyUSD: 100, From: t0.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := store.QuotaUnit(ctx, "synthetic-a", t0)
		if err != nil || before != 5 {
			t.Fatalf("control=%v before=%v err=%v", control, before, err)
		}
		after, err := store.QuotaUnit(ctx, "synthetic-a", t0.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]float64{true: 5, false: 7}[control]; after != want {
			t.Fatalf("control=%v after=%v want=%v", control, after, want)
		}
	}
}

// TestPlanScheduleOverTime (E4.7, schedule arm) checks that an account's plan changes
// at the start of its next period, and the refusals: overlapping periods, a plan
// without a unit, an account without a period.
func TestPlanScheduleOverTime(t *testing.T) {
	c := newClock()
	ctx := context.Background()
	t0 := c.Now()
	change := t0.Add(24 * time.Hour)
	for _, control := range []bool{true, false} {
		store, _ := open(t, c)
		first := PlanPeriod{Alias: "synthetic-a", Plan: "max-20x", From: t0.Add(-time.Hour), Source: "manual", Note: "initial"}
		if !control {
			first.To = change
		}
		if err := store.SetPlanSchedule(ctx, first); err != nil {
			t.Fatal(err)
		}
		if !control {
			if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-a", Plan: "max-5x", From: change, Source: "manual", Note: "downgrade"}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := store.QuotaUnit(ctx, "synthetic-a", change.Add(-time.Minute))
		if err != nil || before != 10 {
			t.Fatalf("control=%v before=%v err=%v", control, before, err)
		}
		after, err := store.QuotaUnit(ctx, "synthetic-a", change)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]float64{true: 10, false: 5}[control]; after != want {
			t.Fatalf("control=%v after=%v want=%v", control, after, want)
		}
		if !control {
			periods, err := store.PlanSchedule(ctx, "synthetic-a")
			if err != nil || len(periods) != 2 || periods[0].Plan != "max-20x" || !periods[0].To.Equal(change) || periods[1].Plan != "max-5x" || !periods[1].To.IsZero() || periods[1].Note != "downgrade" {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			// Overlaps are refused: inside the first period, and straddling the change.
			for _, period := range []PlanPeriod{
				{Alias: "synthetic-a", Plan: "pro", From: t0, To: t0.Add(time.Hour), Source: "manual"},
				{Alias: "synthetic-a", Plan: "pro", From: change.Add(-time.Hour), To: change.Add(time.Hour), Source: "manual"},
				{Alias: "synthetic-a", Plan: "pro", From: change.Add(time.Hour), Source: "manual"},
			} {
				if err := store.SetPlanSchedule(ctx, period); !errors.Is(err, ErrPeriodOverlap) {
					t.Fatalf("overlapping period accepted: %+v err=%v", period, err)
				}
			}
			// The same start replaces the period instead.
			if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-a", Plan: "pro", From: change, Source: "manual"}); err != nil {
				t.Fatal(err)
			}
			if unit, err := store.QuotaUnit(ctx, "synthetic-a", change); err != nil || unit != 1 {
				t.Fatalf("replacement not applied: unit=%v err=%v", unit, err)
			}
		}
		// Before the first period, and for an account or plan nobody scheduled, there is no unit.
		if _, err := store.QuotaUnit(ctx, "synthetic-a", t0.Add(-2*time.Hour)); !errors.Is(err, ErrNoPlan) {
			t.Fatalf("unit before the first period: %v", err)
		}
		if _, err := store.QuotaUnit(ctx, "synthetic-b", t0); !errors.Is(err, ErrNoPlan) {
			t.Fatalf("unit without a schedule: %v", err)
		}
		if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-b", Plan: "team", From: t0, Source: "manual"}); !errors.Is(err, ErrNoPlan) {
			t.Fatalf("schedule on a plan without a unit: %v", err)
		}
		if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-b", Plan: "pro", From: t0, To: t0, Source: "manual"}); err == nil || errors.Is(err, ErrNoPlan) {
			t.Fatalf("empty period accepted: %v", err)
		}
		if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "synthetic-b", Plan: "pro", From: t0, Source: "guess"}); err == nil {
			t.Fatal("unknown source accepted")
		}
	}
}
