package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// TestPlanCommand checks the plan subcommand's parsing and that it writes the periods
// the operator asks for: the quota unit of a plan and an account's plan over time.
func TestPlanCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	ctx := context.Background()
	for _, arguments := range [][]string{
		{"--session-db", db, "schedule", "sample-ab", "max-20x", "2026-09-01T00:00:00Z", "2026-11-15T00:00:00Z", "--note", "until the downgrade"},
		{"--session-db", db, "schedule", "sample-ab", "max-5x", "2026-11-15T00:00:00Z"},
		{"schedule", "sample-ac", "pro", "2026-09-01T00:00:00Z", "--session-db", db},
		{"--session-db", db, "unit", "max-5x", "5", "100", "1970-01-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"--session-db", db, "unit", "max-5x", "6", "120", "2027-01-01T00:00:00Z"},
	} {
		cmd, err := parsePlanCommand(arguments)
		if err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if err := runPlan(ctx, cmd); err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
	}
	store, err := session.Open(ctx, db, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, test := range []struct {
		alias string
		at    time.Time
		unit  float64
	}{
		{"sample-ab", time.Date(2026, 11, 14, 23, 59, 59, 0, time.UTC), 10},
		{"sample-ab", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), 5},
		{"sample-ab", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 6},
		{"sample-ac", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), 1},
	} {
		if unit, err := store.QuotaUnit(ctx, test.alias, test.at); err != nil || unit != test.unit {
			t.Fatalf("%s at %s: unit=%v err=%v", test.alias, test.at, unit, err)
		}
	}
	periods, err := store.PlanSchedule(ctx, "sample-ab")
	if err != nil || len(periods) != 2 || periods[0].Note != "until the downgrade" || periods[0].Source != "manual" {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	for _, bad := range [][]string{
		nil,
		{"schedule", "sample-ab", "pro", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "schedule", "Sample AB", "pro", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "schedule", "sample-ab", "pro", "yesterday"},
		{"--session-db", db, "schedule", "sample-ab", "pro", "2026-09-02T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "unit", "pro", "0", "20", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "unit", "pro", "1", "20"},
		{"--session-db", db, "rename", "pro"},
	} {
		if _, err := parsePlanCommand(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	// Overlaps and unknown plans are refused by the store, not written.
	for _, overlapping := range [][]string{
		{"--session-db", db, "schedule", "sample-ab", "pro", "2026-10-01T00:00:00Z"},
		{"--session-db", db, "schedule", "sample-ac", "team", "2027-01-01T00:00:00Z"},
	} {
		cmd, err := parsePlanCommand(overlapping)
		if err != nil {
			t.Fatal(err)
		}
		if err := runPlan(ctx, cmd); err == nil {
			t.Fatalf("%v written", overlapping)
		}
	}
}
