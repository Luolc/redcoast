package session

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// The least readings and the least span the estimate of a window needs.
const (
	burnMinPoints = 3
	burnMinSpan   = 30 * time.Minute
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
	out.RatePerHour = &rate
	if rate <= 0 {
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
	var sx, sy, sxx, sxy float64
	for _, p := range points {
		x, y := p.At.Sub(t0).Hours(), p.Utilization
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	return (n*sxy - sx*sy) / (n*sxx - sx*sx)
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

// burn computes the account's Burn from its quota history and latest readings.
func (r *Reader) burn(ctx context.Context, alias string, history map[string][]QuotaPoint, latest map[string]Quota, now time.Time) (Burn, error) {
	var newest time.Time
	for _, q := range latest {
		if q.ObservedAt.After(newest) {
			newest = q.ObservedAt
		}
	}
	out := Burn{ByMachine: []MachineBurn{}}
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
		tokens, err := r.machineTokens(ctx, alias, used[0].At)
		if err != nil {
			return Burn{}, err
		}
		out.ByMachine = append(out.ByMachine, shares(w.name, *est.RatePerHour, tokens)...)
	}
	return out, nil
}

// machineTokens returns the tokens of the inference requests carried on alias since
// since, by the client machine of their sessions; requests of no known session are left out.
func (r *Reader) machineTokens(ctx context.Context, alias string, since time.Time) (map[string]int64, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT s.client_machine,
			SUM(COALESCE(t.input_tokens, 0) + COALESCE(t.output_tokens, 0) + COALESCE(t.cache_read_tokens, 0) + COALESCE(t.cache_creation_tokens, 0))
		FROM traffic t JOIN sessions s ON s.id = t.session_id
		WHERE t.alias = ? AND t.kind = 'inference' AND t.ts >= ? GROUP BY s.client_machine`, alias, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read machine tokens: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var machine string
		var tokens int64
		if err := rows.Scan(&machine, &tokens); err != nil {
			return nil, fmt.Errorf("read machine tokens: %v", err)
		}
		out[machine] = tokens
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read machine tokens: %v", err)
	}
	return out, nil
}

// totals returns the account's Totals at now.
func (r *Reader) totals(ctx context.Context, alias string, now time.Time) (Totals, error) {
	day, week := now.Add(-dashboardHours*time.Hour).UnixMilli(), now.Add(-dashboardDays*24*time.Hour).UnixMilli()
	var out Totals
	d, w := &out.Day, &out.Week
	err := r.store.db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(ts >= ?1), 0), COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(input_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(output_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_read_tokens, 0) END), 0),
			COALESCE(SUM(CASE WHEN ts >= ?1 THEN COALESCE(cache_creation_tokens, 0) END), 0),
			COUNT(*), COALESCE(SUM(COALESCE(input_tokens, 0)), 0), COALESCE(SUM(COALESCE(output_tokens, 0)), 0),
			COALESCE(SUM(COALESCE(cache_read_tokens, 0)), 0), COALESCE(SUM(COALESCE(cache_creation_tokens, 0)), 0)
		FROM traffic WHERE alias = ?2 AND kind = 'inference' AND ts >= ?3`, day, alias, week).
		Scan(&d.Requests, &d.Input, &d.Output, &d.CacheRead, &d.CacheCreation, &w.Requests, &w.Input, &w.Output, &w.CacheRead, &w.CacheCreation)
	if err != nil && err != sql.ErrNoRows {
		return Totals{}, fmt.Errorf("read totals: %v", err)
	}
	return out, nil
}
