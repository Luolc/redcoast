package session

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"slices"
	"time"
)

// Windows of the dashboard's tables.
const (
	dashboardHours   = 24 // hourly traffic and result counts
	dashboardDays    = 7  // daily counts and CONNECT destinations, by UTC day
	dashboardHistory = 50 // binding history rows
	shortID          = 8  // characters of a session ID the dashboard shows
	idleReference    = 24 * time.Hour
)

// Reader reads the dashboard's snapshot over a connection SQLite opens read-only, with
// query_only on as well, so no statement it runs can write.
type Reader struct {
	store     *Store
	maxTunnel time.Duration
}

// OpenReadOnly opens the existing SQLite file at path for reading. It does not create
// the file or migrate it; the gateway's own Store does both. maxTunnel is the gateway's
// tunnel limit: CONNECT rows that started that long before the daily window are read
// for the tunnel peak, since a tunnel that started before the window can overlap it.
func OpenReadOnly(path string, cfg Config, maxTunnel time.Duration) (*Reader, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "10000")
	query["_pragma"] = []string{"query_only(1)"}
	db, err := sql.Open("sqlite", fileURI(path, query))
	if err != nil {
		return nil, fmt.Errorf("open session store read-only: %v", err)
	}
	return &Reader{store: &Store{db: db, cfg: cfg}, maxTunnel: maxTunnel}, nil
}

// Close closes the connection.
func (r *Reader) Close() error {
	return r.store.Close()
}

// Snapshot is what the dashboard shows of the store at one instant. Session IDs are
// cut to their first characters; credentials and their hashes are never read.
type Snapshot struct {
	At           time.Time      `json:"at"`
	Soft         float64        `json:"soft"` // the quota thresholds selection reads now
	Hard         float64        `json:"hard"`
	DBBytes      int64          `json:"db_bytes"` // the main file's pages; the WAL file is not counted
	Accounts     []AccountView  `json:"accounts"`
	Machines     []MachineView  `json:"machines"`
	Sessions     []LiveSession  `json:"sessions"`
	IdleSessions int            `json:"idle_sessions"` // live sessions without a request for idleReference
	Hourly       []HourTraffic  `json:"hourly"`
	Results      []ResultCount  `json:"results"`
	Destinations []Destination  `json:"destinations"`
	Days         []DayCounts    `json:"days"`
	History      []HistoryEntry `json:"history"`
}

// AccountView is AccountStatus with the account's next plan change.
type AccountView struct {
	AccountStatus
	NextPlan *PlanStatus `json:"next_plan"` // the first period starting after now; null when none
}

// MachineView is a registered client machine with its live sessions and the latest
// request of any of its sessions.
type MachineView struct {
	Machine
	LiveSessions int       `json:"live_sessions"`
	LastSeen     time.Time `json:"last_seen,omitzero"`
}

// LiveSession is one live session.
type LiveSession struct {
	ID       string    `json:"id"` // the first shortID characters
	Machine  string    `json:"client_machine"`
	Alias    string    `json:"alias"` // empty when unbound
	Created  time.Time `json:"created_at"`
	LastSeen time.Time `json:"last_seen"` // the creation time until the first request
}

// HourTraffic is one hour's requests and bytes of one account and kind.
type HourTraffic struct {
	Hour      time.Time `json:"hour"`
	Alias     string    `json:"alias"` // empty for requests carried on no account
	Kind      string    `json:"kind"`
	Requests  int       `json:"requests"`
	BytesUp   int64     `json:"bytes_up"`
	BytesDown int64     `json:"bytes_down"`
}

// ResultCount is how many rows of one kind ended with one result and status.
type ResultCount struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Status int    `json:"status"` // 0 when there was none
	Count  int    `json:"count"`
}

// Destination is one CONNECT target as one account's session reached it on one route.
type Destination struct {
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Alias     string    `json:"alias"`
	Route     string    `json:"route"` // direct or account; empty for an invalid target
	Tunnels   int       `json:"tunnels"`
	OK        int       `json:"ok"` // of them, with result ok
	BytesUp   int64     `json:"bytes_up"`
	BytesDown int64     `json:"bytes_down"`
	Last      time.Time `json:"last"`
}

// DayCounts are the daily figures for one UTC day.
type DayCounts struct {
	Day              time.Time `json:"day"`
	LimitedInference int       `json:"limited_inference"`
	LimitedConnect   int       `json:"limited_connect"`
	TunnelPeak       int       `json:"tunnel_peak"`  // most opened tunnels overlapping at once
	SessionPeak      int       `json:"session_peak"` // most sessions live at once
	Leaked           int       `json:"leaked"`       // sessions ended that day by idle expiry
}

// HistoryEntry is one binding history row.
type HistoryEntry struct {
	Session string    `json:"session"` // the first shortID characters
	Alias   string    `json:"alias"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to,omitzero"`
	Reason  string    `json:"reason"`
}

// Snapshot reads every table of the dashboard for the accounts in aliases.
func (r *Reader) Snapshot(ctx context.Context, aliases []string) (Snapshot, error) {
	status, err := r.store.Status(ctx, aliases)
	if err != nil {
		return Snapshot{}, err
	}
	now := status.At.UnixMilli()
	out := Snapshot{At: status.At, Soft: status.Soft, Hard: status.Hard, Accounts: []AccountView{}, Machines: []MachineView{}, Sessions: []LiveSession{}, Hourly: []HourTraffic{},
		Results: []ResultCount{}, Destinations: []Destination{}, History: []HistoryEntry{}}
	db := r.store.db
	if err := db.QueryRowContext(ctx, "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&out.DBBytes); err != nil {
		return Snapshot{}, fmt.Errorf("database size: %v", err)
	}
	for _, a := range status.Accounts {
		view := AccountView{AccountStatus: a}
		var next PlanStatus
		var from int64
		err := db.QueryRowContext(ctx, "SELECT plan, effective_from FROM plan_schedule WHERE alias = ? AND effective_from > ? ORDER BY effective_from LIMIT 1", a.Alias, now).Scan(&next.Name, &from)
		switch {
		case err == nil:
			next.From = fromMillis(from)
			view.NextPlan = &next
		case err != sql.ErrNoRows:
			return Snapshot{}, fmt.Errorf("read next plan: %v", err)
		}
		out.Accounts = append(out.Accounts, view)
	}
	if out.Machines, err = r.machines(ctx); err != nil {
		return Snapshot{}, err
	}
	err = collect(ctx, db, &out.Sessions, func(rows *sql.Rows) (LiveSession, error) {
		var v LiveSession
		var created, seen int64
		err := rows.Scan(&v.ID, &v.Machine, &v.Alias, &created, &seen)
		v.Created, v.LastSeen = fromMillis(created), fromMillis(seen)
		return v, err
	}, `SELECT substr(s.id, 1, ?), s.client_machine, COALESCE(b.alias, ''), s.created_at, COALESCE(s.last_seen_at, s.created_at)
		FROM sessions s LEFT JOIN bindings b ON b.session_id = s.id WHERE s.ended_at IS NULL ORDER BY s.created_at`, shortID)
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range out.Sessions {
		if s.LastSeen.UnixMilli() <= now-idleReference.Milliseconds() {
			out.IdleSessions++
		}
	}
	since := now - dashboardHours*time.Hour.Milliseconds()
	err = collect(ctx, db, &out.Hourly, func(rows *sql.Rows) (HourTraffic, error) {
		var v HourTraffic
		var hour int64
		err := rows.Scan(&hour, &v.Alias, &v.Kind, &v.Requests, &v.BytesUp, &v.BytesDown)
		v.Hour = fromMillis(hour)
		return v, err
	}, `SELECT ts / 3600000 * 3600000 AS hour, COALESCE(alias, '') AS a, kind, COUNT(*), SUM(bytes_up), SUM(bytes_down)
		FROM traffic WHERE ts >= ? GROUP BY hour, a, kind ORDER BY hour, a, kind`, since)
	if err != nil {
		return Snapshot{}, err
	}
	err = collect(ctx, db, &out.Results, func(rows *sql.Rows) (ResultCount, error) {
		var v ResultCount
		return v, rows.Scan(&v.Kind, &v.Result, &v.Status, &v.Count)
	}, `SELECT kind, result, COALESCE(status, 0) AS st, COUNT(*) AS n FROM traffic WHERE ts >= ?
		GROUP BY kind, result, st ORDER BY kind, n DESC, result, st`, since)
	if err != nil {
		return Snapshot{}, err
	}
	dayStart := status.At.Truncate(24*time.Hour).AddDate(0, 0, 1-dashboardDays).UnixMilli()
	err = collect(ctx, db, &out.Destinations, func(rows *sql.Rows) (Destination, error) {
		var v Destination
		var last int64
		err := rows.Scan(&v.Host, &v.Port, &v.Alias, &v.Route, &v.Tunnels, &v.OK, &v.BytesUp, &v.BytesDown, &last)
		v.Last = fromMillis(last)
		return v, err
	}, `SELECT host, port, COALESCE(alias, '') AS a, COALESCE(route, '') AS rt, COUNT(*) AS n, SUM(result = 'ok'), SUM(bytes_up), SUM(bytes_down), MAX(ts)
		FROM traffic WHERE kind = 'connect' AND ts >= ? GROUP BY host, port, a, rt ORDER BY n DESC, host, port, a, rt`, dayStart)
	if err != nil {
		return Snapshot{}, err
	}
	if out.Days, err = r.days(ctx, dayStart, now); err != nil {
		return Snapshot{}, err
	}
	err = collect(ctx, db, &out.History, func(rows *sql.Rows) (HistoryEntry, error) {
		var v HistoryEntry
		var from int64
		var to sql.NullInt64
		err := rows.Scan(&v.Session, &v.Alias, &from, &to, &v.Reason)
		v.From = fromMillis(from)
		if to.Valid {
			v.To = fromMillis(to.Int64)
		}
		return v, err
	}, "SELECT substr(session_id, 1, ?), alias, from_ts, to_ts, reason FROM binding_history ORDER BY id DESC LIMIT ?", shortID, dashboardHistory)
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// machines returns the registered machines with their live sessions and latest request.
func (r *Reader) machines(ctx context.Context) ([]MachineView, error) {
	machines, err := r.store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MachineView, 0, len(machines))
	for _, m := range machines {
		v := MachineView{Machine: m}
		var seen sql.NullInt64
		if err := r.store.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(ended_at IS NULL), 0), MAX(last_seen_at) FROM sessions WHERE client_machine = ?", m.Name).Scan(&v.LiveSessions, &seen); err != nil {
			return nil, fmt.Errorf("read machine sessions: %v", err)
		}
		if seen.Valid {
			v.LastSeen = fromMillis(seen.Int64)
		}
		out = append(out, v)
	}
	return out, nil
}

// days returns the daily figures of the UTC days from start, a midnight, to now's day.
// Rows stamped after now, as after the clock stepped back, wait until now reaches them.
func (r *Reader) days(ctx context.Context, start, now int64) ([]DayCounts, error) {
	const day = 24 * 60 * 60 * 1000
	out := make([]DayCounts, 0, dashboardDays)
	for d := start; d <= now; d += day {
		out = append(out, DayCounts{Day: fromMillis(d)})
	}
	at := func(ts int64) *DayCounts { return &out[(ts-start)/day] }
	db := r.store.db
	var limited [][2]int64
	if err := collect(ctx, db, &limited, scanPair, "SELECT ts, kind = 'connect' FROM traffic WHERE ts BETWEEN ? AND ? AND result = 'limited'", start, now); err != nil {
		return nil, err
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
		return nil, err
	}
	for _, ts := range ended {
		at(ts).Leaked++
	}
	var tunnels, sessions [][2]int64
	if err := collect(ctx, db, &tunnels, scanPair, "SELECT ts, ts + duration_ms FROM traffic WHERE kind = 'connect' AND result IN ('ok', 'idle_timeout', 'max_duration') AND ts >= ? AND ts + duration_ms > ?", start-r.maxTunnel.Milliseconds(), start); err != nil {
		return nil, err
	}
	if err := collect(ctx, db, &sessions, scanPair, "SELECT created_at, COALESCE(ended_at, ?) FROM sessions WHERE COALESCE(ended_at, ?) >= ?", now, now, start); err != nil {
		return nil, err
	}
	for i := range out {
		from := start + int64(i)*day
		out[i].TunnelPeak = peak(tunnels, from, from+day)
		out[i].SessionPeak = peak(sessions, from, from+day)
	}
	return out, nil
}

// peak returns the most half-open intervals [start, end) overlapping at one instant
// within [from, to). An empty interval, such as a session created and read at the
// same millisecond, counts at its instant.
func peak(intervals [][2]int64, from, to int64) int {
	type event struct {
		at    int64
		delta int
	}
	var events []event
	for _, iv := range intervals {
		start, end := max(iv[0], from), min(max(iv[1], iv[0]+1), to)
		if start < end {
			events = append(events, event{start, 1}, event{end, -1})
		}
	}
	// At the same instant ends come first: [a, b) and [b, c) never overlap.
	slices.SortFunc(events, func(a, b event) int {
		return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.delta, b.delta))
	})
	best, open := 0, 0
	for _, e := range events {
		open += e.delta
		best = max(best, open)
	}
	return best
}

// collect appends to out one value scanned from each row of query.
func collect[T any](ctx context.Context, db *sql.DB, out *[]T, scan func(*sql.Rows) (T, error), query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("dashboard query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return fmt.Errorf("dashboard query: %v", err)
		}
		*out = append(*out, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dashboard query: %v", err)
	}
	return nil
}

func scanOne(rows *sql.Rows) (int64, error) {
	var v int64
	return v, rows.Scan(&v)
}

func scanPair(rows *sql.Rows) ([2]int64, error) {
	var v [2]int64
	return v, rows.Scan(&v[0], &v[1])
}
