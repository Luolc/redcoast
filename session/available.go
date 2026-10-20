package session

import (
	"context"
	"database/sql"
	"fmt"
)

// Available reports whether a new session could be bound to one of candidates right
// now, by the same rules Bind applies: nil when an account is choosable, QuotaExhausted
// when every candidate is out for a quota reason, ErrNoAccount when one is out for
// another reason (setAside names candidates set aside, as Bind takes them) or there is
// none. Launchers ask before a session is issued; the
// answer is a reading of this instant, not a reservation.
func (s *Store) Available(ctx context.Context, candidates []string, setAside ...string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	eligible, blocked, err := unpaused(ctx, tx, candidates, setAside, now)
	if err != nil {
		return err
	}
	standings, err := standings(ctx, tx, eligible, now, &blocked)
	if err != nil {
		return err
	}
	_, err = choose(ctx, tx, "", standings, blocked, now)
	return err
}
