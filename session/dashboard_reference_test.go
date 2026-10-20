package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reference below is the dashboard's earlier implementation of the tables window,
// connects and days build: one SQL query per table, with the grouping, the ordering and
// the error rule in SQL. TestDashboardMatchesReference checks that the one-pass reads
// give the same JSON.

// referenceFailed is the SQL form of failed, over a traffic row aliased t.
const referenceFailed = `(t.result <> 'ok' AND NOT (t.kind = 'connect' AND t.result IN ('idle_timeout', 'max_duration'))
	OR COALESCE(t.status, 0) >= 400)`

// referenceTokenSums is the SQL list, over traffic rows aliased t, of the sums of the
// four token counts, zero for a group in which no row reported usage.
const referenceTokenSums = `COALESCE(SUM(t.input_tokens), 0), COALESCE(SUM(t.output_tokens), 0),
	COALESCE(SUM(t.cache_read_tokens), 0), COALESCE(SUM(t.cache_creation_tokens), 0)`

// referenceTables reads the tables window, connects and days build the way the earlier
// implementation did, into a Snapshot with only those tables set.
func referenceTables(t *testing.T, r *Reader, now time.Time) Snapshot {
	t.Helper()
	ctx := context.Background()
	db := r.store.db
	nowMs := now.UnixMilli()
	since := nowMs - dashboardHours*time.Hour.Milliseconds()
	dayStart := now.Truncate(24*time.Hour).AddDate(0, 0, 1-dashboardDays).UnixMilli()
	out := Snapshot{Usage: []UsagePoint{}, ModelUsage: []ModelUsage{}, SourceCounts: []SourceCount{},
		ClientHourly: []ClientHour{}, Destinations: []Destination{}}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(collect(ctx, db, &out.Usage, func(rows *sql.Rows) (UsagePoint, error) {
		var v UsagePoint
		var hour int64
		err := rows.Scan(&hour, &v.Alias, &v.Machine, &v.Kind, &v.Requests, &v.BytesUp, &v.BytesDown, &v.Errors, &v.Limited,
			&v.InputTokens, &v.OutputTokens, &v.CacheRead, &v.CacheCreation)
		v.Hour = fromMillis(hour)
		return v, err
	}, `SELECT t.ts / 3600000 * 3600000 AS hour, COALESCE(t.alias, '') AS a, COALESCE(s.client_machine, '') AS m, t.kind,
		COUNT(*), SUM(t.bytes_up), SUM(t.bytes_down), SUM(`+referenceFailed+`), SUM(t.result = 'limited'), `+referenceTokenSums+`
		FROM traffic t LEFT JOIN sessions s ON s.id = t.session_id WHERE t.ts >= ?
		GROUP BY hour, a, m, t.kind ORDER BY hour, a, m, t.kind`, since))
	check(collect(ctx, db, &out.ModelUsage, func(rows *sql.Rows) (ModelUsage, error) {
		var v ModelUsage
		var hour int64
		err := rows.Scan(&hour, &v.Alias, &v.Model, &v.Requests, &v.InputTokens, &v.OutputTokens, &v.CacheRead, &v.CacheCreation)
		v.Hour = fromMillis(hour)
		return v, err
	}, `SELECT t.ts / 3600000 * 3600000 AS hour, COALESCE(t.alias, '') AS a, COALESCE(t.model, '') AS md, COUNT(*), `+referenceTokenSums+`
		FROM traffic t WHERE t.ts >= ? AND t.input_tokens IS NOT NULL GROUP BY hour, a, md ORDER BY hour, a, md`, since))
	check(collect(ctx, db, &out.SourceCounts, func(rows *sql.Rows) (SourceCount, error) {
		var v SourceCount
		return v, rows.Scan(&v.Alias, &v.Machine, &v.Kind, &v.Result, &v.Status, &v.Error, &v.Count)
	}, `SELECT COALESCE(t.alias, '') AS a, COALESCE(s.client_machine, '') AS m, t.kind, t.result, COALESCE(t.status, 0) AS st, `+referenceFailed+`, COUNT(*) AS n
		FROM traffic t LEFT JOIN sessions s ON s.id = t.session_id WHERE t.ts >= ?
		GROUP BY a, m, t.kind, t.result, st ORDER BY a, m, t.kind, n DESC, t.result, st`, since))
	out.Latency = referenceLatency(t, db, since)
	check(collect(ctx, db, &out.Destinations, func(rows *sql.Rows) (Destination, error) {
		var v Destination
		var last int64
		err := rows.Scan(&v.Host, &v.Port, &v.Alias, &v.Route, &v.Tunnels, &v.OK, &v.BytesUp, &v.BytesDown, &last)
		v.Last = fromMillis(last)
		return v, err
	}, `SELECT host, port, COALESCE(alias, '') AS a, COALESCE(route, '') AS rt, COUNT(*) AS n, SUM(result = 'ok'), SUM(bytes_up), SUM(bytes_down), MAX(ts)
		FROM traffic WHERE kind = 'connect' AND ts >= ? GROUP BY host, port, a, rt ORDER BY n DESC, host, port, a, rt`, dayStart))
	check(collect(ctx, db, &out.ClientHourly, func(rows *sql.Rows) (ClientHour, error) {
		var v ClientHour
		var hour int64
		err := rows.Scan(&hour, &v.Machine, &v.Client, &v.Requests)
		v.Hour = fromMillis(hour)
		return v, err
	}, `SELECT t.ts / 3600000 * 3600000 AS hour, s.client_machine AS m, `+clientColumn+` AS c, COUNT(*)
		FROM traffic t JOIN sessions s ON s.id = t.session_id WHERE t.ts >= ? GROUP BY hour, m, c ORDER BY hour, m, c`, since))
	out.Days = referenceDays(t, r, dayStart, nowMs)
	return out
}

// referenceLatency is the earlier latency: the durations of the inference requests
// since since that are no error, read with their session's machine joined.
func referenceLatency(t *testing.T, db *sql.DB, since int64) LatencyView {
	t.Helper()
	type row struct {
		hour           int64
		alias, machine string
		duration       int64
	}
	var rows []row
	err := collect(context.Background(), db, &rows, func(rows *sql.Rows) (row, error) {
		var v row
		return v, rows.Scan(&v.hour, &v.alias, &v.machine, &v.duration)
	}, `SELECT t.ts / 3600000 * 3600000, COALESCE(t.alias, ''), COALESCE(s.client_machine, ''), t.duration_ms
		FROM traffic t LEFT JOIN sessions s ON s.id = t.session_id WHERE t.ts >= ? AND t.kind = 'inference' AND NOT `+referenceFailed, since)
	if err != nil {
		t.Fatal(err)
	}
	hours, accounts, machines := map[int64][]int64{}, map[string][]int64{}, map[string][]int64{}
	for _, v := range rows {
		hours[v.hour] = append(hours[v.hour], v.duration)
		accounts[v.alias] = append(accounts[v.alias], v.duration)
		machines[v.machine] = append(machines[v.machine], v.duration)
	}
	out := LatencyView{Hourly: []HourLatency{}, Accounts: []AccountLatency{}, Machines: []MachineLatency{}}
	for _, h := range slices.Sorted(maps.Keys(hours)) {
		out.Hourly = append(out.Hourly, HourLatency{Hour: fromMillis(h), Latency: spread(hours[h])})
	}
	for _, a := range slices.Sorted(maps.Keys(accounts)) {
		out.Accounts = append(out.Accounts, AccountLatency{Alias: a, Latency: spread(accounts[a])})
	}
	for _, m := range slices.Sorted(maps.Keys(machines)) {
		out.Machines = append(out.Machines, MachineLatency{Machine: m, Latency: spread(machines[m])})
	}
	return out
}

// referenceDays is the earlier days, which read the limited rows and the tunnels with
// queries of their own.
func referenceDays(t *testing.T, r *Reader, start, now int64) []DayCounts {
	t.Helper()
	ctx := context.Background()
	const day = 24 * 60 * 60 * 1000
	out := make([]DayCounts, 0, dashboardDays)
	for d := start; d <= now; d += day {
		out = append(out, DayCounts{Day: fromMillis(d)})
	}
	at := func(ts int64) *DayCounts { return &out[(ts-start)/day] }
	db := r.store.db
	var limited [][2]int64
	if err := collect(ctx, db, &limited, scanPair, "SELECT ts, kind = 'connect' FROM traffic WHERE ts BETWEEN ? AND ? AND result = 'limited'", start, now); err != nil {
		t.Fatal(err)
	}
	for _, l := range limited {
		if l[1] == 1 {
			at(l[0]).LimitedConnect++
		} else {
			at(l[0]).LimitedInference++
		}
	}
	var ended []int64
	if err := collect(ctx, db, &ended, scanOne, "SELECT ended_at FROM sessions WHERE end_reason = 'idle' AND ended_at BETWEEN ? AND ?", start, now); err != nil {
		t.Fatal(err)
	}
	for _, ts := range ended {
		at(ts).Leaked++
	}
	var tunnels, sessions [][2]int64
	if err := collect(ctx, db, &tunnels, scanPair, "SELECT ts, ts + duration_ms FROM traffic WHERE kind = 'connect' AND result IN ('ok', 'idle_timeout', 'max_duration') AND ts >= ? AND ts + duration_ms > ?", start-r.maxTunnel.Milliseconds(), start); err != nil {
		t.Fatal(err)
	}
	if err := collect(ctx, db, &sessions, scanPair, "SELECT created_at, COALESCE(ended_at, ?) FROM sessions WHERE COALESCE(ended_at, ?) >= ?", now, now, start); err != nil {
		t.Fatal(err)
	}
	for i := range out {
		from := start + int64(i)*day
		out[i].TunnelPeak = peak(tunnels, from, from+day)
		out[i].SessionPeak = peak(sessions, from, from+day)
	}
	return out
}

// seedReference fills store with made-up sessions and traffic rows drawn from a fixed
// seed: more sessions than one lookup batch reads, rows across nine days and a little
// after now, NULL and empty columns, rows of a missing session and of a session with an empty ID, launch metadata that
// names no known client or that SQLite cannot parse, and the window bounds exactly.
func seedReference(t *testing.T, store *Store, now time.Time, maxTunnel time.Duration) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(7, 11))
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	pick := func(values ...any) any { return values[rng.IntN(len(values))] }
	nowMs := now.UnixMilli()
	const hour, day = int64(3600000), int64(86400000)
	since := nowMs - dashboardHours*hour
	dayStart := now.Truncate(24*time.Hour).AddDate(0, 0, 1-dashboardDays).UnixMilli()
	metas := []any{nil, `{"client":"claude-code","client_version":"2.1.3"}`, `{"client":"pi"}`, `{"client":"codex-cli","client_version":7}`,
		`{"client":"other-tool"}`, `not json`, `{"client":"pi","client":"claude-code"}`,
		`{"client":"pi","a":` + strings.Repeat("[", 1500) + strings.Repeat("]", 1500) + `}`}
	var ids []any
	for i := range 3*sessionBatch/2 + 7 {
		id := fmt.Sprintf("s-%04d-%016x", i, rng.Uint64())
		created := nowMs - rng.Int64N(10*day)
		var ended, reason any
		if rng.IntN(3) > 0 {
			ended, reason = created+rng.Int64N(2*day), pick("idle", "deleted", "client_exit")
		}
		exec("INSERT INTO sessions VALUES (?, ?, ?, '192.0.2.1:5000', ?, ?, ?, ?, ?)",
			id, "hash-"+id, pick("machine-a", "machine-b", "machine-c"), metas[rng.IntN(len(metas))], created, created+rng.Int64N(hour), ended, reason)
		ids = append(ids, id)
	}
	// A session with an empty ID, which a row of no session must not join.
	exec("INSERT INTO sessions VALUES ('', 'hash-empty', 'machine-c', '192.0.2.1:5000', NULL, ?, ?, NULL, NULL)", nowMs-day, nowMs-day)
	tsOf := func() int64 {
		switch rng.IntN(12) {
		case 0:
			return pick(since, since-1, dayStart, dayStart-1, nowMs, nowMs+1, dayStart-maxTunnel.Milliseconds(), dayStart-maxTunnel.Milliseconds()-1).(int64)
		case 1:
			return nowMs + rng.Int64N(hour) // stamped after now, as after the clock stepped back
		case 2, 3, 4:
			return nowMs - 9*day + rng.Int64N(9*day)
		default:
			return nowMs - rng.Int64N(dashboardHours*hour+hour)
		}
	}
	for range 6000 {
		ts := tsOf()
		session := pick(nil, "", "missing-session", ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))])
		alias := pick(nil, "", "sample-a", "sample-b")
		kind := pick("inference", "connect").(string)
		result := pick("ok", "ok", "ok", "limited", "idle_timeout", "max_duration", "egress_failed", "upstream_failed")
		host, route, duration := "api.example.com", any(nil), rng.Int64N(120000)
		var status, model, tokens any = pick(nil, 0, 200, 200, 429, 500, 503), nil, nil
		if kind == "connect" {
			host, route, status = pick("a.example", "b.example", "c.example", "d.example").(string), pick(nil, "direct", "account"), nil
			if rng.IntN(4) == 0 {
				duration = rng.Int64N(3 * maxTunnel.Milliseconds() / 2) // may start before the daily window and overlap it
			}
			if ts < dayStart && rng.IntN(4) == 0 {
				duration = dayStart - ts // ends exactly at the window start
			}
		} else if rng.IntN(2) == 0 {
			model, tokens = pick(nil, "", "model-x", "model-y"), rng.Int64N(1000)
		}
		input := tokens
		if rng.IntN(5) == 0 {
			input = nil // the other counts are still summed into usage
		}
		exec(`INSERT INTO traffic (ts, session_id, alias, kind, host, port, bytes_up, bytes_down, duration_ms, result, status, route, model,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ts, session, alias, kind, host, pick(443, 8443), rng.Int64N(5000), rng.Int64N(50000), duration, result, status, route, model,
			input, tokens, pick(nil, tokens), tokens)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestDashboardMatchesReference checks that the one-pass reads give, field for field, the
// same JSON as the earlier one-query-per-table implementation, on made-up data that
// reaches every grouping column, the ties of each ordering and the window bounds.
func TestDashboardMatchesReference(t *testing.T) {
	c := newClock()
	store, path := open(t, c)
	reader := openReader(t, path, c)
	seedReference(t, store, c.Now(), reader.maxTunnel)
	snap, err := reader.Snapshot(context.Background(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	want := referenceTables(t, reader, snap.At)
	tables := []struct {
		name      string
		got, want any
		size      int
	}{
		{"usage", snap.Usage, want.Usage, len(want.Usage)},
		{"model_usage", snap.ModelUsage, want.ModelUsage, len(want.ModelUsage)},
		{"result_sources", snap.SourceCounts, want.SourceCounts, len(want.SourceCounts)},
		{"client_hourly", snap.ClientHourly, want.ClientHourly, len(want.ClientHourly)},
		{"latency", snap.Latency, want.Latency, len(want.Latency.Machines)},
		{"destinations", snap.Destinations, want.Destinations, len(want.Destinations)},
		{"days", snap.Days, want.Days, len(want.Days)},
	}
	for _, table := range tables {
		got, err := json.Marshal(table.got)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := json.Marshal(table.want)
		if err != nil {
			t.Fatal(err)
		}
		if table.size < 2 {
			t.Errorf("%s: the reference has %d rows, too few to compare", table.name, table.size)
		}
		if string(got) != string(reference) {
			t.Errorf("%s differs from the reference:\n got  %s\n want %s", table.name, got, reference)
		}
	}
}
