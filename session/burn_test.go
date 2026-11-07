package session

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

var burnNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// history returns readings at the given minutes before burnNow, oldest first.
func history(minutesAgo []int, utilization []float64) []QuotaPoint {
	var out []QuotaPoint
	for i, m := range minutesAgo {
		out = append(out, QuotaPoint{At: burnNow.Add(-time.Duration(m) * time.Minute), Utilization: utilization[i]})
	}
	return out
}

func TestEstimateBurn(t *testing.T) {
	reset := burnNow.Add(5 * time.Hour)
	given := &Quota{Utilization: 0.4, ResetAt: reset, ObservedAt: burnNow}
	tests := []struct {
		name       string
		history    []QuotaPoint
		quota      *Quota
		newest     time.Time
		rate       float64 // 0 when none
		exhausts   time.Duration
		before     *bool
		points     int
		span       float64
		reason     string
		usedPoints int
	}{
		{name: "steady rise", history: history([]int{90, 60, 30, 0}, []float64{0.1, 0.2, 0.3, 0.4}), quota: given, newest: burnNow,
			rate: 0.2, exhausts: 3 * time.Hour, before: ptr(true), points: 4, span: 90, usedPoints: 4},
		{name: "least squares over noisy readings", history: history([]int{120, 90, 60, 30, 0}, []float64{0.0, 0.1, 0.1, 0.4, 0.4}), quota: given, newest: burnNow,
			rate: 0.22, exhausts: 2*time.Hour + 43*time.Minute + 38*time.Second, before: ptr(true), points: 5, span: 120, usedPoints: 5},
		{name: "runs out after the reset", history: history([]int{90, 60, 30, 0}, []float64{0.1, 0.11, 0.12, 0.13}), quota: given, newest: burnNow,
			rate: 0.02, exhausts: 43*time.Hour + 30*time.Minute, before: ptr(false), points: 4, span: 90, usedPoints: 4},
		{name: "a drop is a reset: only the readings after it", history: history([]int{200, 170, 140, 90, 60, 30, 0}, []float64{0.7, 0.8, 0.9, 0.0, 0.1, 0.2, 0.3}), quota: given, newest: burnNow,
			rate: 0.2, exhausts: 3*time.Hour + 30*time.Minute, before: ptr(true), points: 4, span: 90, usedPoints: 4},
		{name: "fewer than 3 points since the reset", history: history([]int{200, 170, 140, 60, 0}, []float64{0.7, 0.8, 0.9, 0.0, 0.1}), quota: given, newest: burnNow,
			points: 2, span: 60, reason: "too_few_points"},
		{name: "3 points spanning under 30 minutes", history: history([]int{20, 10, 0}, []float64{0.1, 0.2, 0.3}), quota: given, newest: burnNow,
			points: 3, span: 20, reason: "span_too_short"},
		{name: "no reading", quota: given, newest: burnNow, reason: "no_reading"},
		{name: "flat readings", history: history([]int{90, 60, 30, 0}, []float64{0.4, 0.4, 0.4, 0.4}), quota: given, newest: burnNow,
			rate: 1e-300, points: 4, span: 90, reason: "flat", usedPoints: 4},
		{name: "window the upstream no longer gives", history: history([]int{90, 60, 30, 0}, []float64{0.1, 0.2, 0.3, 0.4}),
			quota:  &Quota{Utilization: 0.4, ResetAt: burnNow.Add(-time.Hour), ObservedAt: burnNow.Add(-2 * time.Hour)},
			newest: burnNow.Add(-30 * time.Minute), points: 4, span: 90, reason: "window_not_given"},
		{name: "reset passed without a newer reading", history: history([]int{90, 60, 30}, []float64{0.1, 0.2, 0.3}),
			quota: &Quota{Utilization: 0.3, ResetAt: burnNow.Add(-time.Minute), ObservedAt: burnNow.Add(-30 * time.Minute)}, newest: burnNow.Add(-30 * time.Minute),
			points: 3, span: 60, reason: "reset_passed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, used := estimateBurn(tc.history, tc.quota, tc.newest, burnNow)
			if got.Points != tc.points || math.Abs(got.SpanMinutes-tc.span) > 1e-9 || got.Reason != tc.reason || len(used) != tc.usedPoints {
				t.Fatalf("estimate = %+v with %d readings used", got, len(used))
			}
			if tc.reason != "" && tc.reason != "flat" {
				if got.RatePerHour != nil || got.ExhaustsAt != nil || got.BeforeReset != nil {
					t.Fatalf("estimate with a reason has numbers: %+v", got)
				}
				return
			}
			if tc.reason == "flat" {
				if got.RatePerHour == nil || *got.RatePerHour != 0 || got.ExhaustsAt != nil || got.BeforeReset != nil {
					t.Fatalf("flat estimate = %+v", got)
				}
				return
			}
			if got.RatePerHour == nil || math.Abs(*got.RatePerHour-tc.rate) > 1e-9 {
				t.Fatalf("rate = %v, want %v", *got.RatePerHour, tc.rate)
			}
			last := tc.history[len(tc.history)-1].At
			if got.ExhaustsAt == nil || got.ExhaustsAt.Sub(last.Add(tc.exhausts)).Abs() > time.Second {
				t.Fatalf("exhausts_at = %v, want %v", got.ExhaustsAt, last.Add(tc.exhausts))
			}
			if got.BeforeReset == nil || *got.BeforeReset != *tc.before || got.ResetsAt == nil || !got.ResetsAt.Equal(reset) {
				t.Fatalf("before_reset = %v, resets_at = %v", got.BeforeReset, got.ResetsAt)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestShares(t *testing.T) {
	got := shares(Window5h, 0.2, map[string]int64{"machine-b": 100, "machine-a": 300, "machine-c": 0})
	want := []MachineBurn{
		{Window: "5h", Machine: "machine-a", Share: 0.75, RatePerHour: 0.15, Estimate: true},
		{Window: "5h", Machine: "machine-b", Share: 0.25, RatePerHour: 0.05, Estimate: true},
	}
	if len(got) != 2 || !slices.EqualFunc(got, want, func(a, b MachineBurn) bool {
		return a.Window == b.Window && a.Machine == b.Machine && a.Estimate == b.Estimate && math.Abs(a.Share-b.Share) < 1e-9 && math.Abs(a.RatePerHour-b.RatePerHour) < 1e-9
	}) {
		t.Fatalf("shares = %+v, want %+v", got, want)
	}
}

// TestDashboardBurn checks the snapshot's burn and totals of an account whose traffic
// two machines carried.
func TestDashboardBurn(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	now := c.Now()
	sessionOf := map[string]string{}
	for _, name := range []string{"machine-a", "machine-b"} {
		credential := strings.Repeat(name[len(name)-1:], 43)
		if err := store.AddMachine(ctx, name, hashOf(credential), 12); err != nil {
			t.Fatal(err)
		}
		issued, err := store.IssueForMachine(ctx, credential, "198.51.100.2", nil)
		if err != nil {
			t.Fatal(err)
		}
		sessionOf[name] = issued.ID
	}
	row := func(machine string, ago time.Duration, kind string, usage *Usage) {
		t.Helper()
		if err := store.RecordTraffic(ctx, Traffic{At: now.Add(-ago), SessionID: sessionOf[machine], Alias: "sample-a", Kind: kind, Host: "api.example.com", Port: 443, Result: "ok", Status: 200, Usage: usage}); err != nil {
			t.Fatal(err)
		}
	}
	row("machine-a", 60*time.Minute, "inference", &Usage{InputTokens: 100, OutputTokens: 100, CacheRead: 50, CacheCreation: 50})
	row("machine-b", 45*time.Minute, "inference", &Usage{InputTokens: 50, OutputTokens: 25, CacheRead: 20, CacheCreation: 5})
	row("machine-a", 30*time.Minute, "inference", nil)
	row("machine-a", 30*time.Minute, "connect", nil)
	row("machine-a", 3*time.Hour, "inference", &Usage{InputTokens: 1000}) // before the last reset
	row("machine-b", 3*24*time.Hour, "inference", &Usage{OutputTokens: 7})
	row("machine-b", 8*24*time.Hour, "inference", &Usage{OutputTokens: 9999})
	for i, u := range []float64{0.1, 0.2, 0.3} {
		at := now.Add(-time.Duration(90-30*i) * time.Minute)
		if _, err := store.db.Exec("INSERT INTO quota_history (alias, window, at, utilization) VALUES ('sample-a', '5h', ?, ?)", at.UnixMilli(), u); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordQuota(ctx, "sample-a", []Reading{{Window: Window5h, Utilization: 0.4, ResetAt: now.Add(5 * time.Hour), Source: "response"}}); err != nil {
		t.Fatal(err)
	}
	snap, err := openReader(t, path, c).Snapshot(ctx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	a := snap.Accounts[1]
	if a.Alias != "sample-a" {
		t.Fatalf("accounts = %+v", snap.Accounts)
	}
	if a.Burn.FiveHour.RatePerHour == nil || math.Abs(*a.Burn.FiveHour.RatePerHour-0.2) > 1e-9 || a.Burn.FiveHour.Points != 4 {
		t.Fatalf("5h burn = %+v", a.Burn.FiveHour)
	}
	if a.Burn.SevenDay.Reason != "no_reading" || a.Burn.SevenDay.RatePerHour != nil {
		t.Fatalf("7d burn = %+v", a.Burn.SevenDay)
	}
	// The row of 3 hours ago is before the first reading since the reset, and the
	// request without usage adds no tokens: machine-a 300 tokens, machine-b 100.
	if len(a.Burn.ByMachine) != 2 || a.Burn.ByMachine[0].Machine != "machine-a" || math.Abs(a.Burn.ByMachine[0].Share-0.75) > 1e-9 ||
		math.Abs(a.Burn.ByMachine[1].RatePerHour-0.05) > 1e-9 || !a.Burn.ByMachine[1].Estimate {
		t.Fatalf("by machine = %+v", a.Burn.ByMachine)
	}
	wantDay := WindowTotals{Requests: 4, Input: 1150, Output: 125, CacheRead: 70, CacheCreation: 55}
	wantWeek := WindowTotals{Requests: 5, Input: 1150, Output: 132, CacheRead: 70, CacheCreation: 55}
	if a.Totals.Day != wantDay || a.Totals.Week != wantWeek {
		t.Fatalf("totals = %+v, want %+v and %+v", a.Totals, wantDay, wantWeek)
	}
	if b := snap.Accounts[0]; b.Totals != (Totals{}) || len(b.Burn.ByMachine) != 0 || b.Burn.FiveHour.Reason != "no_reading" {
		t.Fatalf("idle account = %+v", b)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"burn":{"5h":{`, `"7d":{"rate_per_hour":null`, `"by_machine":[]`, `"totals":{"24h":{"requests":0`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("snapshot lacks %s: %s", key, body)
		}
	}
}
