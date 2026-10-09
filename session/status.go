package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccountStatus is one account's state for the operator.
type AccountStatus struct {
	Alias          string           `json:"alias"`
	PausedUntil    time.Time        `json:"paused_until,omitzero"`
	PauseReason    string           `json:"pause_reason,omitempty"`
	BoundSessions  int              `json:"bound_sessions"`  // live sessions bound to the account
	ActiveSessions int              `json:"active_sessions"` // of them, with a request in the active window
	Quota          map[string]Quota `json:"quota"`           // latest reading by window
	Plan           *PlanStatus      `json:"plan"`            // plan period in effect; null when none
}

// PlanStatus is the plan period in effect for an account, the one selection reads.
type PlanStatus struct {
	Name string     `json:"name"`
	From time.Time  `json:"effective_from"`
	To   *time.Time `json:"effective_to"` // null when open-ended
}

// Status is the operator's view of the store at one instant.
type Status struct {
	At           time.Time       `json:"at"`
	Soft         float64         `json:"soft"`
	Hard         float64         `json:"hard"`
	LiveSessions int             `json:"live_sessions"`
	Accounts     []AccountStatus `json:"accounts"`
}

// Status reports the thresholds, the live sessions and each candidate account's pause,
// sessions and quota readings at now.
func (s *Store) Status(ctx context.Context, candidates []string) (Status, error) {
	now := s.now()
	out := Status{At: fromMillis(now), Accounts: []AccountStatus{}}
	var err error
	if out.Soft, out.Hard, err = s.Thresholds(ctx); err != nil {
		return Status{}, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL").Scan(&out.LiveSessions); err != nil {
		return Status{}, fmt.Errorf("count sessions: %v", err)
	}
	for _, alias := range candidates {
		a := AccountStatus{Alias: alias}
		var until sql.NullInt64
		var reason sql.NullString
		err := s.db.QueryRowContext(ctx, "SELECT paused_until, pause_reason FROM account_state WHERE alias = ?", alias).Scan(&until, &reason)
		if err != nil && err != sql.ErrNoRows {
			return Status{}, fmt.Errorf("read account state: %v", err)
		}
		if until.Valid && until.Int64 > now {
			a.PausedUntil, a.PauseReason = fromMillis(until.Int64), reason.String
		}
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(s.last_seen_at >= ?), 0) FROM bindings b JOIN sessions s ON s.id = b.session_id
			WHERE s.ended_at IS NULL AND b.alias = ?`, now-activeWindow.Milliseconds(), alias).Scan(&a.BoundSessions, &a.ActiveSessions); err != nil {
			return Status{}, fmt.Errorf("count bound sessions: %v", err)
		}
		if a.Quota, err = s.LatestQuota(ctx, alias); err != nil {
			return Status{}, err
		}
		if a.Plan, err = s.planInEffect(ctx, alias, now); err != nil {
			return Status{}, err
		}
		out.Accounts = append(out.Accounts, a)
	}
	return out, nil
}

// planInEffect returns alias's plan period in effect at the time in Unix milliseconds,
// with the same condition as quotaUnitAt, or nil when there is none.
func (s *Store) planInEffect(ctx context.Context, alias string, at int64) (*PlanStatus, error) {
	var plan PlanStatus
	var from int64
	var to sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT plan, effective_from, effective_to FROM plan_schedule WHERE alias = ? AND effective_from <= ? AND (effective_to IS NULL OR effective_to > ?)", alias, at, at).Scan(&plan.Name, &from, &to)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read plan in effect: %v", err)
	}
	plan.From = fromMillis(from)
	if to.Valid {
		end := fromMillis(to.Int64)
		plan.To = &end
	}
	return &plan, nil
}
