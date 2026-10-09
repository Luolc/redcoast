package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Default retention of the rows Prune deletes. sessions outlive traffic because a
// traffic row is matched to its machine and launch metadata through its session.
const (
	DefaultTrafficRetention = 180 * 24 * time.Hour
	DefaultSessionRetention = 365 * 24 * time.Hour
)

// pruneBatch is how many rows one delete transaction removes, so a large backlog never
// holds the write lock long enough to stall requests.
const pruneBatch = 1000

// pruneInterval is how often RunRetention prunes.
const pruneInterval = 24 * time.Hour

// Retention is how long Prune keeps traffic rows, and closed binding_history rows and
// ended sessions.
type Retention struct {
	Traffic  time.Duration
	Sessions time.Duration
}

// Pruned counts the rows one Prune deleted.
type Pruned struct {
	Traffic  int `json:"traffic"`
	History  int `json:"binding_history"`
	Sessions int `json:"sessions"`
}

// RetentionRun is the outcome of the last retention run since the gateway started.
type RetentionRun struct {
	At       time.Time `json:"at,omitzero"` // absent before the first run
	Pruned   Pruned    `json:"pruned"`
	Vacuumed bool      `json:"vacuumed"`
	Error    string    `json:"error,omitempty"`
}

// LastRetention returns the last retention run; zero when none finished yet.
func (s *Store) LastRetention() RetentionRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRetention
}

// QuickCheck runs PRAGMA quick_check and returns an error unless the file is intact.
// The gateway does not start on a file that fails it.
func (s *Store) QuickCheck(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("session store quick_check: %v", err)
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return fmt.Errorf("session store quick_check: %v", err)
		}
		problems = append(problems, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("session store quick_check: %v", err)
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return fmt.Errorf("session store failed quick_check: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Prune deletes traffic rows older than r.Traffic, binding_history rows closed longer
// ago than r.Sessions, and sessions ended longer ago than r.Sessions together with
// their bindings row. A session that traffic or binding_history still refers to is
// kept. Each batch is its own transaction.
func (s *Store) Prune(ctx context.Context, r Retention) (Pruned, error) {
	trafficCutoff := s.now() - r.Traffic.Milliseconds()
	sessionCutoff := s.now() - r.Sessions.Milliseconds()
	var out Pruned
	var err error
	if out.Traffic, err = s.inBatches(ctx, func(tx *sql.Tx) (int64, error) {
		return exec(ctx, tx, "DELETE FROM traffic WHERE id IN (SELECT id FROM traffic WHERE ts < ? ORDER BY ts LIMIT ?)", trafficCutoff, pruneBatch)
	}); err != nil {
		return out, fmt.Errorf("prune traffic: %v", err)
	}
	if out.History, err = s.inBatches(ctx, func(tx *sql.Tx) (int64, error) {
		return exec(ctx, tx, "DELETE FROM binding_history WHERE id IN (SELECT id FROM binding_history WHERE to_ts < ? LIMIT ?)", sessionCutoff, pruneBatch)
	}); err != nil {
		return out, fmt.Errorf("prune binding history: %v", err)
	}
	if out.Sessions, err = s.inBatches(ctx, func(tx *sql.Tx) (int64, error) {
		return pruneSessions(ctx, tx, sessionCutoff)
	}); err != nil {
		return out, fmt.Errorf("prune sessions: %v", err)
	}
	return out, nil
}

// pruneSessions deletes up to pruneBatch sessions ended before cutoff that no traffic
// or binding_history row refers to, each with its bindings row, which the foreign key
// would otherwise keep.
func pruneSessions(ctx context.Context, tx *sql.Tx, cutoff int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions s WHERE ended_at < ?
		AND NOT EXISTS (SELECT 1 FROM traffic t WHERE t.session_id = s.id)
		AND NOT EXISTS (SELECT 1 FROM binding_history h WHERE h.session_id = s.id)
		LIMIT ?`, cutoff, pruneBatch)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := exec(ctx, tx, "DELETE FROM bindings WHERE session_id = ?", id); err != nil {
			return 0, err
		}
		if _, err := exec(ctx, tx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}

// exec runs one statement in tx and returns the rows it changed.
func exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// inBatches runs batch in one transaction at a time until it deletes fewer than
// pruneBatch rows, and returns the total.
func (s *Store) inBatches(ctx context.Context, batch func(tx *sql.Tx) (int64, error)) (int, error) {
	total := 0
	for {
		var deleted int64
		err := s.transact(ctx, func(tx *sql.Tx) error {
			var err error
			deleted, err = batch(tx)
			return err
		})
		if err != nil {
			return total, err
		}
		total += int(deleted)
		if deleted < pruneBatch {
			return total, nil
		}
	}
}

// VacuumIfSparse rebuilds the file with VACUUM when at least a quarter of its pages are
// free. Deleted rows leave free pages that later inserts reuse, so the file only needs
// shrinking after a large prune; VACUUM holds the write lock while it copies the file.
func (s *Store) VacuumIfSparse(ctx context.Context) (bool, error) {
	var free, pages int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return false, fmt.Errorf("session store free pages: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return false, fmt.Errorf("session store pages: %v", err)
	}
	if free*4 < pages {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return false, fmt.Errorf("session store vacuum: %v", err)
	}
	return true, nil
}

// RunRetention prunes with r once at start and then every day until ctx is canceled.
// A failed run is logged and retried the next day; it never stops the gateway.
func (s *Store) RunRetention(ctx context.Context, r Retention) error {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		pruned, err := s.Prune(ctx, r)
		vacuumed := false
		if err == nil {
			vacuumed, err = s.VacuumIfSparse(ctx)
		}
		if ctx.Err() != nil {
			return nil
		}
		run := RetentionRun{At: fromMillis(s.now()), Pruned: pruned, Vacuumed: vacuumed}
		if err != nil {
			run.Error = err.Error()
		}
		s.mu.Lock()
		s.lastRetention = run
		s.mu.Unlock()
		if err != nil {
			log.Printf("retention: %v", err)
		} else {
			log.Printf("retention: deleted traffic=%d binding_history=%d sessions=%d vacuum=%t", pruned.Traffic, pruned.History, pruned.Sessions, vacuumed)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// VacuumInto writes a consistent copy of the file to path, which must not exist. It
// reads under one transaction, so the gateway keeps serving while it runs.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("session store snapshot: %v", err)
	}
	return nil
}
