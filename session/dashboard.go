package session

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
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

// sessionBatch is how many session IDs one lookup of the sessions table names.
const sessionBatch = 500

// failed reports whether a traffic row counts as an error: a result other than ok, or a
// status of 400 or more (an inference request the upstream answered with an error
// status is recorded with result ok); status is 0 when the row has none. A CONNECT
// tunnel that the gateway's limits closed (idle_timeout, max_duration) was open and
// carried traffic, so it is no error; an inference stream they cut short is one.
func failed(kind, result string, status int) bool {
	closedByLimit := kind == "connect" && (result == "idle_timeout" || result == "max_duration")
	return result != "ok" && !closedByLimit || status >= 400
}

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
	IdleSessions int            `json:"idle_sessions"`  // live sessions without a request for idleReference
	Usage        []UsagePoint   `json:"usage"`          // the hourly figures by account and client machine
	ModelUsage   []ModelUsage   `json:"model_usage"`    // the hourly tokens by account and model
	SourceCounts []SourceCount  `json:"result_sources"` // results by account and client machine
	Clients      []ClientCount  `json:"clients"`
	ClientHourly []ClientHour   `json:"client_hourly"`
	Destinations []Destination  `json:"destinations"`
	Days         []DayCounts    `json:"days"`
	History      []HistoryEntry `json:"history"`
	Latency      LatencyView    `json:"latency"`
}

// AccountView is AccountStatus with the account's next plan change and its quota
// history over the length of each window.
type AccountView struct {
	AccountStatus
	NextPlan     *PlanStatus             `json:"next_plan"`     // the first period starting after now; null when none
	QuotaHistory map[string][]QuotaPoint `json:"quota_history"` // by window, oldest first
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

	// Client is the client program and ClientVersion its version, each empty when the
	// launcher did not report a valid one.
	Client        string `json:"client"`
	ClientVersion string `json:"client_version"`
}

// ResultCount is how many rows of one kind ended with one result and status.
type ResultCount struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Status int    `json:"status"` // 0 when there was none
	Count  int    `json:"count"`
}

// UsagePoint is one hour's requests, bytes, errors and tokens of one account, client
// machine and kind. Errors are the rows the failed expression matches. Limited, among
// them, are the rows with result limited: requests the gateway itself turned away
// because its binding queue, the account's slots or the forward proxy's slots were
// full, not upstream 429s, which are recorded with result ok and status 429. The tokens
// are the sums of what the responses reported (see Usage), errors included: a stream
// cut short still used the tokens it reported.
type UsagePoint struct {
	Hour      time.Time `json:"hour"`
	Alias     string    `json:"alias"`          // empty for requests carried on no account
	Machine   string    `json:"client_machine"` // empty for requests of no session
	Kind      string    `json:"kind"`
	Requests  int       `json:"requests"`
	BytesUp   int64     `json:"bytes_up"`
	BytesDown int64     `json:"bytes_down"`
	Errors    int       `json:"errors"`
	Limited   int       `json:"limited"`
	Tokens
}

// Tokens are sums of reported token counts.
type Tokens struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
}

// ModelUsage is one hour's tokens of one account and model, over the requests whose
// response reported usage.
type ModelUsage struct {
	Hour     time.Time `json:"hour"`
	Alias    string    `json:"alias"` // empty for requests carried on no account
	Model    string    `json:"model"` // empty when the response named no model
	Requests int       `json:"requests"`
	Tokens
}

// SourceCount is a ResultCount of one account and client machine, with whether that
// result and status count as an error.
type SourceCount struct {
	ResultCount
	Alias   string `json:"alias"`          // empty for requests carried on no account
	Machine string `json:"client_machine"` // empty for requests of no session
	Error   bool   `json:"error"`
}

// Latency is the spread of the durations of the inference requests in one group that
// are no error (see failed), in milliseconds. A duration runs from the request's
// arrival until its response finished streaming, so it is the whole request, not the
// time to its first byte. CONNECT tunnels are left out: a tunnel's duration is how long
// it stayed open. Percentiles are nearest-rank: the smallest duration with at least
// that share of the group at or below it.
type Latency struct {
	Requests int   `json:"requests"`
	P50      int64 `json:"duration_p50_ms"`
	P95      int64 `json:"duration_p95_ms"`
}

// LatencyView is the latency of the last dashboardHours hours by hour, by account and
// by client machine. A group without a request has no row.
type LatencyView struct {
	Hourly   []HourLatency    `json:"hourly"`
	Accounts []AccountLatency `json:"accounts"`
	Machines []MachineLatency `json:"machines"`
}

// HourLatency is the latency of one hour's requests, of every account and machine.
type HourLatency struct {
	Hour time.Time `json:"hour"`
	Latency
}

// AccountLatency is the latency of one account's requests over the whole window.
type AccountLatency struct {
	Alias string `json:"alias"`
	Latency
}

// MachineLatency is the latency of one client machine's requests over the whole window.
type MachineLatency struct {
	Machine string `json:"client_machine"`
	Latency
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
	out := Snapshot{At: status.At, Soft: status.Soft, Hard: status.Hard, Accounts: []AccountView{}, Machines: []MachineView{}, Sessions: []LiveSession{},
		Usage: []UsagePoint{}, ModelUsage: []ModelUsage{}, SourceCounts: []SourceCount{}, Destinations: []Destination{}, History: []HistoryEntry{}}
	db := r.store.db
	if err := db.QueryRowContext(ctx, "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&out.DBBytes); err != nil {
		return Snapshot{}, fmt.Errorf("database size: %v", err)
	}
	for _, a := range status.Accounts {
		view, err := r.account(ctx, a, now)
		if err != nil {
			return Snapshot{}, err
		}
		out.Accounts = append(out.Accounts, view)
	}
	if out.Machines, err = r.machines(ctx); err != nil {
		return Snapshot{}, err
	}
	err = collect(ctx, db, &out.Sessions, func(rows *sql.Rows) (LiveSession, error) {
		var v LiveSession
		var created, seen int64
		err := rows.Scan(&v.ID, &v.Machine, &v.Alias, &created, &seen, &v.Client, &v.ClientVersion)
		v.Created, v.LastSeen = fromMillis(created), fromMillis(seen)
		v.ClientVersion = clientVersion(v.Client, v.ClientVersion)
		return v, err
	}, `SELECT substr(s.id, 1, ?), s.client_machine, COALESCE(b.alias, ''), s.created_at, COALESCE(s.last_seen_at, s.created_at), `+clientColumn+`, `+clientVersionColumn+`
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
	dayStart := status.At.Truncate(24*time.Hour).AddDate(0, 0, 1-dashboardDays).UnixMilli()
	connects, err := r.scan(ctx, since, dayStart, now, &out)
	if err != nil {
		return Snapshot{}, err
	}
	out.Destinations = connects.destinations
	if out.Clients, err = r.clients(ctx, dayStart); err != nil {
		return Snapshot{}, err
	}
	if out.Days, err = r.days(ctx, dayStart, now, connects); err != nil {
		return Snapshot{}, err
	}
	if out.History, err = r.history(ctx); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// history returns the latest dashboardHistory rows of the binding history.
func (r *Reader) history(ctx context.Context) ([]HistoryEntry, error) {
	history := []HistoryEntry{}
	err := collect(ctx, r.store.db, &history, func(rows *sql.Rows) (HistoryEntry, error) {
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
	return history, err
}

// scan runs window and connects, which take most of a snapshot's time, at once, each
// on a connection of its own. When one fails, the other is canceled, and scan returns
// once both have stopped.
func (r *Reader) scan(ctx context.Context, since, dayStart, now int64, out *Snapshot) (connectWindow, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var connects connectWindow
	var connectsErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		if connects, connectsErr = r.connects(ctx, dayStart, now); connectsErr != nil {
			cancel()
		}
	})
	windowErr := r.window(ctx, since, out)
	if windowErr != nil {
		cancel()
	}
	wg.Wait()
	return connects, errors.Join(windowErr, connectsErr)
}

// windowRow is what the hourly tables read of one traffic row.
type windowRow struct {
	hour                         int64
	session                      sql.NullString
	alias, kind, result, model   string // alias and model empty when NULL
	status                       int    // 0 when NULL
	reported                     bool   // the response reported usage: input_tokens is not NULL
	bytesUp, bytesDown, duration int64
	Tokens                       // each 0 when NULL
}

// sessionSource is what a traffic row is grouped by of its session: the client machine
// and the client program as clientColumn gives it.
type sessionSource struct {
	machine, client string
}

// window reads the traffic rows since since once and groups them into the snapshot's
// hourly traffic, results, usage, model usage, result sources, client hours and latency.
// A row's client machine is its session's, which is read once per session rather than
// joined to each row. Prune keeps every session a traffic row names, so only a row of
// no session has no machine, and is left out of the client hours.
func (r *Reader) window(ctx context.Context, since int64, out *Snapshot) error {
	var rows []windowRow
	err := collect(ctx, r.store.db, &rows, func(rows *sql.Rows) (windowRow, error) {
		var v windowRow
		err := rows.Scan(&v.hour, &v.session, &v.alias, &v.kind, &v.result, &v.status, &v.model, &v.reported, &v.bytesUp, &v.bytesDown, &v.duration,
			&v.InputTokens, &v.OutputTokens, &v.CacheRead, &v.CacheCreation)
		return v, err
	}, `SELECT ts / 3600000 * 3600000, session_id, COALESCE(alias, ''), kind, result, COALESCE(status, 0), COALESCE(model, ''), input_tokens IS NOT NULL,
		bytes_up, bytes_down, duration_ms, COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0)
		FROM traffic WHERE ts >= ?`, since)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, v := range rows {
		if v.session.Valid {
			ids[v.session.String] = true
		}
	}
	sessions, err := r.sessionSources(ctx, slices.Collect(maps.Keys(ids)))
	if err != nil {
		return err
	}

	type resultKey struct {
		kind, result string
		status       int
	}
	type usageKey struct {
		hour                 int64
		alias, machine, kind string
	}
	type modelKey struct {
		hour         int64
		alias, model string
	}
	type sourceKey struct {
		alias, machine string
		resultKey
	}
	type clientKey struct {
		hour            int64
		machine, client string
	}
	usage, models := map[usageKey]*UsagePoint{}, map[modelKey]*ModelUsage{}
	sources, clients := map[sourceKey]*SourceCount{}, map[clientKey]*ClientHour{}
	hours, accounts, machines := map[int64][]int64{}, map[string][]int64{}, map[string][]int64{}
	for _, v := range rows {
		var source sessionSource
		joined := false
		if v.session.Valid {
			source, joined = sessions[v.session.String]
		}
		isError := failed(v.kind, v.result, v.status)
		limited := 0
		if v.result == "limited" {
			limited = 1
		}

		result := resultKey{v.kind, v.result, v.status}

		u := entry(usage, usageKey{v.hour, v.alias, source.machine, v.kind}, func(k usageKey) UsagePoint {
			return UsagePoint{Hour: fromMillis(k.hour), Alias: k.alias, Machine: k.machine, Kind: k.kind}
		})
		u.Requests++
		u.BytesUp += v.bytesUp
		u.BytesDown += v.bytesDown
		if isError {
			u.Errors++
		}
		u.Limited += limited
		u.Tokens = u.add(v.Tokens)

		if v.reported {
			m := entry(models, modelKey{v.hour, v.alias, v.model}, func(k modelKey) ModelUsage {
				return ModelUsage{Hour: fromMillis(k.hour), Alias: k.alias, Model: k.model}
			})
			m.Requests++
			m.Tokens = m.add(v.Tokens)
		}

		entry(sources, sourceKey{v.alias, source.machine, result}, func(k sourceKey) SourceCount {
			return SourceCount{ResultCount: ResultCount{Kind: k.kind, Result: k.result, Status: k.status}, Alias: k.alias, Machine: k.machine,
				Error: failed(k.kind, k.result, k.status)}
		}).Count++

		if joined {
			entry(clients, clientKey{v.hour, source.machine, source.client}, func(k clientKey) ClientHour {
				return ClientHour{Hour: fromMillis(k.hour), Machine: k.machine, Client: k.client}
			}).Requests++
		}

		if v.kind == "inference" && !isError {
			hours[v.hour] = append(hours[v.hour], v.duration)
			accounts[v.alias] = append(accounts[v.alias], v.duration)
			machines[source.machine] = append(machines[source.machine], v.duration)
		}
	}

	out.Usage = sortedValues(usage, func(a, b *UsagePoint) int {
		return cmp.Or(a.Hour.Compare(b.Hour), cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Machine, b.Machine), cmp.Compare(a.Kind, b.Kind))
	})
	out.ModelUsage = sortedValues(models, func(a, b *ModelUsage) int {
		return cmp.Or(a.Hour.Compare(b.Hour), cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Model, b.Model))
	})
	out.SourceCounts = sortedValues(sources, func(a, b *SourceCount) int {
		return cmp.Or(cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Machine, b.Machine), cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(b.Count, a.Count), cmp.Compare(a.Result, b.Result), cmp.Compare(a.Status, b.Status))
	})
	out.ClientHourly = sortedValues(clients, func(a, b *ClientHour) int {
		return cmp.Or(a.Hour.Compare(b.Hour), cmp.Compare(a.Machine, b.Machine), cmp.Compare(a.Client, b.Client))
	})
	out.Latency = latency(hours, accounts, machines)
	return nil
}

// add returns the sums of t and u.
func (t Tokens) add(u Tokens) Tokens {
	return Tokens{t.InputTokens + u.InputTokens, t.OutputTokens + u.OutputTokens, t.CacheRead + u.CacheRead, t.CacheCreation + u.CacheCreation}
}

// sessionSources returns the sessionSource of each session in ids that exists, reading
// sessionBatch sessions per query.
func (r *Reader) sessionSources(ctx context.Context, ids []string) (map[string]sessionSource, error) {
	type row struct {
		id string
		sessionSource
	}
	out := make(map[string]sessionSource, len(ids))
	for batch := range slices.Chunk(ids, sessionBatch) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		var rows []row
		err := collect(ctx, r.store.db, &rows, func(rows *sql.Rows) (row, error) {
			var v row
			return v, rows.Scan(&v.id, &v.machine, &v.client)
		}, `SELECT s.id, s.client_machine, `+clientColumn+` FROM sessions s WHERE s.id IN (?`+strings.Repeat(", ?", len(batch)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, v := range rows {
			out[v.id] = v.sessionSource
		}
	}
	return out, nil
}

// entry returns the value of groups for key, adding the one first returns when there
// is none.
func entry[K comparable, V any](groups map[K]*V, key K, first func(K) V) *V {
	v, ok := groups[key]
	if !ok {
		group := first(key)
		v = &group
		groups[key] = v
	}
	return v
}

// sortedValues returns the values of groups in the order of compare, an empty slice
// when there are none.
func sortedValues[K comparable, V any](groups map[K]*V, compare func(a, b *V) int) []V {
	out := make([]V, 0, len(groups))
	for _, v := range slices.SortedFunc(maps.Values(groups), compare) {
		out = append(out, *v)
	}
	return out
}

// latency returns the latency of the durations grouped by hour, account and client
// machine. SQLite has no percentile function, so the window's durations are read into
// memory, one per request, and sorted.
func latency(hours map[int64][]int64, accounts, machines map[string][]int64) LatencyView {
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

// connectWindow is what the daily tables read of the CONNECT and limited rows.
type connectWindow struct {
	destinations []Destination
	tunnels      [][2]int64 // the start and end of each tunnel that opened and can overlap the daily window
	limited      [][2]int64 // the ts of each limited row in the daily window, and 1 for a CONNECT row
}

// connects reads once the CONNECT rows from the tunnel limit before start, a midnight,
// and the limited rows of either kind, and returns the CONNECT destinations since start,
// the opened tunnels that end after start, which include those that started before it,
// and the limited rows from start to now.
func (r *Reader) connects(ctx context.Context, start, now int64) (connectWindow, error) {
	type destinationKey struct {
		host         string
		port         int
		alias, route string
	}
	destinations := map[destinationKey]*Destination{}
	var out connectWindow
	tunnelsFrom := start - r.maxTunnel.Milliseconds()
	err := each(ctx, r.store.db, func(rows *sql.Rows) error {
		var ts, bytesUp, bytesDown, duration int64
		var connect bool
		var key destinationKey
		var result string
		if err := rows.Scan(&ts, &connect, &key.host, &key.port, &key.alias, &key.route, &result, &bytesUp, &bytesDown, &duration); err != nil {
			return err
		}
		if connect && ts >= start {
			d := entry(destinations, key, func(k destinationKey) Destination {
				return Destination{Host: k.host, Port: k.port, Alias: k.alias, Route: k.route}
			})
			d.add(ts, result, bytesUp, bytesDown)
		}
		opened := result == "ok" || result == "idle_timeout" || result == "max_duration"
		if connect && opened && ts >= tunnelsFrom && ts+duration > start {
			out.tunnels = append(out.tunnels, [2]int64{ts, ts + duration})
		}
		if result == "limited" && ts >= start && ts <= now {
			kind := int64(0)
			if connect {
				kind = 1
			}
			out.limited = append(out.limited, [2]int64{ts, kind})
		}
		return nil
	}, `SELECT ts, kind = 'connect', host, port, COALESCE(alias, ''), COALESCE(route, ''), result, bytes_up, bytes_down, duration_ms
		FROM traffic WHERE ts >= ? AND (kind = 'connect' OR result = 'limited')`, min(start, tunnelsFrom))
	if err != nil {
		return connectWindow{}, err
	}
	out.destinations = sortedValues(destinations, func(a, b *Destination) int {
		return cmp.Or(cmp.Compare(b.Tunnels, a.Tunnels), cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port),
			cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Route, b.Route))
	})
	return out, nil
}

// add counts a tunnel to the destination that started at ts and ended with result.
func (d *Destination) add(ts int64, result string, bytesUp, bytesDown int64) {
	d.Tunnels++
	if result == "ok" {
		d.OK++
	}
	d.BytesUp += bytesUp
	d.BytesDown += bytesDown
	if last := fromMillis(ts); last.After(d.Last) {
		d.Last = last
	}
}

// spread returns the count and the nearest-rank 50th and 95th percentiles of the
// durations, which it sorts in place. There is at least one duration.
func spread(durations []int64) Latency {
	slices.Sort(durations)
	return Latency{Requests: len(durations), P50: percentile(durations, 50), P95: percentile(durations, 95)}
}

// percentile returns the nearest-rank pth percentile of sorted, which is not empty: the
// element at rank ceil(p/100 * n), counting from 1.
func percentile(sorted []int64, p int) int64 {
	return sorted[(p*len(sorted)+99)/100-1]
}

// account returns a's view: its next plan change after now and its quota history.
func (r *Reader) account(ctx context.Context, a AccountStatus, now int64) (AccountView, error) {
	view := AccountView{AccountStatus: a}
	var err error
	if view.QuotaHistory, err = r.store.QuotaHistory(ctx, a.Alias); err != nil {
		return AccountView{}, err
	}
	var next PlanStatus
	var from int64
	err = r.store.db.QueryRowContext(ctx, "SELECT plan, effective_from FROM plan_schedule WHERE alias = ? AND effective_from > ? ORDER BY effective_from LIMIT 1", a.Alias, now).Scan(&next.Name, &from)
	switch {
	case err == nil:
		next.From = fromMillis(from)
		view.NextPlan = &next
	case err != sql.ErrNoRows:
		return AccountView{}, fmt.Errorf("read next plan: %v", err)
	}
	return view, nil
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
// The limited rows and the tunnels are those connects read.
func (r *Reader) days(ctx context.Context, start, now int64, connects connectWindow) ([]DayCounts, error) {
	const day = 24 * 60 * 60 * 1000
	out := make([]DayCounts, 0, dashboardDays)
	for d := start; d <= now; d += day {
		out = append(out, DayCounts{Day: fromMillis(d)})
	}
	at := func(ts int64) *DayCounts { return &out[(ts-start)/day] }
	db := r.store.db
	for _, l := range connects.limited {
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
	var sessions [][2]int64
	if err := collect(ctx, db, &sessions, scanPair, "SELECT created_at, COALESCE(ended_at, ?) FROM sessions WHERE COALESCE(ended_at, ?) >= ?", now, now, start); err != nil {
		return nil, err
	}
	for i := range out {
		from := start + int64(i)*day
		out[i].TunnelPeak = peak(connects.tunnels, from, from+day)
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
	return each(ctx, db, func(rows *sql.Rows) error {
		v, err := scan(rows)
		*out = append(*out, v)
		return err
	}, query, args...)
}

// each calls use on each row of query, stopping at the first error.
func each(ctx context.Context, db *sql.DB, use func(*sql.Rows) error, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("dashboard query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := use(rows); err != nil {
			return fmt.Errorf("dashboard query: %v", err)
		}
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
