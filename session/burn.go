package session

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The least readings and the least span the estimate of a window needs.
const (
	burnMinPoints = 3
	burnMinSpan   = 30 * time.Minute

	// burnMinRate, in utilization per hour, is the rate at which a whole quota lasts a
	// year. A slower one is rounding or noise, not use, and is reported as none.
	burnMinRate = 1.0 / (365 * 24)
)

// Why a window has no rate: the reasons of WindowBurn.
const (
	burnNoReading = "no_reading"       // the window has no reading in its span
	burnFewPoints = "too_few_points"   // fewer than burnMinPoints readings since the last reset
	burnShortSpan = "span_too_short"   // those readings span less than burnMinSpan
	burnNotGiven  = "window_not_given" // the upstream no longer gives the window
	burnResetPast = "reset_passed"     // the window's reset has passed and no reading since
	burnNotRising = "flat"             // the readings since the last reset did not rise
)

// Burn is how fast an account uses its quota windows, as the readings of the quota
// history say.
type Burn struct {
	FiveHour  WindowBurn    `json:"5h"`
	SevenDay  WindowBurn    `json:"7d"`
	ByMachine []MachineBurn `json:"by_machine"` // an estimate, by window and client machine
}

// WindowBurn is the estimate for one window from the readings since its last reset,
// which is a drop in utilization. The rate is the least-squares slope of those
// readings, in utilization per hour (1 is the whole quota). Rate, ExhaustsAt and
// BeforeReset are null, with Reason, when the readings do not allow an estimate; Points
// and SpanMinutes always describe the readings that were used.
type WindowBurn struct {
	RatePerHour *float64   `json:"rate_per_hour"`
	ExhaustsAt  *time.Time `json:"exhausts_at"`  // null at a rate of 0 or none
	ResetsAt    *time.Time `json:"resets_at"`    // null when the latest reading has none
	BeforeReset *bool      `json:"before_reset"` // whether the quota runs out before it resets; null when either time is
	Points      int        `json:"points"`
	SpanMinutes float64    `json:"span_minutes"`
	Reason      string     `json:"reason,omitempty"`
}

// MachineBurn is a client machine's share of an account's rate in one window: the share
// of the tokens the machine's sessions carried on the account since the window's last
// reset, which says nothing of how much quota each token costs.
type MachineBurn struct {
	Window      string  `json:"window"`
	Machine     string  `json:"machine"`
	Share       float64 `json:"share"`
	RatePerHour float64 `json:"rate_per_hour"`
	Estimate    bool    `json:"estimate"` // always true
}

// Totals are an account's requests and tokens over the last 24 hours and 7 days.
type Totals struct {
	Day  WindowTotals `json:"24h"`
	Week WindowTotals `json:"7d"`
}

// WindowTotals counts inference requests, whether or not their responses reported
// usage; a response that did not adds nothing to the tokens.
type WindowTotals struct {
	Requests      int64 `json:"requests"`
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// sinceReset returns the readings from the last drop in utilization on, which are those
// since the window's last reset.
func sinceReset(points []QuotaPoint) []QuotaPoint {
	start := 0
	for i := 1; i < len(points); i++ {
		if points[i].Utilization < points[i-1].Utilization {
			start = i
		}
	}
	return points[start:]
}

// windowGiven reports whether the upstream still gives the window of reading q. It does
// not when q is older than the newest reading of the account, which was observed at or
// after q's reset: the readings of one upstream answer share an observed_at, so the
// answer did not carry this window.
func windowGiven(q Quota, newest time.Time) bool {
	return q.ResetAt.IsZero() || !newest.After(q.ObservedAt) || newest.Before(q.ResetAt)
}

// estimateBurn estimates one window from its quota history, oldest first, and its latest
// reading q (nil when it has none); newest is the latest observed_at over the account's
// windows. It returns the readings it used, for the caller's share of the traffic.
func estimateBurn(history []QuotaPoint, q *Quota, newest, now time.Time) (WindowBurn, []QuotaPoint) {
	points := sinceReset(history)
	out := WindowBurn{Points: len(points)}
	if len(points) > 0 {
		out.SpanMinutes = points[len(points)-1].At.Sub(points[0].At).Minutes()
	}
	if q != nil && !q.ResetAt.IsZero() {
		reset := q.ResetAt
		out.ResetsAt = &reset
	}
	switch {
	case len(history) == 0:
		out.Reason = burnNoReading
	case q != nil && !windowGiven(*q, newest):
		out.Reason = burnNotGiven
	case q != nil && !q.ResetAt.IsZero() && !q.ResetAt.After(now):
		out.Reason = burnResetPast
	case len(points) < burnMinPoints:
		out.Reason = burnFewPoints
	case time.Duration(out.SpanMinutes*float64(time.Minute)) < burnMinSpan:
		out.Reason = burnShortSpan
	}
	if out.Reason != "" {
		return out, nil
	}
	rate := slope(points)
	if rate < burnMinRate {
		rate = 0
	}
	out.RatePerHour = &rate
	if rate == 0 {
		out.Reason = burnNotRising
		return out, points
	}
	last := points[len(points)-1]
	remaining := max(1-last.Utilization, 0)
	exhausts := last.At.Add(time.Duration(remaining / rate * float64(time.Hour)))
	out.ExhaustsAt = &exhausts
	if out.ResetsAt != nil {
		before := exhausts.Before(*out.ResetsAt)
		out.BeforeReset = &before
	}
	return out, points
}

// slope is the least-squares slope of the readings' utilization over their time, per
// hour. The readings must span some time.
func slope(points []QuotaPoint) float64 {
	t0 := points[0].At
	n := float64(len(points))
	var sx, sy float64
	for _, p := range points {
		sx += p.At.Sub(t0).Hours()
		sy += p.Utilization
	}
	mx, my := sx/n, sy/n
	var sxx, sxy float64
	for _, p := range points {
		dx := p.At.Sub(t0).Hours() - mx
		sxx += dx * dx
		sxy += dx * (p.Utilization - my)
	}
	return sxy / sxx
}

// shares splits rate over the machines by their tokens; a machine with none is left out.
func shares(window string, rate float64, tokens map[string]int64) []MachineBurn {
	var total int64
	for _, t := range tokens {
		total += t
	}
	var out []MachineBurn
	for machine, t := range tokens {
		if t <= 0 {
			continue
		}
		share := float64(t) / float64(total)
		out = append(out, MachineBurn{Window: window, Machine: machine, Share: share, RatePerHour: rate * share, Estimate: true})
	}
	slices.SortFunc(out, func(a, b MachineBurn) int {
		return cmp.Or(cmp.Compare(b.Share, a.Share), cmp.Compare(a.Machine, b.Machine))
	})
	return out
}

// machineWindow is a window of an account whose rate is split over the client machines,
// by the tokens carried on the account since the window's last reset.
type machineWindow struct {
	account       int // the index of the account in the snapshot
	alias, window string
	rate          float64
	since         time.Time
}

// burn computes the account's Burn from its quota history and latest readings. Its
// ByMachine is empty: it returns the windows to split, for machineBursts.
func (r *Reader) burn(alias string, history map[string][]QuotaPoint, latest map[string]Quota, now time.Time) (Burn, []machineWindow) {
	var newest time.Time
	for _, q := range latest {
		if q.ObservedAt.After(newest) {
			newest = q.ObservedAt
		}
	}
	out := Burn{ByMachine: []MachineBurn{}}
	var pending []machineWindow
	for _, w := range []struct {
		name string
		dst  *WindowBurn
	}{{Window5h, &out.FiveHour}, {Window7d, &out.SevenDay}} {
		var q *Quota
		if v, ok := latest[w.name]; ok {
			q = &v
		}
		est, used := estimateBurn(history[w.name], q, newest, now)
		*w.dst = est
		if est.RatePerHour == nil || *est.RatePerHour <= 0 {
			continue
		}
		pending = append(pending, machineWindow{alias: alias, window: w.name, rate: *est.RatePerHour, since: used[0].At})
	}
	return out, pending
}

// machineBursts returns the share of the rate by client machine of each window, in the
// order of windows, from one scan of the traffic since the earliest start of the windows.
func (r *Reader) machineBursts(ctx context.Context, windows []machineWindow) ([][]MachineBurn, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	// The windows join the traffic rows, not the other way round, so that the rows are
	// read once: a window's own start is a condition of the join.
	var values []string
	var args []any
	earliest := windows[0].since.UnixMilli()
	for _, w := range windows {
		values = append(values, "(?, ?, ?)")
		args = append(args, w.alias, w.window, w.since.UnixMilli())
		earliest = min(earliest, w.since.UnixMilli())
	}
	args = append(args, earliest)
	rows, err := r.store.db.QueryContext(ctx, `WITH w(alias, window, since) AS (VALUES `+strings.Join(values, ", ")+`)
		SELECT w.alias, w.window, s.client_machine,
			SUM(COALESCE(t.input_tokens, 0) + COALESCE(t.output_tokens, 0) + COALESCE(t.cache_read_tokens, 0) + COALESCE(t.cache_creation_tokens, 0))
		FROM traffic t CROSS JOIN w ON w.alias = t.alias AND t.ts >= w.since
		JOIN sessions s ON s.id = t.session_id
		WHERE t.kind = 'inference' AND t.ts >= ? GROUP BY w.alias, w.window, s.client_machine`, args...)
	if err != nil {
		return nil, fmt.Errorf("read machine tokens: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type key struct{ alias, window string }
	tokens := map[key]map[string]int64{}
	for rows.Next() {
		var k key
		var machine string
		var n int64
		if err := rows.Scan(&k.alias, &k.window, &machine, &n); err != nil {
			return nil, fmt.Errorf("read machine tokens: %v", err)
		}
		if tokens[k] == nil {
			tokens[k] = map[string]int64{}
		}
		tokens[k][machine] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read machine tokens: %v", err)
	}
	out := make([][]MachineBurn, len(windows))
	for i, w := range windows {
		out[i] = shares(w.window, w.rate, tokens[key{w.alias, w.window}])
	}
	return out, nil
}

// totals returns the Totals at now of every account that has inference traffic in the
// last 7 days, by alias, in one scan of that range.
func (r *Reader) totals(ctx context.Context, now time.Time) (map[string]Totals, error) {
	day, week := now.Add(-dashboardHours*time.Hour).UnixMilli(), now.Add(-dashboardDays*24*time.Hour).UnixMilli()
	rows, err := r.store.db.QueryContext(ctx, `SELECT alias,
			COALESCE(SUM(ts >= ?1), 0), COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(input_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(output_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_read_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_creation_tokens, 0) END), 0),
			COUNT(*), COALESCE(SUM(COALESCE(input_tokens, 0)), 0), COALESCE(SUM(COALESCE(output_tokens, 0)), 0),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0)), 0), COALESCE(SUM(COALESCE(cache_creation_tokens, 0)), 0)
		FROM traffic WHERE alias IS NOT NULL AND kind = 'inference' AND ts >= ?2 GROUP BY alias`, day, week)
	if err != nil {
		return nil, fmt.Errorf("read totals: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Totals{}
	for rows.Next() {
		var alias string
		var t Totals
		d, w := &t.Day, &t.Week
		if err := rows.Scan(&alias, &d.Requests, &d.Input, &d.Output, &d.CacheRead, &d.CacheCreation, &w.Requests, &w.Input, &w.Output, &w.CacheRead, &w.CacheCreation); err != nil {
			return nil, fmt.Errorf("read totals: %v", err)
		}
		out[alias] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read totals: %v", err)
	}
	return out, nil
}
