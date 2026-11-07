package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// statements records the text of every statement the "sqlite-recording" driver runs.
var statements struct {
	sync.Mutex
	texts []string
}

type recordingDriver struct{ driver.Driver }

type recordingInner interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.QueryerContext
	driver.ExecerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type recordingConn struct{ recordingInner }

func record(query string) {
	statements.Lock()
	defer statements.Unlock()
	statements.texts = append(statements.texts, query)
}

func (d recordingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return recordingConn{c.(recordingInner)}, nil
}

func (c recordingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	record(query)
	return c.recordingInner.PrepareContext(ctx, query)
}

func (c recordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	record(query)
	return c.recordingInner.QueryContext(ctx, query, args)
}

func (c recordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	record(query)
	return c.recordingInner.ExecContext(ctx, query, args)
}

func init() {
	plain, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register("sqlite-recording", recordingDriver{plain.Driver()})
	_ = plain.Close()
}

// oldTotals is the per-account query of totals before it became one scan.
func oldTotals(t *testing.T, r *Reader, alias string, now time.Time) Totals {
	t.Helper()
	day, week := now.Add(-dashboardHours*time.Hour).UnixMilli(), now.Add(-dashboardDays*24*time.Hour).UnixMilli()
	var out Totals
	d, w := &out.Day, &out.Week
	err := r.store.db.QueryRow(`SELECT
			COALESCE(SUM(ts >= ?1), 0), COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(input_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(output_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_read_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_creation_tokens, 0) END), 0),
			COUNT(*), COALESCE(SUM(COALESCE(input_tokens, 0)), 0), COALESCE(SUM(COALESCE(output_tokens, 0)), 0),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0)), 0), COALESCE(SUM(COALESCE(cache_creation_tokens, 0)), 0)
		FROM traffic WHERE alias = ?2 AND kind = 'inference' AND ts >= ?3`, day, alias, week).
		Scan(&d.Requests, &d.Input, &d.Output, &d.CacheRead, &d.CacheCreation, &w.Requests, &w.Input, &w.Output, &w.CacheRead, &w.CacheCreation)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// oldMachineTokens is the per-account query of machineTokens before it became one scan.
func oldMachineTokens(t *testing.T, r *Reader, alias string, since time.Time) map[string]int64 {
	t.Helper()
	rows, err := r.store.db.Query(`SELECT s.client_machine,
			SUM(COALESCE(t.input_tokens, 0) + COALESCE(t.output_tokens, 0) + COALESCE(t.cache_read_tokens, 0) + COALESCE(t.cache_creation_tokens, 0))
		FROM traffic t JOIN sessions s ON s.id = t.session_id
		WHERE t.alias = ? AND t.kind = 'inference' AND t.ts >= ? GROUP BY s.client_machine`, alias, since.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var machine string
		var tokens int64
		if err := rows.Scan(&machine, &tokens); err != nil {
			t.Fatal(err)
		}
		out[machine] = tokens
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fixtureRow is a traffic row of a machine's session, ago before the clock.
type fixtureRow struct {
	machine string
	ago     time.Duration
	kind    string
	usage   *Usage
}

// trafficFixture returns a reader of a store with two machines and the accounts, each
// with the traffic rows and the quota history (readings that many ago, by window).
func trafficFixture(t *testing.T, aliases []string, rows func(alias string) []fixtureRow, history func(alias string) map[string][]time.Duration) (*Reader, *clock) {
	t.Helper()
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
	for _, alias := range aliases {
		schedule(t, store, alias, "pro")
		for _, r := range rows(alias) {
			if err := store.RecordTraffic(ctx, Traffic{At: now.Add(-r.ago), SessionID: sessionOf[r.machine], Alias: alias, Kind: r.kind, Host: "api.example.com", Port: 443, Result: "ok", Status: 200, Usage: r.usage}); err != nil {
				t.Fatal(err)
			}
		}
		for window, agos := range history(alias) {
			for i, ago := range agos {
				if _, err := store.db.Exec("INSERT INTO quota_history (alias, window, at, utilization) VALUES (?, ?, ?, ?)", alias, window, now.Add(-ago).UnixMilli(), 0.1*float64(i+1)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return openReader(t, path, c), c
}

// TestSnapshotMatchesPerAccountQueries checks that the totals and the burn by machine
// the snapshot reads in one scan each equal what the query of each account gives, for
// accounts whose windows start at different times.
func TestSnapshotMatchesPerAccountQueries(t *testing.T) {
	aliases := []string{"sample-a", "sample-b", "sample-c"}
	use := func(in, out int64) *Usage {
		return &Usage{InputTokens: in, OutputTokens: out, CacheRead: 1, CacheCreation: 2}
	}
	// Each account has its own token counts and its own split over the machines, so that
	// a query that mixed accounts would give another result.
	k := map[string]int64{"sample-a": 1, "sample-b": 2, "sample-c": 3}
	rows := func(alias string) []fixtureRow {
		n := k[alias]
		major, minor := "machine-a", "machine-b"
		if n%2 == 0 {
			major, minor = minor, major
		}
		return []fixtureRow{
			{major, 10 * time.Minute, "inference", use(100*n, n)},
			{minor, 50 * time.Minute, "inference", use(10*n, 5)},
			{major, 2 * time.Hour, "inference", nil}, // NULL tokens
			{minor, 2 * time.Hour, "connect", nil},
			{major, 30 * time.Hour, "inference", use(7*n, 7)},
			{minor, 3 * 24 * time.Hour, "inference", use(9*n, 9)},
			{minor, dashboardHours * time.Hour, "inference", use(11*n, 1)},   // exactly at the start of the 24 h window
			{major, dashboardDays * 24 * time.Hour, "inference", use(13, 1)}, // exactly at the start of the 7 d window
			{minor, 8 * 24 * time.Hour, "inference", use(999, 9)},            // outside 7 d
		}
	}
	// Different window starts: sample-a 5h from 90 minutes ago, sample-b 5h from 40
	// minutes ago and 7d from 3 days ago; sample-c has no readings.
	history := func(alias string) map[string][]time.Duration {
		switch alias {
		case "sample-a":
			return map[string][]time.Duration{Window5h: {90 * time.Minute, 60 * time.Minute, 30 * time.Minute}}
		case "sample-b":
			return map[string][]time.Duration{Window5h: {40 * time.Minute, 20 * time.Minute, 0}, Window7d: {72 * time.Hour, 48 * time.Hour, 24 * time.Hour}}
		}
		return nil
	}
	r, c := trafficFixture(t, aliases, rows, history)
	snap, err := r.Snapshot(context.Background(), aliases)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != len(aliases) {
		t.Fatalf("accounts = %+v", snap.Accounts)
	}
	machineWindows := 0
	for _, a := range snap.Accounts {
		if want := oldTotals(t, r, a.Alias, c.Now()); a.Totals != want {
			t.Fatalf("%s totals = %+v, want %+v", a.Alias, a.Totals, want)
		}
		_, windows := r.burn(a.Alias, a.QuotaHistory, a.Quota, c.Now())
		var want []MachineBurn
		for _, w := range windows {
			want = append(want, shares(w.window, w.rate, oldMachineTokens(t, r, a.Alias, w.since))...)
		}
		machineWindows += len(windows)
		if fmt.Sprint(a.Burn.ByMachine) != fmt.Sprint(append([]MachineBurn{}, want...)) {
			t.Fatalf("%s by machine = %+v, want %+v", a.Alias, a.Burn.ByMachine, want)
		}
	}
	if machineWindows != 3 {
		t.Fatalf("%d windows were split over machines, want 3: the fixture does not exercise the scan", machineWindows)
	}
	if got := snap.Accounts[0].Totals; got.Day.Requests != 4 || got.Week.Requests != 7 {
		t.Fatalf("sample-a totals = %+v, want 4 requests in 24 h and 7 in 7 d, the rows at the edges included", got)
	}
}

// trafficStatements returns how many statements that read the traffic table a snapshot of
// n accounts runs.
func trafficStatements(t *testing.T, n int) int {
	t.Helper()
	var aliases []string
	for i := range n {
		aliases = append(aliases, fmt.Sprintf("sample-%02d", i))
	}
	r, _ := trafficFixture(t, aliases, func(string) []fixtureRow {
		return []fixtureRow{{"machine-a", 10 * time.Minute, "inference", &Usage{InputTokens: 1}}, {"machine-b", 20 * time.Minute, "inference", &Usage{InputTokens: 1}}}
	}, func(string) map[string][]time.Duration {
		return map[string][]time.Duration{Window5h: {90 * time.Minute, 60 * time.Minute, 30 * time.Minute}, Window7d: {72 * time.Hour, 48 * time.Hour, 24 * time.Hour}}
	})
	// Read through the recording driver.
	query := url.Values{}
	query.Set("mode", "ro")
	query["_pragma"] = []string{"query_only(1)"}
	var path string
	if err := r.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite-recording", fileURI(path, query))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Reader{store: &Store{db: db, cfg: r.store.cfg}, maxTunnel: r.maxTunnel}
	statements.Lock()
	statements.texts = nil
	statements.Unlock()
	snap, err := recorded.Snapshot(context.Background(), aliases)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != n || len(snap.Accounts[0].Burn.ByMachine) == 0 || snap.Accounts[0].Totals.Week.Requests == 0 {
		t.Fatalf("fixture reads nothing: %+v", snap.Accounts)
	}
	statements.Lock()
	defer statements.Unlock()
	count := 0
	for _, q := range statements.texts {
		if strings.Contains(q, "traffic") {
			count++
		}
	}
	return count
}

// TestSnapshotTrafficStatementsDoNotGrowWithAccounts checks that the number of
// statements a snapshot runs on the traffic table is the same for 1 account and for 10.
func TestSnapshotTrafficStatementsDoNotGrowWithAccounts(t *testing.T) {
	one, ten := trafficStatements(t, 1), trafficStatements(t, 10)
	if one == 0 || one != ten {
		t.Fatalf("traffic statements: %d for 1 account, %d for 10 accounts", one, ten)
	}
}
