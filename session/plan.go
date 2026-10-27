package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrNoPlan means an account has no plan at the given time, or its plan has no quota
// unit then. Such an account is not a candidate.
var ErrNoPlan = errors.New("no plan in effect")

// ErrPlanGap means a plan write would leave an account without a plan with a quota
// unit at a time it had one before. Nothing is written.
var ErrPlanGap = errors.New("the change would leave an account without a plan")

// ErrNothingToEnd means EndPlan found no period of the account in effect at or after
// the given time.
var ErrNothingToEnd = errors.New("account has no plan to end")

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

// SetPlanUnit sets the quota unit of a plan from unit.From on, open-ended; unit.To must
// be zero. In one transaction the plan's period in effect then ends at unit.From (one
// that starts at that instant is replaced), its later periods are deleted and the new
// one is stored; earlier periods stay. A write that would leave an account without a
// plan with a quota unit at a time it had one is refused with ErrPlanGap.
func (s *Store) SetPlanUnit(ctx context.Context, unit PlanUnit) error {
	if unit.Plan == "" || unit.QuotaUnit <= 0 || unit.MonthlyUSD < 0 || unit.From.IsZero() || !unit.To.IsZero() {
		return errors.New("plan unit must name a plan, a positive unit and a start, and no end")
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		return keepCoverage(ctx, tx, func() error {
			if _, err := cut(ctx, tx, "plans", "plan", unit.Plan, unit.From.UnixMilli()); err != nil {
				return err
			}
			return insertPlanUnit(ctx, tx, unit)
		})
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

// SetPlanSchedule puts account period.Alias on period.Plan from period.From on,
// open-ended; period.To must be zero. In one transaction the account's period in effect
// then ends at period.From (one that starts at that instant is replaced), its later
// periods are deleted and the new one is stored; earlier periods stay, and a reader
// sees the schedule either before or after the change. The plan must have a quota unit
// at every time from period.From on (ErrNoPlan otherwise), and a write that would leave
// an account without a plan with a quota unit at a time it had one is refused with
// ErrPlanGap.
func (s *Store) SetPlanSchedule(ctx context.Context, period PlanPeriod) error {
	if period.Alias == "" || period.Plan == "" || (period.Source != "manual" && period.Source != "auto") || period.From.IsZero() || !period.To.IsZero() {
		return errors.New("plan period must name an account, a plan, a source and a start, and no end")
	}
	at := period.From.UnixMilli()
	return s.transact(ctx, func(tx *sql.Tx) error {
		if err := unitFrom(ctx, tx, period.Plan, at); err != nil {
			return err
		}
		return keepCoverage(ctx, tx, func() error {
			if _, err := cut(ctx, tx, "plan_schedule", "alias", period.Alias, at); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO plan_schedule (alias, effective_from, effective_to, plan, source, note, created_at) VALUES (?, ?, NULL, ?, ?, ?, ?)",
				period.Alias, at, period.Plan, period.Source, nullable(period.Note), s.now())
			if err != nil {
				return fmt.Errorf("write plan period: %v", err)
			}
			return nil
		})
	})
}

// EndPlan takes account alias off its plan from at on: the period in effect then ends
// at at (one that starts at that instant is deleted), and its later periods are
// deleted. It is the one plan write that may leave an account without a plan.
// ErrNothingToEnd when the account has no period in effect at or after at.
func (s *Store) EndPlan(ctx context.Context, alias string, at time.Time) error {
	if alias == "" || at.IsZero() {
		return errors.New("plan end must name an account and a time")
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		changed, err := cut(ctx, tx, "plan_schedule", "alias", alias, at.UnixMilli())
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrNothingToEnd
		}
		return nil
	})
}

// cut clears the periods of key in table from at on: the one in effect at at ends
// there, and those that start at at or later are deleted. It returns how many rows it
// ended or deleted.
func cut(ctx context.Context, tx *sql.Tx, table, column, key string, at int64) (int64, error) {
	ended, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET effective_to = ? WHERE %s = ? AND effective_from < ? AND (effective_to IS NULL OR effective_to > ?)", table, column), at, key, at, at)
	if err != nil {
		return 0, fmt.Errorf("end period: %v", err)
	}
	n, err := ended.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("end period: %v", err)
	}
	deleted, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = ? AND effective_from >= ?", table, column), key, at)
	if err != nil {
		return 0, fmt.Errorf("delete later periods: %v", err)
	}
	m, err := deleted.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete later periods: %v", err)
	}
	return n + m, nil
}

// unitFrom fails with ErrNoPlan unless plan has a quota unit at every time from at on.
// A gap can only start at at or where one of the plan's unit periods ends, so it looks
// only at those times.
func unitFrom(ctx context.Context, tx *sql.Tx, plan string, at int64) error {
	ends, err := column[int64](ctx, tx, "SELECT effective_to FROM plans WHERE plan = ? AND effective_to > ?", plan, at)
	if err != nil {
		return err
	}
	for _, t := range append([]int64{at}, ends...) {
		if _, err := planUnitAt(ctx, tx, plan, t); err != nil {
			if errors.Is(err, ErrNoPlan) {
				return fmt.Errorf("plan %s has no quota unit at %s: %w", plan, fromMillis(t).Format(time.RFC3339), err)
			}
			return err
		}
	}
	return nil
}

// span is the time [from, to) in Unix milliseconds; to is math.MaxInt64 when
// open-ended.
type span struct{ from, to int64 }

// keepCoverage runs write and then fails with ErrPlanGap, for the caller's transaction
// to roll back, when some account lacks a plan with a quota unit at a time it had one
// before write.
func keepCoverage(ctx context.Context, tx *sql.Tx, write func() error) error {
	before, err := coverage(ctx, tx)
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	after, err := coverage(ctx, tx)
	if err != nil {
		return err
	}
	for alias, spans := range before {
		for _, s := range spans {
			if at, lost := uncovered(s, after[alias]); lost {
				return fmt.Errorf("%w: %s at %s", ErrPlanGap, alias, fromMillis(at).Format(time.RFC3339))
			}
		}
	}
	return nil
}

// coverage returns, for every account with a plan period, the times in which it has a
// plan with a quota unit, as merged spans, earliest first. Whether it has one changes
// only where a stored period starts or ends, so it looks only at those times.
func coverage(ctx context.Context, tx *sql.Tx) (map[string][]span, error) {
	times, err := column[int64](ctx, tx, `
		SELECT effective_from FROM plans UNION SELECT effective_to FROM plans WHERE effective_to IS NOT NULL
		UNION SELECT effective_from FROM plan_schedule UNION SELECT effective_to FROM plan_schedule WHERE effective_to IS NOT NULL
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	aliases, err := column[string](ctx, tx, "SELECT DISTINCT alias FROM plan_schedule")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]span, len(aliases))
	for _, alias := range aliases {
		var spans []span
		for i, t := range times {
			_, err := quotaUnitAt(ctx, tx, alias, t)
			if errors.Is(err, ErrNoPlan) {
				continue
			}
			if err != nil {
				return nil, err
			}
			end := int64(math.MaxInt64)
			if i+1 < len(times) {
				end = times[i+1]
			}
			if n := len(spans); n > 0 && spans[n-1].to == t {
				spans[n-1].to = end
			} else {
				spans = append(spans, span{t, end})
			}
		}
		out[alias] = spans
	}
	return out, nil
}

// uncovered returns the first time in s that spans, merged and earliest first, do not
// cover, and whether there is one.
func uncovered(s span, spans []span) (int64, bool) {
	at := s.from
	for _, c := range spans {
		if c.from <= at && at < c.to {
			at = c.to
		}
	}
	return at, at < s.to
}

// column returns the values of the one column that query selects.
func column[T any](ctx context.Context, tx *sql.Tx, query string, args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read plan periods: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var values []T
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read plan periods: %v", err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read plan periods: %v", err)
	}
	return values, nil
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

// QuotaUnits returns the quota unit at time at of every account that has a plan
// period, 0 for one with no plan or no unit then.
func (s *Store) QuotaUnits(ctx context.Context, at time.Time) (map[string]float64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT alias FROM plan_schedule")
	if err != nil {
		return nil, fmt.Errorf("read plan schedule: %v", err)
	}
	var aliases []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read plan schedule: %v", err)
		}
		aliases = append(aliases, alias)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read plan schedule: %v", err)
	}
	units := make(map[string]float64, len(aliases))
	for _, alias := range aliases {
		unit, err := quotaUnitAt(ctx, tx, alias, at.UnixMilli())
		if err != nil && !errors.Is(err, ErrNoPlan) {
			return nil, err
		}
		units[alias] = unit
	}
	return units, nil
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
