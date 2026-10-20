package session

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// openReader opens a read-only reader of the file at path on clock c.
func openReader(t *testing.T, path string, c *clock) *Reader {
	t.Helper()
	reader, err := OpenReadOnly(path, Config{Now: c.Now}, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func TestDashboardEmptyStore(t *testing.T) {
	c := newClock()
	_, path := open(t, c)
	snap, err := openReader(t, path, c).Snapshot(context.Background(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 2 || len(snap.Sessions) != 0 || len(snap.Machines) != 0 || len(snap.Destinations) != 0 || len(snap.History) != 0 || snap.DBBytes <= 0 {
		t.Fatalf("empty snapshot = %+v", snap)
	}
	if len(snap.Days) != 7 || !snap.Days[6].Day.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) || snap.Days[6] != (DayCounts{Day: snap.Days[6].Day}) {
		t.Fatalf("days = %+v", snap.Days)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"machines", "sessions", "hourly", "results", "usage", "result_sources", "clients", "client_hourly", "destinations", "history"} {
		if !strings.Contains(string(body), `"`+table+`":[]`) {
			t.Fatalf("empty table %s is not an empty array: %s", table, body)
		}
	}
}

func TestDashboardSnapshot(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	now := c.Now()
	credential := strings.Repeat("c", 43)
	if err := store.AddMachine(ctx, "machine-a", hashOf(credential), 12); err != nil {
		t.Fatal(err)
	}
	// A session the client never ends: the idle expiry (30 s here) ends it, a leak.
	if _, err := store.IssueForMachine(ctx, credential, "198.51.100.2", nil); err != nil {
		t.Fatal(err)
	}
	c.Advance(31 * time.Second)
	if n, err := store.ExpireIdle(ctx); err != nil || n != 1 {
		t.Fatalf("ExpireIdle = %d, %v", n, err)
	}
	live, err := store.IssueForMachine(ctx, credential, "198.51.100.2", nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindFrom(ctx, live.Token, "198.51.100.2", accounts)
	if err != nil {
		t.Fatal(err)
	}
	// A live session with no request for 25 h, written directly: the idle expiry of
	// this store would end it on any read through Bind.
	idle, err := store.Issue(ctx, "local", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE sessions SET created_at = ?, last_seen_at = ? WHERE id = ?", now.Add(-25*time.Hour).UnixMilli(), now.Add(-25*time.Hour).UnixMilli(), idle.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Pause(ctx, "sample-a", now.Add(time.Hour), "manual check"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "sample-b", Plan: "pro", From: time.UnixMilli(0), To: now.Add(72 * time.Hour), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlanSchedule(ctx, PlanPeriod{Alias: "sample-b", Plan: "max-5x", From: now.Add(72 * time.Hour), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	tunnel := func(at time.Time, d time.Duration, host, result string) {
		t.Helper()
		route := "account"
		if host == "github.com" && at.After(now.Add(-24*time.Hour)) {
			route = "direct"
		}
		row := Traffic{At: at, SessionID: live.ID, Alias: bound.Alias, Kind: "connect", Host: host, Port: 443, Route: route, BytesUp: 10, BytesDown: 100, Duration: d, Result: result}
		if err := store.RecordTraffic(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	// Three tunnels, at most two at once: the first ends when the third starts.
	start := now.Add(-time.Hour)
	tunnel(start, 10*time.Second, "github.com", "ok")
	tunnel(start.Add(5*time.Second), 15*time.Second, "github.com", "ok")
	tunnel(start.Add(10*time.Second), 5*time.Second, "pypi.org", "egress_failed")
	tunnel(now.Add(-48*time.Hour), time.Second, "pypi.org", "ok")
	// The same target on the other route is a destination of its own.
	tunnel(now.Add(-72*time.Hour), time.Second, "github.com", "ok")
	// A 50 h tunnel that started two days before the first shown day overlaps it.
	tunnel(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), 50*time.Hour, "long.example", "ok")
	tunnel(now.Add(-30*24*time.Hour), time.Second, "old.example", "ok")
	tunnel(start, 0, "github.com", "limited")
	if err := store.RecordTraffic(ctx, Traffic{At: start, Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "limited", Status: 503}); err != nil {
		t.Fatal(err)
	}

	snap, err := openReader(t, path, c).Snapshot(ctx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	byAlias := map[string]AccountView{}
	for _, a := range snap.Accounts {
		byAlias[a.Alias] = a
	}
	if a := byAlias["sample-a"]; a.PauseReason != "manual check" || a.NextPlan != nil {
		t.Fatalf("sample-a = %+v", a)
	}
	if b := byAlias["sample-b"]; b.NextPlan == nil || b.NextPlan.Name != "max-5x" || b.Plan == nil || b.Plan.Name != "pro" {
		t.Fatalf("sample-b = %+v", b)
	}
	if len(snap.Machines) != 1 || snap.Machines[0].LiveSessions != 1 || !snap.Machines[0].LastSeen.Equal(c.Now()) {
		t.Fatalf("machines = %+v", snap.Machines)
	}
	if len(snap.Sessions) != 2 || snap.IdleSessions != 1 || snap.Sessions[1].ID != live.ID[:8] || snap.Sessions[1].Alias != bound.Alias {
		t.Fatalf("sessions = %+v, idle %d", snap.Sessions, snap.IdleSessions)
	}
	if len(snap.Destinations) != 3 || snap.Destinations[0] != (Destination{Host: "github.com", Port: 443, Alias: bound.Alias, Route: "direct", Tunnels: 3, OK: 2, BytesUp: 30, BytesDown: 300, Last: start.Add(5 * time.Second)}) ||
		snap.Destinations[1].Host != "pypi.org" || snap.Destinations[1].Route != "account" || snap.Destinations[1].Tunnels != 2 ||
		snap.Destinations[2].Host != "github.com" || snap.Destinations[2].Route != "account" || snap.Destinations[2].Tunnels != 1 {
		t.Fatalf("destinations = %+v", snap.Destinations)
	}
	results := map[string]int{}
	for _, r := range snap.Results {
		results[r.Kind+"/"+r.Result] += r.Count
	}
	if results["connect/ok"] != 2 || results["connect/limited"] != 1 || results["connect/egress_failed"] != 1 || results["inference/limited"] != 1 {
		t.Fatalf("results = %+v", snap.Results)
	}
	today := DayCounts{Day: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), LimitedInference: 1, LimitedConnect: 1, TunnelPeak: 2, SessionPeak: 2, Leaked: 1}
	if snap.Days[6] != today || snap.Days[4].TunnelPeak != 1 || snap.Days[5].SessionPeak != 1 || snap.Days[0].TunnelPeak != 1 {
		t.Fatalf("days = %+v", snap.Days)
	}
	if len(snap.History) == 0 || snap.History[0].Session != live.ID[:8] {
		t.Fatalf("history = %+v", snap.History)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{live.Token, live.ID, hashOf(credential), hashToken(live.Token), credential} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("snapshot carries a credential or a full session ID: %s", body)
		}
	}
	// Control arm: the cut ID is there, so the search above read the session table.
	if !strings.Contains(string(body), live.ID[:8]) {
		t.Fatal("snapshot lacks the cut session ID")
	}
}

func TestReaderCannotWrite(t *testing.T) {
	c := newClock()
	store, path := open(t, c)
	reader := openReader(t, path, c)
	const write = "INSERT INTO settings (key, value, updated_at) VALUES ('probe', '1', 0)"
	if _, err := reader.store.db.Exec(write); err == nil {
		t.Fatal("the reader wrote to the store")
	}
	// Control arm: the same statement goes through the gateway's own connection.
	if _, err := store.db.Exec(write); err != nil {
		t.Fatal(err)
	}
}

func TestPeak(t *testing.T) {
	intervals := [][2]int64{{0, 10}, {5, 20}, {10, 15}, {30, 30}, {100, 200}}
	cases := map[[2]int64]int{{0, 50}: 2, {0, 5}: 1, {150, 160}: 1, {200, 300}: 0, {30, 31}: 1}
	for window, want := range cases {
		if got := peak(intervals, window[0], window[1]); got != want {
			t.Errorf("peak in %v = %d, want %d", window, got, want)
		}
	}
}

func TestDashboardUsageBySource(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	now := c.Now()
	one, err := store.Issue(ctx, "devbox-1", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := store.Issue(ctx, "devbox-2", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := func(at time.Time, session, alias, kind, result string, status int) {
		t.Helper()
		row := Traffic{At: at, SessionID: session, Alias: alias, Kind: kind, Host: "api.example.com", Port: 443, BytesUp: 10, BytesDown: 100, Result: result, Status: status}
		if err := store.RecordTraffic(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	hour := now.Add(-30 * time.Minute) // in the hour starting at 11:00
	// The error boundaries, all on devbox-1 and sample-a: four rows that are no error,
	// then five that are.
	record(hour, one.ID, "sample-a", "inference", "ok", 200)
	record(hour, one.ID, "sample-a", "inference", "ok", 399)
	record(hour, one.ID, "sample-a", "connect", "idle_timeout", 200)
	record(hour, one.ID, "sample-a", "connect", "max_duration", 200)
	record(hour, one.ID, "sample-a", "inference", "ok", 400)
	record(hour, one.ID, "sample-a", "inference", "idle_timeout", 200)
	record(hour, one.ID, "sample-a", "inference", "limited", 0)
	record(hour, one.ID, "sample-a", "connect", "limited", 0)
	record(hour, one.ID, "sample-a", "connect", "egress_failed", 0)
	// The same account on the other machine, and another account on the first one.
	record(hour, two.ID, "sample-a", "inference", "ok", 200)
	record(hour, one.ID, "sample-b", "inference", "upstream_failed", 0)
	// A request of no session and no account, on the window's first millisecond, and
	// one a millisecond before the window.
	since := now.Add(-24 * time.Hour)
	record(since, "", "", "inference", "auth_failed", 0)
	record(since.Add(-time.Millisecond), one.ID, "sample-a", "inference", "ok", 500)

	snap, err := openReader(t, path, c).Snapshot(ctx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	at11 := time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC)
	want := []UsagePoint{
		{Hour: since, Kind: "inference", Requests: 1, BytesUp: 10, BytesDown: 100, Errors: 1},
		{Hour: at11, Alias: "sample-a", Machine: "devbox-1", Kind: "connect", Requests: 4, BytesUp: 40, BytesDown: 400, Errors: 2, Limited: 1},
		{Hour: at11, Alias: "sample-a", Machine: "devbox-1", Kind: "inference", Requests: 5, BytesUp: 50, BytesDown: 500, Errors: 3, Limited: 1},
		{Hour: at11, Alias: "sample-a", Machine: "devbox-2", Kind: "inference", Requests: 1, BytesUp: 10, BytesDown: 100},
		{Hour: at11, Alias: "sample-b", Machine: "devbox-1", Kind: "inference", Requests: 1, BytesUp: 10, BytesDown: 100, Errors: 1},
	}
	if len(snap.Usage) != len(want) {
		t.Fatalf("usage = %+v", snap.Usage)
	}
	for i := range want {
		if got := snap.Usage[i]; got != want[i] || !got.Hour.Equal(want[i].Hour) {
			t.Errorf("usage[%d] = %+v, want %+v", i, got, want[i])
		}
	}
	type key struct{ alias, machine, kind, result string }
	type count struct {
		status, n int
		error     bool
	}
	got := map[key][]count{}
	for _, r := range snap.SourceCounts {
		k := key{r.Alias, r.Machine, r.Kind, r.Result}
		got[k] = append(got[k], count{r.Status, r.Count, r.Error})
	}
	wantCounts := map[key][]count{
		{"", "", "inference", "auth_failed"}:                     {{0, 1, true}},
		{"sample-a", "devbox-1", "inference", "ok"}:              {{200, 1, false}, {399, 1, false}, {400, 1, true}},
		{"sample-a", "devbox-1", "inference", "idle_timeout"}:    {{200, 1, true}},
		{"sample-a", "devbox-1", "inference", "limited"}:         {{0, 1, true}},
		{"sample-a", "devbox-1", "connect", "idle_timeout"}:      {{200, 1, false}},
		{"sample-a", "devbox-1", "connect", "max_duration"}:      {{200, 1, false}},
		{"sample-a", "devbox-1", "connect", "limited"}:           {{0, 1, true}},
		{"sample-a", "devbox-1", "connect", "egress_failed"}:     {{0, 1, true}},
		{"sample-a", "devbox-2", "inference", "ok"}:              {{200, 1, false}},
		{"sample-b", "devbox-1", "inference", "upstream_failed"}: {{0, 1, true}},
	}
	if len(got) != len(wantCounts) {
		t.Fatalf("result sources = %+v", snap.SourceCounts)
	}
	for k, w := range wantCounts {
		if !slices.Equal(got[k], w) {
			t.Errorf("result sources %+v = %+v, want %+v", k, got[k], w)
		}
	}
	// The existing tables keep their grouping: hourly by account and kind only.
	if len(snap.Hourly) != 4 || len(snap.Results) == 0 {
		t.Fatalf("hourly = %+v, results = %+v", snap.Hourly, snap.Results)
	}
}
