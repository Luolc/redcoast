package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Account tiers by quota: under the soft threshold, at or over soft but under hard, and
// at or over hard (or refused by the upstream). Selection prefers the lowest tier and
// never picks an exhausted account.
const (
	tierOpen      = 0
	tierSoft      = 1
	tierExhausted = 2
)

// Quota readings without a reset time, and accounts without a reading, are scored as
// if the window reset in a full week. The remaining time never goes below a minute so
// that an imminent reset does not divide by nearly zero.
const (
	fullWindow   = 7 * 24 * time.Hour
	minRemaining = time.Minute
)

// QuotaExhausted is returned by Bind when every candidate is unavailable for a quota
// reason: a reading at or over the hard threshold, or a pause after an upstream 429.
// ResetAt is the earliest reset or pause end among them; zero when none is known.
type QuotaExhausted struct {
	ResetAt time.Time
}

func (e *QuotaExhausted) Error() string { return "every account is over its hard quota threshold" }

// excluded describes the candidates Bind set aside before scoring: the ends of the
// quota pauses (upstream 429), and whether any was set aside for another reason (a
// pause after a 401, or no plan at this time). The other kind decides between the
// local 429 and the 503 when nothing is left to choose.
type excluded struct {
	quotaPaused map[string]int64 // pause end by alias
	quotaResets []int64          // pause ends of the quota-paused accounts that have a plan
	other       bool
}

// standing is one candidate's quota tier and score at the time of a request.
type standing struct {
	alias   string
	tier    int
	score   float64 // (1 − 7d utilization) × quota unit ÷ time to the 7d reset
	resetAt int64   // earliest reset of a window at or over hard; 0 when unknown
}

// standings computes the tier and score of every eligible account at now. An account
// with no plan or no quota unit at now is left out: it is not a candidate, and blocked
// notes that an account is out for a reason other than quota.
func standings(ctx context.Context, tx *sql.Tx, eligible map[string]bool, now int64, blocked *excluded) (map[string]standing, error) {
	soft, hard, err := thresholds(ctx, tx)
	if err != nil {
		return nil, err
	}
	// A quota-paused account that also has no plan is out for both reasons; the
	// non-quota one decides.
	for alias, until := range blocked.quotaPaused {
		_, err := quotaUnitAt(ctx, tx, alias, now)
		switch {
		case errors.Is(err, ErrNoPlan):
			blocked.other = true
		case err != nil:
			return nil, err
		default:
			blocked.quotaResets = append(blocked.quotaResets, until)
		}
	}
	out := make(map[string]standing, len(eligible))
	for alias := range eligible {
		unit, err := quotaUnitAt(ctx, tx, alias, now)
		if errors.Is(err, ErrNoPlan) {
			blocked.other = true
			continue
		}
		if err != nil {
			return nil, err
		}
		st, err := accountStanding(ctx, tx, alias, now, soft, hard)
		if err != nil {
			return nil, err
		}
		st.score *= unit
		out[alias] = st
	}
	return out, nil
}

// accountStanding reads alias's latest readings and applies the thresholds. A reading
// whose reset has passed is void: the window counts as unread. Either window at or
// over a threshold puts the account in that tier; a rejected status counts as hard.
func accountStanding(ctx context.Context, tx *sql.Tx, alias string, now int64, soft, hard float64) (standing, error) {
	rows, err := tx.QueryContext(ctx, "SELECT window, utilization, COALESCE(status, ''), reset_at FROM quota_latest WHERE alias = ?", alias)
	if err != nil {
		return standing{}, fmt.Errorf("read quota: %v", err)
	}
	defer func() { _ = rows.Close() }()
	st := standing{alias: alias}
	utilization7d, remaining := 0.0, fullWindow
	for rows.Next() {
		var window, status string
		var utilization float64
		var reset sql.NullInt64
		if err := rows.Scan(&window, &utilization, &status, &reset); err != nil {
			return standing{}, fmt.Errorf("read quota: %v", err)
		}
		if reset.Valid && reset.Int64 <= now {
			continue
		}
		tier := windowTier(utilization, status, soft, hard)
		if tier == tierExhausted && reset.Valid && (st.resetAt == 0 || reset.Int64 < st.resetAt) {
			st.resetAt = reset.Int64
		}
		st.tier = max(st.tier, tier)
		if window == Window7d {
			utilization7d = utilization
			if reset.Valid {
				remaining = max(time.Duration(reset.Int64-now)*time.Millisecond, minRemaining)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return standing{}, fmt.Errorf("read quota: %v", err)
	}
	st.score = (1 - utilization7d) / remaining.Hours()
	return st, nil
}

// windowTier is one window's tier: exhausted at or over hard or when rejected, soft at
// or over soft, open otherwise.
func windowTier(utilization float64, status string, soft, hard float64) int {
	switch {
	case utilization >= hard || status == "rejected":
		return tierExhausted
	case utilization >= soft:
		return tierSoft
	}
	return tierOpen
}

// choose picks the account for session id: in the lowest tier present, the highest
// score, ties broken by fewer active sessions other than id, then by alias. When
// nothing is left to choose it returns QuotaExhausted if every candidate is out for a
// quota reason and at least one is, and ErrNoAccount otherwise: a candidate out for
// another reason (401, no plan) is a fault to notice, not a limit to wait out.
func choose(ctx context.Context, tx *sql.Tx, id string, standings map[string]standing, blocked excluded, now int64) (string, error) {
	lowest := tierExhausted
	for _, st := range standings {
		lowest = min(lowest, st.tier)
	}
	if lowest == tierExhausted {
		if blocked.other || len(standings)+len(blocked.quotaResets) == 0 {
			return "", ErrNoAccount
		}
		exhausted := &QuotaExhausted{}
		resets := blocked.quotaResets
		for _, st := range standings {
			resets = append(resets, st.resetAt)
		}
		for _, reset := range resets {
			if reset != 0 && (exhausted.ResetAt.IsZero() || fromMillis(reset).Before(exhausted.ResetAt)) {
				exhausted.ResetAt = fromMillis(reset)
			}
		}
		return "", exhausted
	}
	active, err := activeSessions(ctx, tx, id, now)
	if err != nil {
		return "", err
	}
	var pool []standing
	for _, st := range standings {
		if st.tier == lowest {
			pool = append(pool, st)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		switch {
		case a.score != b.score:
			return a.score > b.score
		case active[a.alias] != active[b.alias]:
			return active[a.alias] < active[b.alias]
		}
		return a.alias < b.alias
	})
	return pool[0].alias, nil
}

// activeSessions counts, by account, the sessions other than id that sent a request
// within the active window.
func activeSessions(ctx context.Context, tx *sql.Tx, id string, now int64) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT b.alias, COUNT(*) FROM bindings b JOIN sessions s ON s.id = b.session_id
		WHERE s.ended_at IS NULL AND s.last_seen_at >= ? AND s.id != ? GROUP BY b.alias`, now-activeWindow.Milliseconds(), id)
	if err != nil {
		return nil, fmt.Errorf("count active sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	active := make(map[string]int)
	for rows.Next() {
		var alias string
		var count int
		if err := rows.Scan(&alias, &count); err != nil {
			return nil, fmt.Errorf("count active sessions: %v", err)
		}
		active[alias] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count active sessions: %v", err)
	}
	return active, nil
}
