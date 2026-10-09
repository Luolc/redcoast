package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoPlan means an account has no plan at the given time, or its plan has no quota
// unit then. Such an account is not a candidate.
var ErrNoPlan = errors.New("no plan in effect")

// ErrPeriodOverlap means a plan period overlaps one already stored for the same plan
// or account.
var ErrPeriodOverlap = errors.New("period overlaps an existing one")

// PlanUnit is the quota unit and monthly price of a subscription plan over one period.
// A zero To means open-ended.
type PlanUnit struct {
	Plan       string
	QuotaUnit  float64
	MonthlyUSD float64
	From, To   time.Time
}

// PlanPeriod is one period of an account's plan. A zero To means open-ended. Source is
// manual (written by the management command) or auto (a later stage's automatic
// change).
type PlanPeriod struct {
	Alias, Plan  string
	From, To     time.Time
	Source, Note string
}

// SetPlanUnit stores the quota unit of a plan from unit.From on. A period that starts
// at the same instant replaces the stored one; one that overlaps another period of the
// same plan is refused.
func (s *Store) SetPlanUnit(ctx context.Context, unit PlanUnit) error {
	if unit.Plan == "" || unit.QuotaUnit <= 0 || unit.MonthlyUSD < 0 || (!unit.To.IsZero() && !unit.To.After(unit.From)) {
		return errors.New("plan unit must name a plan, a positive unit and an ordered period")
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		if err := checkOverlap(ctx, tx, "plans", "plan", unit.Plan, unit.From, unit.To); err != nil {
			return err
		}
		return insertPlanUnit(ctx, tx, unit)
	})
}

// insertPlanUnit writes unit, replacing a row with the same plan and start.
func insertPlanUnit(ctx context.Context, tx *sql.Tx, unit PlanUnit) error {
	_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO plans (plan, effective_from, effective_to, quota_unit, monthly_usd) VALUES (?, ?, ?, ?, ?)",
		unit.Plan, unit.From.UnixMilli(), nullableTime(unit.To), unit.QuotaUnit, unit.MonthlyUSD)
	if err != nil {
		return fmt.Errorf("write plan unit: %v", err)
	}
	return nil
}

// SetPlanSchedule stores one period of an account's plan. The plan must have a quota
// unit at the period's start; a period that starts at the same instant as a stored one
// replaces it; one that overlaps another period of the account is refused.
func (s *Store) SetPlanSchedule(ctx context.Context, period PlanPeriod) error {
	if period.Alias == "" || period.Plan == "" || (period.Source != "manual" && period.Source != "auto") || (!period.To.IsZero() && !period.To.After(period.From)) {
		return errors.New("plan period must name an account, a plan, a source and an ordered period")
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		if _, err := planUnitAt(ctx, tx, period.Plan, period.From.UnixMilli()); err != nil {
			return err
		}
		if err := checkOverlap(ctx, tx, "plan_schedule", "alias", period.Alias, period.From, period.To); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO plan_schedule (alias, effective_from, effective_to, plan, source, note, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			period.Alias, period.From.UnixMilli(), nullableTime(period.To), period.Plan, period.Source, nullable(period.Note), s.now())
		if err != nil {
			return fmt.Errorf("write plan period: %v", err)
		}
		return nil
	})
}

// checkOverlap fails with ErrPeriodOverlap when table holds a period of key that
// overlaps [from, to), other than one starting exactly at from, which the caller
// replaces. SQLite cannot express this constraint, so it is checked here.
func checkOverlap(ctx context.Context, tx *sql.Tx, table, column, key string, from, to time.Time) error {
	end := sql.NullInt64{Int64: to.UnixMilli(), Valid: !to.IsZero()}
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*) FROM %s WHERE %s = ? AND effective_from != ?
		AND (effective_to IS NULL OR effective_to > ?)
		AND (? IS NULL OR effective_from < ?)`, table, column), key, from.UnixMilli(), from.UnixMilli(), end, end).Scan(&count)
	if err != nil {
		return fmt.Errorf("check periods: %v", err)
	}
	if count > 0 {
		return ErrPeriodOverlap
	}
	return nil
}

// QuotaUnit returns the quota unit of account alias at time at: the plan in effect
// then, and that plan's unit in effect then. ErrNoPlan when either is missing.
func (s *Store) QuotaUnit(ctx context.Context, alias string, at time.Time) (float64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return quotaUnitAt(ctx, tx, alias, at.UnixMilli())
}

// quotaUnitAt is QuotaUnit inside a transaction, with the time in Unix milliseconds.
func quotaUnitAt(ctx context.Context, tx *sql.Tx, alias string, at int64) (float64, error) {
	var plan string
	err := tx.QueryRowContext(ctx, "SELECT plan FROM plan_schedule WHERE alias = ? AND effective_from <= ? AND (effective_to IS NULL OR effective_to > ?)", alias, at, at).Scan(&plan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNoPlan
	case err != nil:
		return 0, fmt.Errorf("read plan: %v", err)
	}
	return planUnitAt(ctx, tx, plan, at)
}

// planUnitAt returns plan's quota unit in effect at the time in Unix milliseconds.
func planUnitAt(ctx context.Context, tx *sql.Tx, plan string, at int64) (float64, error) {
	var unit float64
	err := tx.QueryRowContext(ctx, "SELECT quota_unit FROM plans WHERE plan = ? AND effective_from <= ? AND (effective_to IS NULL OR effective_to > ?)", plan, at, at).Scan(&unit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNoPlan
	case err != nil:
		return 0, fmt.Errorf("read plan unit: %v", err)
	}
	return unit, nil
}

// PlanSchedule returns account alias's plan periods, earliest first.
func (s *Store) PlanSchedule(ctx context.Context, alias string) ([]PlanPeriod, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT plan, effective_from, effective_to, source, COALESCE(note, '') FROM plan_schedule WHERE alias = ? ORDER BY effective_from", alias)
	if err != nil {
		return nil, fmt.Errorf("read plan schedule: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var periods []PlanPeriod
	for rows.Next() {
		period := PlanPeriod{Alias: alias}
		var from int64
		var to sql.NullInt64
		if err := rows.Scan(&period.Plan, &from, &to, &period.Source, &period.Note); err != nil {
			return nil, fmt.Errorf("read plan schedule: %v", err)
		}
		period.From = fromMillis(from)
		if to.Valid {
			period.To = fromMillis(to.Int64)
		}
		periods = append(periods, period)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read plan schedule: %v", err)
	}
	return periods, nil
}

// nullableTime stores a zero time as NULL.
func nullableTime(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: !t.IsZero()}
}
