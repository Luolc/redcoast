package session

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

// TestRecordQuotaKeepsLatestPerWindow checks that each window's latest reading replaces
// the previous one and that a window absent from a later response keeps its reading.
func TestRecordQuotaKeepsLatestPerWindow(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	reset := c.Now().Add(3 * time.Hour)
	if err := store.RecordQuota(ctx, "sample-a", []Reading{
		{Window: Window5h, Utilization: 0.25, Status: "allowed", ResetAt: reset, Source: "response"},
		{Window: Window7d, Utilization: 0.5, Status: "allowed_warning", Source: "response"},
	}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if err := store.RecordQuota(ctx, "sample-a", []Reading{{Window: Window5h, Utilization: 0.3, Status: "rejected", ResetAt: reset, Source: "429"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordQuota(ctx, "sample-a", nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.LatestQuota(ctx, "sample-a")
	if err != nil || len(got) != 2 {
		t.Fatalf("quota=%+v err=%v", got, err)
	}
	if q := got[Window5h]; q.Utilization != 0.3 || q.Status != "rejected" || !q.ResetAt.Equal(reset) || q.Source != "429" || !q.ObservedAt.Equal(c.Now()) {
		t.Fatalf("5h=%+v", q)
	}
	if q := got[Window7d]; q.Utilization != 0.5 || q.Status != "allowed_warning" || !q.ResetAt.IsZero() || q.Source != "response" || !q.ObservedAt.Equal(c.Now().Add(-time.Minute)) {
		t.Fatalf("7d=%+v", q)
	}
	if err := store.RecordQuota(ctx, "sample-a", []Reading{{Window: "1h", Utilization: 0.1, Source: "response"}}); err == nil {
		t.Fatal("unknown window accepted")
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), -0.1} {
		if err := store.RecordQuota(ctx, "sample-a", []Reading{{Window: Window7d, Utilization: bad, Source: "response"}}); err == nil {
			t.Fatalf("utilization %v accepted", bad)
		}
	}
	if again, err := store.LatestQuota(ctx, "sample-a"); err != nil || again[Window7d].Utilization != 0.5 || again[Window5h].Utilization != 0.3 {
		t.Fatalf("refused readings changed the stored ones: %+v %v", again, err)
	}
	if other, err := store.LatestQuota(ctx, "sample-b"); err != nil || len(other) != 0 {
		t.Fatalf("another account has readings: %+v %v", other, err)
	}
}

// TestThresholds checks the defaults and that edits to the settings table take effect
// on the next read, while a bad edit is reported instead of defaulted.
func TestThresholds(t *testing.T) {
	store, _ := open(t, newClock())
	ctx := context.Background()
	soft, hard, err := store.Thresholds(ctx)
	if err != nil || soft != 0.9 || hard != 0.95 {
		t.Fatalf("defaults soft=%v hard=%v err=%v", soft, hard, err)
	}
	if _, err := store.db.Exec("UPDATE settings SET value = '0.7' WHERE key = 'soft'"); err != nil {
		t.Fatal(err)
	}
	if soft, _, err = store.Thresholds(ctx); err != nil || soft != 0.7 {
		t.Fatalf("edited soft=%v err=%v", soft, err)
	}
	for _, bad := range []string{"seventy", "1.5", "0.97", "NaN", "+Inf", "-Inf"} {
		if _, err := store.db.Exec("UPDATE settings SET value = ? WHERE key = 'soft'", bad); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Thresholds(ctx); err == nil {
			t.Fatalf("soft=%q accepted", bad)
		}
	}
}

// TestSetThresholds checks that both thresholds are written together, with the time of
// the write, and that a pair Thresholds would refuse leaves the stored pair as it was.
func TestSetThresholds(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	c.Advance(time.Minute)
	if err := store.SetThresholds(ctx, "0.85", "0.97"); err != nil {
		t.Fatal(err)
	}
	if soft, hard, err := store.Thresholds(ctx); err != nil || soft != 0.85 || hard != 0.97 {
		t.Fatalf("soft=%v hard=%v err=%v", soft, hard, err)
	}
	if n := count(t, store, "SELECT COUNT(*) FROM settings WHERE updated_at = ?", c.Now().UnixMilli()); n != 2 {
		t.Fatalf("%d settings rows carry the time of the write, want 2", n)
	}
	for _, bad := range [][2]string{{"0.97", "0.85"}, {"0.9", "1.5"}, {"-0.1", "0.9"}, {"ninety", "0.95"}} {
		if err := store.SetThresholds(ctx, bad[0], bad[1]); err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if soft, hard, err := store.Thresholds(ctx); err != nil || soft != 0.85 || hard != 0.97 {
			t.Fatalf("after %q: soft=%v hard=%v err=%v", bad, soft, hard, err)
		}
	}
}

// TestMigrateFromVersionOne opens a file left at schema version 1 and checks that the
// plan tables and their seeds are added without touching the version-1 data.
func TestMigrateFromVersionOne(t *testing.T) {
	c := newClock()
	store, path := open(t, c)
	ctx := context.Background()
	issued, err := store.Issue(ctx, "m", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Turn the file back into a version-1 file.
	for _, statement := range append(undoV7, "ALTER TABLE traffic DROP COLUMN request", "ALTER TABLE traffic DROP COLUMN route", "DROP TABLE machines", "DROP TABLE plan_schedule", "DROP TABLE plans", "PRAGMA user_version = 1") {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var plans int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM plans").Scan(&plans); err != nil || plans != 3 {
		t.Fatalf("seeded plans=%d err=%v", plans, err)
	}
	schedule(t, reopened, "sample-a", "pro")
	if _, err := reopened.Bind(ctx, issued.Token, []string{"sample-a"}); err != nil {
		t.Fatalf("version-1 session lost: %v", err)
	}
}

// TestMigrateRescalesQuota opens a version-3 file whose sample-a reading was stored by
// a gateway that read the header as a percentage: 0.0095 for a header of 0.95, past
// hard. After the upgrade the reading is 0.95 and a new session goes to sample-b.
// Control arm: the same row in a file already at version 4 stays as it is, and the
// session goes to sample-a, whose reset is nearer.
func TestMigrateRescalesQuota(t *testing.T) {
	for _, arm := range []struct {
		name    string
		version int
		want    float64
		alias   string
	}{{"version_3", 3, 0.95, "sample-b"}, {"version_4", 4, 0.0095, "sample-a"}} {
		t.Run(arm.name, func(t *testing.T) {
			c := newClock()
			store, path := open(t, c)
			ctx := context.Background()
			reading := Reading{Window: Window7d, Utilization: 0.0095, Status: "allowed", ResetAt: c.Now().Add(24 * time.Hour), Source: "response"}
			if err := store.RecordQuota(ctx, "sample-a", []Reading{reading}); err != nil {
				t.Fatal(err)
			}
			for _, statement := range append(undoV7, "ALTER TABLE traffic DROP COLUMN request", "ALTER TABLE traffic DROP COLUMN route") {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", arm.version)); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path, Config{Now: c.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if q, err := reopened.LatestQuota(ctx, "sample-a"); err != nil || math.Abs(q[Window7d].Utilization-arm.want) > 1e-12 {
				t.Fatalf("7d=%+v err=%v, want %v", q[Window7d], err, arm.want)
			}
			issued, err := reopened.Issue(ctx, "m", LocalSource, nil)
			if err != nil {
				t.Fatal(err)
			}
			if bound, err := reopened.Bind(ctx, issued.Token, accounts); err != nil || bound.Alias != arm.alias {
				t.Fatalf("bound=%+v err=%v, want %s", bound, err, arm.alias)
			}
		})
	}
}

// TestQuotaHistorySampling records readings of one window at the boundaries of the
// sampling rule and reads back which were kept: the first one; a changed one only once
// quotaSampleGap has passed since the last kept one; an unchanged one only once
// quotaSampleHeartbeat has; none while the clock is behind the last kept one. The
// other window and account are sampled on their own, and every reading still replaces
// the latest.
func TestQuotaHistorySampling(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	start := c.Now()
	record := func(alias string, window string, utilization float64) {
		t.Helper()
		if err := store.RecordQuota(ctx, alias, []Reading{{Window: window, Utilization: utilization, Source: "response"}}); err != nil {
			t.Fatal(err)
		}
	}
	at := func(offset time.Duration) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.now = start.Add(offset)
	}
	steps := []struct {
		offset      time.Duration
		utilization float64
	}{
		{0, 0.10}, // kept: the first
		{quotaSampleGap - time.Millisecond, 0.20},                        // changed, under the gap
		{quotaSampleGap, 0.20},                                           // changed from the kept 0.10, at the gap: kept
		{quotaSampleGap + 30*time.Minute, 0.20},                          // unchanged, under the heartbeat
		{quotaSampleGap + quotaSampleHeartbeat - time.Millisecond, 0.20}, // unchanged, just under it
		{quotaSampleGap + quotaSampleHeartbeat, 0.20},                    // unchanged, at the heartbeat: kept
		{time.Minute, 0.90},                                              // the clock stepped back
	}
	for _, s := range steps {
		at(s.offset)
		record("sample-a", Window5h, s.utilization)
	}
	at(time.Minute)
	record("sample-a", Window7d, 0.40)
	record("sample-b", Window5h, 0.30)
	rows, err := store.db.Query("SELECT alias, window, at, utilization FROM quota_history ORDER BY alias, window, at")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var alias, window string
		var ms int64
		var utilization float64
		if err := rows.Scan(&alias, &window, &ms, &utilization); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %s %v %.2f", alias, window, fromMillis(ms).Sub(start), utilization))
	}
	want := []string{"sample-a 5h 0s 0.10", "sample-a 5h 5m0s 0.20", "sample-a 5h 1h5m0s 0.20", "sample-a 7d 1m0s 0.40", "sample-b 5h 1m0s 0.30"}
	if err := rows.Err(); err != nil || !slices.Equal(got, want) {
		t.Fatalf("history=%q err=%v, want %q", got, err, want)
	}
	if q, err := store.LatestQuota(ctx, "sample-a"); err != nil || q[Window5h].Utilization != 0.90 {
		t.Fatalf("latest 5h=%+v err=%v, want the last reading", q[Window5h], err)
	}
}

// TestQuotaHistorySpan reads back each window's history over that window's length:
// readings at the start of the span are in it, ones a millisecond older are not.
func TestQuotaHistorySpan(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	now := c.Now()
	for _, r := range []struct {
		window string
		at     time.Time
		value  float64
	}{
		{Window5h, now.Add(-5*time.Hour - time.Millisecond), 0.1},
		{Window5h, now.Add(-5 * time.Hour), 0.2},
		{Window7d, now.Add(-5*time.Hour - time.Millisecond), 0.3},
		{Window7d, now.Add(-7*24*time.Hour - time.Millisecond), 0.4},
		{Window7d, now.Add(-7 * 24 * time.Hour), 0.5},
	} {
		if _, err := store.db.Exec("INSERT INTO quota_history (alias, window, at, utilization) VALUES ('sample-a', ?, ?, ?)", r.window, r.at.UnixMilli(), r.value); err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.QuotaHistory(ctx, "sample-a")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]QuotaPoint{
		Window5h: {{At: now.Add(-5 * time.Hour), Utilization: 0.2}},
		Window7d: {{At: now.Add(-7 * 24 * time.Hour), Utilization: 0.5}, {At: now.Add(-5*time.Hour - time.Millisecond), Utilization: 0.3}},
	}
	if len(history) != len(want) {
		t.Fatalf("history=%+v, want %+v", history, want)
	}
	for window, points := range want {
		if !slices.EqualFunc(history[window], points, func(a, b QuotaPoint) bool { return a.At.Equal(b.At) && a.Utilization == b.Utilization }) {
			t.Errorf("%s history=%+v, want %+v", window, history[window], points)
		}
	}
	if other, err := store.QuotaHistory(ctx, "sample-b"); err != nil || other == nil || len(other) != 0 {
		t.Fatalf("an account without history: %+v err=%v", other, err)
	}
}
