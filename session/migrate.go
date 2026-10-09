package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Migration is what MigrateBindings did: how many sessions it rebound and how many it
// left without a binding.
type Migration struct {
	Moved    int
	Unplaced int
}

// MigrateBindings rebinds, in one write transaction, every live session bound to one
// of the from accounts to an account chosen among candidates by the selection rules,
// with reason in the binding history. It does not wait for the sessions to send a
// request. A session with nothing to go to loses its binding and its open history row
// is closed; it counts as Unplaced and its next request selects anew. An error leaves
// every binding as it was.
func (s *Store) MigrateBindings(ctx context.Context, from, candidates []string, reason string) (Migration, error) {
	var m Migration
	if len(from) == 0 {
		return m, nil
	}
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		ids, err := boundSessions(ctx, tx, from)
		if err != nil {
			return err
		}
		eligible, blocked, err := unpaused(ctx, tx, candidates, now)
		if err != nil {
			return err
		}
		standings, err := standings(ctx, tx, eligible, now, &blocked)
		if err != nil {
			return err
		}
		for _, id := range ids {
			_, err := s.rebind(ctx, tx, id, standings, blocked, now, reason)
			var exhausted *QuotaExhausted
			switch {
			case errors.Is(err, ErrNoAccount) || errors.As(err, &exhausted):
				if err := unbind(ctx, tx, id, now); err != nil {
					return err
				}
				m.Unplaced++
			case err != nil:
				return err
			default:
				m.Moved++
			}
		}
		return nil
	})
	if err != nil {
		return Migration{}, err
	}
	return m, nil
}

// boundSessions returns the live sessions bound to one of the aliases.
func boundSessions(ctx context.Context, tx *sql.Tx, aliases []string) ([]string, error) {
	args := make([]any, len(aliases))
	for i, alias := range aliases {
		args[i] = alias
	}
	rows, err := tx.QueryContext(ctx, "SELECT b.session_id FROM bindings b JOIN sessions s ON s.id = b.session_id WHERE s.ended_at IS NULL AND b.alias IN (?"+strings.Repeat(", ?", len(aliases)-1)+") ORDER BY b.session_id", args...)
	if err != nil {
		return nil, fmt.Errorf("find bound sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("find bound sessions: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find bound sessions: %v", err)
	}
	return ids, nil
}

// unbind removes session id's binding and closes its open history row at now.
func unbind(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE binding_history SET to_ts = ? WHERE session_id = ? AND to_ts IS NULL", now, id); err != nil {
		return fmt.Errorf("unbind session: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM bindings WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("unbind session: %v", err)
	}
	return nil
}
