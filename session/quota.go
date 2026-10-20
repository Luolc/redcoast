package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Windows are the two quota windows the upstream reports.
const (
	Window5h = "5h"
	Window7d = "7d"
)

// Reading is one window's quota reading as parsed from an upstream response. The
// caller validates it; the store keeps the latest reading of each account and window.
type Reading struct {
	Window      string
	Utilization float64   // fraction of the window's quota used, 1 at the quota; above 1 when used past it
	Status      string    // allowed, allowed_warning or rejected; empty when absent
	ResetAt     time.Time // when the window resets; zero when absent
	Source      string    // response, 429 or refresh
}

// Quota is the stored latest reading of one window.
type Quota struct {
	Utilization float64   `json:"utilization"`
	Status      string    `json:"status"`
	ResetAt     time.Time `json:"reset_at,omitzero"`
	ObservedAt  time.Time `json:"observed_at"`
	Source      string    `json:"source"`
}

// The quota history keeps a reading of an account's window when the window's last kept
// reading is at least quotaSampleGap old and either differs from it or is at least
// quotaSampleHeartbeat old, so it holds at most one reading per gap and none for a
// stretch that did not change.
const (
	quotaSampleGap       = 5 * time.Minute
	quotaSampleHeartbeat = time.Hour
)

// QuotaPoint is one reading of the quota history.
type QuotaPoint struct {
	At          time.Time `json:"at"`
	Utilization float64   `json:"utilization"`
}

// RecordQuota replaces the latest reading of each window in readings for account alias,
// and adds it to the quota history as the sampling rule above allows. A window absent
// from readings keeps its previous reading.
func (s *Store) RecordQuota(ctx context.Context, alias string, readings []Reading) error {
	if len(readings) == 0 {
		return nil
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		for _, r := range readings {
			if r.Window != Window5h && r.Window != Window7d {
				return errors.New("unknown quota window")
			}
			// Stated positively so that NaN is refused; SQLite would reject it anyway and
			// take the other window's reading down with it.
			if !(r.Utilization >= 0 && r.Utilization <= math.MaxFloat64) {
				return errors.New("utilization is not a finite non-negative fraction")
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO quota_latest (alias, window, utilization, status, reset_at, observed_at, source) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (alias, window) DO UPDATE SET utilization = excluded.utilization, status = excluded.status,
					reset_at = excluded.reset_at, observed_at = excluded.observed_at, source = excluded.source`,
				alias, r.Window, r.Utilization, nullable(r.Status), nullableTime(r.ResetAt), now, r.Source)
			if err != nil {
				return fmt.Errorf("record quota: %v", err)
			}
			if err := sampleQuota(ctx, tx, alias, r, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// sampleQuota adds reading r of account alias, taken at now, to the quota history when
// the sampling rule allows it.
func sampleQuota(ctx context.Context, tx *sql.Tx, alias string, r Reading, now int64) error {
	var at int64
	var utilization float64
	err := tx.QueryRowContext(ctx, "SELECT at, utilization FROM quota_history WHERE alias = ? AND window = ? ORDER BY at DESC LIMIT 1", alias, r.Window).Scan(&at, &utilization)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read quota history: %v", err)
	case now-at < quotaSampleGap.Milliseconds(), utilization == r.Utilization && now-at < quotaSampleHeartbeat.Milliseconds():
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO quota_history (alias, window, at, utilization) VALUES (?, ?, ?, ?)", alias, r.Window, now, r.Utilization); err != nil {
		return fmt.Errorf("record quota history: %v", err)
	}
	return nil
}

// QuotaHistory returns the quota history of account alias by window, oldest first, for
// the length of each window back from now: the last five hours of 5h, the last seven
// days of 7d. A window with no reading in that span has no key.
func (s *Store) QuotaHistory(ctx context.Context, alias string) (map[string][]QuotaPoint, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT window, at, utilization FROM quota_history
		WHERE alias = ? AND ((window = ? AND at >= ?) OR (window = ? AND at >= ?)) ORDER BY window, at`,
		alias, Window5h, now-(5*time.Hour).Milliseconds(), Window7d, now-(7*24*time.Hour).Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("read quota history: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]QuotaPoint{}
	for rows.Next() {
		var window string
		var at int64
		var p QuotaPoint
		if err := rows.Scan(&window, &at, &p.Utilization); err != nil {
			return nil, fmt.Errorf("read quota history: %v", err)
		}
		p.At = fromMillis(at)
		out[window] = append(out[window], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read quota history: %v", err)
	}
	return out, nil
}

// LatestQuota returns the stored readings of account alias by window.
func (s *Store) LatestQuota(ctx context.Context, alias string) (map[string]Quota, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT window, utilization, COALESCE(status, ''), reset_at, observed_at, source FROM quota_latest WHERE alias = ?", alias)
	if err != nil {
		return nil, fmt.Errorf("read quota: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Quota{}
	for rows.Next() {
		var window string
		var q Quota
		var reset sql.NullInt64
		var observed int64
		if err := rows.Scan(&window, &q.Utilization, &q.Status, &reset, &observed, &q.Source); err != nil {
			return nil, fmt.Errorf("read quota: %v", err)
		}
		if reset.Valid {
			q.ResetAt = fromMillis(reset.Int64)
		}
		q.ObservedAt = fromMillis(observed)
		out[window] = q
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read quota: %v", err)
	}
	return out, nil
}

// Thresholds returns the soft and hard quota thresholds from the settings table, as
// fractions. They can be changed in the table while the gateway runs; a missing or
// unparsable value is an error, not a default, so a bad edit is noticed.
func (s *Store) Thresholds(ctx context.Context) (soft, hard float64, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return thresholds(ctx, tx)
}

// thresholds is Thresholds inside a transaction.
func thresholds(ctx context.Context, tx *sql.Tx) (soft, hard float64, err error) {
	values := map[string]float64{}
	for _, key := range []string{"soft", "hard"} {
		var text string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&text); err != nil {
			return 0, 0, fmt.Errorf("read %s threshold: %v", key, err)
		}
		value, err := strconv.ParseFloat(text, 64)
		// Stated positively so that NaN, which fails every comparison, is refused too.
		if err != nil || !(value >= 0 && value <= 1) {
			return 0, 0, fmt.Errorf("%s threshold is not a fraction", key)
		}
		values[key] = value
	}
	if !(values["soft"] <= values["hard"]) {
		return 0, 0, errors.New("soft threshold above hard threshold")
	}
	return values["soft"], values["hard"], nil
}

// SetThresholds writes the soft and hard quota thresholds, as the text Thresholds
// parses, in one transaction. Values Thresholds would refuse are not written: the new
// pair is read back with the same rules before the commit.
func (s *Store) SetThresholds(ctx context.Context, soft, hard string) error {
	now := s.now()
	return s.transact(ctx, func(tx *sql.Tx) error {
		for key, value := range map[string]string{"soft": soft, "hard": hard} {
			if _, err := tx.ExecContext(ctx, "UPDATE settings SET value = ?, updated_at = ? WHERE key = ?", value, now, key); err != nil {
				return fmt.Errorf("write %s threshold: %v", key, err)
			}
		}
		_, _, err := thresholds(ctx, tx)
		return err
	})
}
