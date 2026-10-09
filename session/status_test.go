package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestStatusShowsThePlanInEffect covers an open-ended period, a bounded one still in
// effect, one that has ended and an account that never had one; the last two are null.
func TestStatusShowsThePlanInEffect(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	now := c.Now()
	ended := PlanPeriod{Alias: "sample-b", Plan: "max-20x", From: time.UnixMilli(0), To: now.Add(-time.Hour), Source: "manual"}
	bounded := PlanPeriod{Alias: "sample-d", Plan: "max-5x", From: now.Add(-24 * time.Hour), To: now.Add(24 * time.Hour), Source: "manual"}
	for _, period := range []PlanPeriod{ended, bounded} {
		if err := store.SetPlanSchedule(ctx, period); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.Status(ctx, []string{"sample-a", "sample-b", "sample-c", "sample-d"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range status.Accounts {
		plan, err := json.Marshal(a.Plan)
		if err != nil {
			t.Fatal(err)
		}
		got[a.Alias] = string(plan)
	}
	want := map[string]string{
		"sample-a": `{"name":"pro","effective_from":"1970-01-01T00:00:00Z","effective_to":null}`,
		"sample-b": `null`,
		"sample-c": `null`,
		"sample-d": `{"name":"max-5x","effective_from":"2026-10-05T12:00:00Z","effective_to":"2026-10-07T12:00:00Z"}`,
	}
	for alias, plan := range want {
		if got[alias] != plan {
			t.Errorf("%s: plan %s, want %s", alias, got[alias], plan)
		}
	}
}
