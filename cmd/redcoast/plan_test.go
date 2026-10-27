package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// TestPlanCommand checks the plan subcommand's parsing and that it writes what the
// operator asks for: the quota unit of a plan and an account's plan over time, each
// from a time on until the next one.
func TestPlanCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	ctx := context.Background()
	for _, arguments := range [][]string{
		{"--session-db", db, "schedule", "sample-ab", "max-20x", "2026-09-01T00:00:00Z", "--note", "until the downgrade"},
		{"--session-db", db, "schedule", "sample-ab", "max-5x", "2026-11-15T00:00:00Z"},
		{"schedule", "sample-ac", "pro", "2026-09-01T00:00:00Z", "--session-db", db},
		{"--session-db", db, "unit", "max-5x", "6", "120", "2027-01-01T00:00:00Z"},
	} {
		cmd, err := parsePlanCommand(arguments)
		if err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if err := runPlan(ctx, cmd, io.Discard); err != nil {
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
	if err != nil || len(periods) != 2 || periods[0].Note != "until the downgrade" || periods[0].Source != "manual" || !periods[0].To.Equal(periods[1].From) {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	for _, bad := range [][]string{
		nil,
		{"schedule", "sample-ab", "pro", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "schedule", "Sample AB", "pro", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "schedule", "sample-ab", "pro", "yesterday"},
		{"--session-db", db, "schedule", "sample-ab"},
		{"--session-db", db, "unit", "pro", "0", "20", "2026-09-01T00:00:00Z"},
		{"--session-db", db, "unit", "pro", "1"},
		{"--session-db", db, "unit", "pro", "1", "20", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"--session-db", db, "end"},
		{"--session-db", db, "end", "sample-ab", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"--session-db", db, "end", "Sample AB"},
		{"--session-db", db, "rename", "pro"},
	} {
		if _, err := parsePlanCommand(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	// A schedule with an end is a usage error that points to end.
	_, err = parsePlanCommand([]string{"--session-db", db, "schedule", "sample-ab", "pro", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"})
	if !errors.Is(err, errPlanUsage) || !strings.Contains(err.Error(), "use end <alias> [<at>]") {
		t.Fatalf("schedule with <to>: %v", err)
	}
	// A plan without a quota unit is refused by the store, and nothing is written.
	for _, refused := range [][]string{
		{"--session-db", db, "schedule", "sample-ac", "team", "2027-01-01T00:00:00Z"},
		{"--session-db", db, "schedule", "sample-ac", "team"},
	} {
		cmd, err := parsePlanCommand(refused)
		if err != nil {
			t.Fatal(err)
		}
		if err := runPlan(ctx, cmd, io.Discard); !errors.Is(err, session.ErrNoPlan) {
			t.Fatalf("%v: %v", refused, err)
		}
	}
	if periods, err := store.PlanSchedule(ctx, "sample-ac"); err != nil || len(periods) != 1 || periods[0].Plan != "pro" || !periods[0].To.IsZero() {
		t.Fatalf("after refused writes: periods=%+v err=%v", periods, err)
	}
}

// TestPlanRefreshesRaisedAccounts checks that a plan change asks the gateway for one
// refresh of each account whose quota unit is higher now: a new or bigger plan, and a
// bigger unit of the plan an account is on, whether the change starts now or in the
// past. A unit that stays or goes down, a plan for another account and a period that
// starts later send nothing. An account the gateway
// refuses with 409 (paused for another reason) gets a line, not an error. Without the
// socket the command prints how to refresh instead.
func TestPlanRefreshesRaisedAccounts(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	socket := filepath.Join(t.TempDir(), "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/accounts/sample-d/refresh" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"account paused: egress_ip_mismatch"}`))
			return
		}
		_, _ = w.Write([]byte(`{"alias":"sample-a"}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	for _, step := range []struct {
		arguments []string
		refreshed []string
	}{
		{[]string{"schedule", "sample-a", "pro", "2020-01-01T00:00:00Z"}, []string{"sample-a"}},
		{[]string{"schedule", "sample-b", "max-5x", "2020-01-01T00:00:00Z"}, []string{"sample-b"}},
		{[]string{"schedule", "sample-a", "max-20x", "2020-01-01T00:00:00Z"}, []string{"sample-a"}},
		{[]string{"schedule", "sample-a", "max-20x", "2020-01-01T00:00:00Z"}, nil},
		{[]string{"schedule", "sample-a", "pro", "2020-01-01T00:00:00Z"}, nil},
		{[]string{"unit", "max-5x", "6", "120", "1970-01-01T00:00:00Z"}, []string{"sample-b"}},
		{[]string{"unit", "max-5x", "6", "120", "1970-01-01T00:00:00Z"}, nil},
		{[]string{"unit", "max-5x", "4", "80", "1970-01-01T00:00:00Z"}, nil},
		{[]string{"schedule", "sample-c", "max-20x", "2099-01-01T00:00:00Z"}, nil},
		{[]string{"schedule", "sample-a", "max-20x"}, []string{"sample-a"}},
		{[]string{"schedule", "sample-a", "pro", "--note", "downgrade"}, nil},
		{[]string{"schedule", "sample-c", "pro", "2025-01-01T00:00:00Z"}, []string{"sample-c"}},
		{[]string{"schedule", "sample-c", "max-20x", "2099-01-01T00:00:00Z"}, nil},
	} {
		mu.Lock()
		received = nil
		mu.Unlock()
		cmd, err := parsePlanCommand(append([]string{"--session-db", db, "--admin-socket", socket}, step.arguments...))
		if err != nil {
			t.Fatal(err)
		}
		if err := runPlan(t.Context(), cmd, io.Discard); err != nil {
			t.Fatalf("%v: %v", step.arguments, err)
		}
		var want []string
		for _, alias := range step.refreshed {
			want = append(want, "POST /accounts/"+alias+"/refresh")
		}
		mu.Lock()
		got := received
		mu.Unlock()
		if !slices.Equal(got, want) {
			t.Fatalf("%v: socket received %q, want %q", step.arguments, got, want)
		}
	}
	cmd, err := parsePlanCommand([]string{"--session-db", db, "--admin-socket", socket, "schedule", "sample-d", "pro", "2020-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPlan(t.Context(), cmd, &out); err != nil || out.String() != "sample-d not refreshed: account paused: egress_ip_mismatch\n" {
		t.Fatalf("refused refresh: %v output %q", err, out.String())
	}
	mu.Lock()
	received = nil
	mu.Unlock()
	cmd, err = parsePlanCommand([]string{"--session-db", db, "unit", "max-5x", "7", "140", "1970-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runPlan(t.Context(), cmd, &out); err != nil || !strings.HasPrefix(out.String(), "quota unit raised for sample-b; ") || !strings.Contains(out.String(), "redcoast claude refresh <alias>") {
		t.Fatalf("without the socket: %v output %q", err, out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Fatalf("socket received %q without --admin-socket", received)
	}
}

// TestPlanSwitchNow checks schedule without times, for both providers: the account is
// on the new plan from the moment of the command on, the period that was in effect
// ends then, a later period is deleted and a past one stays.
func TestPlanSwitchNow(t *testing.T) {
	ctx := t.Context()
	for _, provider := range []string{"claude", "codex"} {
		db := filepath.Join(t.TempDir(), "gateway.sqlite")
		for _, arguments := range [][]string{
			{"schedule", "sample-a", "pro", "2020-01-01T00:00:00Z"},
			{"schedule", "sample-a", "max-5x", "2021-01-01T00:00:00Z"},
			{"schedule", "sample-a", "pro", "2099-01-01T00:00:00Z"},
		} {
			cmd, err := parsePlanCommand(append([]string{"--session-db", db}, arguments...))
			if err != nil {
				t.Fatal(err)
			}
			if err := runPlan(ctx, cmd, io.Discard); err != nil {
				t.Fatalf("%v: %v", arguments, err)
			}
		}
		start := time.Now()
		_, command, err := providerCommand([]string{provider, "plan", "--session-db", db, "schedule", "sample-a", "max-20x", "--note", "upgrade"})
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := parsePlanCommand(command[1:])
		if err != nil {
			t.Fatal(err)
		}
		if err := runPlan(ctx, cmd, io.Discard); err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		end := time.Now()
		store, err := session.Open(ctx, db, session.Config{})
		if err != nil {
			t.Fatal(err)
		}
		periods, err := store.PlanSchedule(ctx, "sample-a")
		_ = store.Close()
		if err != nil || len(periods) != 3 {
			t.Fatalf("%s: periods=%+v err=%v", provider, periods, err)
		}
		past, ended, current := periods[0], periods[1], periods[2]
		switched := ended.To
		if past.Plan != "pro" || !past.To.Equal(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)) ||
			ended.Plan != "max-5x" || switched.Before(start.Truncate(time.Millisecond)) || switched.After(end) ||
			current.Plan != "max-20x" || !current.From.Equal(switched) || !current.To.IsZero() || current.Note != "upgrade" || current.Source != "manual" {
			t.Fatalf("%s: periods=%+v, switched at %s, command ran from %s to %s", provider, periods, switched, start, end)
		}
	}
}

// TestPlanEnd checks end: the account has no plan from that moment on, and ending it
// again, with nothing in effect or later, is refused.
func TestPlanEnd(t *testing.T) {
	ctx := t.Context()
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	for _, arguments := range [][]string{
		{"schedule", "sample-a", "pro", "2020-01-01T00:00:00Z"},
		{"schedule", "sample-a", "max-5x", "2099-01-01T00:00:00Z"},
		{"end", "sample-a"},
	} {
		cmd, err := parsePlanCommand(append([]string{"--session-db", db}, arguments...))
		if err != nil {
			t.Fatal(err)
		}
		if err := runPlan(ctx, cmd, io.Discard); err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
	}
	store, err := session.Open(ctx, db, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if periods, err := store.PlanSchedule(ctx, "sample-a"); err != nil || len(periods) != 1 || periods[0].Plan != "pro" || periods[0].To.IsZero() || periods[0].To.After(time.Now()) {
		t.Fatalf("after end: periods=%+v err=%v", periods, err)
	}
	if _, err := store.QuotaUnit(ctx, "sample-a", time.Now()); !errors.Is(err, session.ErrNoPlan) {
		t.Fatalf("unit after end: %v", err)
	}
	cmd, err := parsePlanCommand([]string{"--session-db", db, "end", "sample-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runPlan(ctx, cmd, io.Discard); err == nil || err.Error() != "account has no plan to end" {
		t.Fatalf("end with nothing to end: %v", err)
	}
}

// TestPlanRefreshNotImplemented checks a raised unit on a gateway that cannot refresh,
// as the Codex gateway answers 501: the write stands, the command gets a line and no
// error.
func TestPlanRefreshNotImplemented(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	socket := filepath.Join(t.TempDir(), "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"this gateway cannot refresh"}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	_, command, err := providerCommand([]string{"codex", "plan", "--session-db", db, "--admin-socket", socket, "schedule", "sample-a", "max-20x"})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := parsePlanCommand(command[1:])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPlan(t.Context(), cmd, &out); err != nil || out.String() != "sample-a not refreshed: this gateway cannot refresh\n" {
		t.Fatalf("refresh not implemented: %v output %q", err, out.String())
	}
}
