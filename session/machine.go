package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// DefaultMaxSessions is the session limit of a machine registered without one: a
// conservative start, to be raised from the client machines' real concurrency.
const DefaultMaxSessions = 8

// machineName matches a registered machine's name, which is not secret.
var machineName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// credentialHash matches the SHA-256 of a machine credential in lowercase hex.
var credentialHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Errors callers branch on.
var (
	// ErrUnknownMachine means the credential matches no registered, unrevoked machine.
	ErrUnknownMachine = errors.New("unknown machine credential")
	// ErrMachineExists means a machine with that name or credential is registered.
	ErrMachineExists = errors.New("machine name or credential already registered")
	// ErrNoSuchMachine means no machine has that name.
	ErrNoSuchMachine = errors.New("no such machine")
	// ErrMachineShape means the name or the hash has the wrong shape.
	ErrMachineShape = errors.New("machine name must be 1 to 64 of [a-z0-9_.-] and the credential hash 64 hex digits")
)

// Machine is one registered client machine. The credential itself is never here.
type Machine struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	RevokedAt   time.Time `json:"revoked_at,omitzero"`
	MaxSessions int       `json:"max_sessions"`
}

// MachineLimit is returned by IssueForMachine when the machine is at its limit.
type MachineLimit struct {
	Name        string
	MaxSessions int
}

func (e *MachineLimit) Error() string {
	return fmt.Sprintf("machine %s is at its limit of %d sessions", e.Name, e.MaxSessions)
}

// AddMachine registers a machine by name with the SHA-256 of its credential and its
// session limit (DefaultMaxSessions when 0). The gateway never sees the credential
// itself here; the client machine generated it and sent the hash.
func (s *Store) AddMachine(ctx context.Context, name, hash string, maxSessions int) error {
	if !machineName.MatchString(name) || !credentialHash.MatchString(hash) || maxSessions < 0 {
		return ErrMachineShape
	}
	if maxSessions == 0 {
		maxSessions = DefaultMaxSessions
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		var taken int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM machines WHERE name = ? OR cred_hash = ?", name, hash).Scan(&taken); err != nil {
			return fmt.Errorf("add machine: %v", err)
		}
		if taken > 0 {
			return ErrMachineExists
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO machines (name, cred_hash, created_at, max_sessions) VALUES (?, ?, ?, ?)", name, hash, s.now(), maxSessions); err != nil {
			return fmt.Errorf("add machine: %v", err)
		}
		return nil
	})
}

// RevokeMachine marks the machine revoked and ends every live session it holds, in
// one transaction. It returns how many sessions it ended.
func (s *Store) RevokeMachine(ctx context.Context, name string) (int, error) {
	var ended int
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		result, err := tx.ExecContext(ctx, "UPDATE machines SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL", now, name)
		if err != nil {
			return fmt.Errorf("revoke machine: %v", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNoSuchMachine
		}
		ids, err := machineSessions(ctx, tx, name, "")
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := end(ctx, tx, id, now, "machine_revoked"); err != nil {
				return err
			}
		}
		ended = len(ids)
		return nil
	})
	return ended, err
}

// machineSessions returns the IDs of the machine's live sessions, narrowed by an extra
// SQL condition (with its arguments) when given.
func machineSessions(ctx context.Context, tx *sql.Tx, name, condition string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM sessions WHERE client_machine = ? AND ended_at IS NULL "+condition, append([]any{name}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("machine sessions: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("machine sessions: %v", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("machine sessions: %v", err)
	}
	return ids, nil
}

// SetMachineLimit changes a machine's session limit; sessions above it are not ended.
func (s *Store) SetMachineLimit(ctx context.Context, name string, maxSessions int) error {
	if maxSessions < 1 {
		return errors.New("the session limit must be at least 1")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE machines SET max_sessions = ? WHERE name = ?", maxSessions, name)
	if err != nil {
		return fmt.Errorf("set machine limit: %v", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNoSuchMachine
	}
	return nil
}

// Machines lists every registered machine, revoked ones included, by name.
func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, created_at, revoked_at, max_sessions FROM machines ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list machines: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&m.Name, &created, &revoked, &m.MaxSessions); err != nil {
			return nil, fmt.Errorf("list machines: %v", err)
		}
		m.CreatedAt = fromMillis(created)
		if revoked.Valid {
			m.RevokedAt = fromMillis(revoked.Int64)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list machines: %v", err)
	}
	return out, nil
}

// AuthenticateMachine returns the name of the registered, unrevoked machine whose
// credential this is, or ErrUnknownMachine. The credential is hashed and compared; it
// is stored nowhere and appears in no error.
func (s *Store) AuthenticateMachine(ctx context.Context, credential string) (string, error) {
	if credential == "" || len(credential) > 256 {
		return "", ErrUnknownMachine
	}
	var name string
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT name, revoked_at FROM machines WHERE cred_hash = ?", hashToken(credential)).Scan(&name, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid):
		return "", ErrUnknownMachine
	case err != nil:
		return "", fmt.Errorf("look up machine: %v", err)
	}
	return name, nil
}

// IssueForMachine authenticates credential against the registered machines and, when
// the machine is under its session limit, issues a session for it from source. The
// credential is hashed and compared; it is stored nowhere and appears in no error.
// Sessions of the machine that have had no request for the idle expiry are ended
// first, so a launcher that died without revoking does not hold a slot for good. The
// expiry, the count and the insert are one write transaction, so concurrent requests
// cannot pass the limit together.
func (s *Store) IssueForMachine(ctx context.Context, credential, source string, launchMeta json.RawMessage) (Issued, error) {
	if credential == "" || len(credential) > 256 {
		return Issued{}, ErrUnknownMachine
	}
	var issued Issued
	err := s.transact(ctx, func(tx *sql.Tx) error {
		var name string
		var maxSessions int
		var revoked sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT name, max_sessions, revoked_at FROM machines WHERE cred_hash = ?", hashToken(credential)).Scan(&name, &maxSessions, &revoked)
		switch {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid):
			return ErrUnknownMachine
		case err != nil:
			return fmt.Errorf("look up machine: %v", err)
		}
		now := s.now()
		idle, err := machineSessions(ctx, tx, name, "AND COALESCE(last_seen_at, created_at) + ? <= ?", s.cfg.IdleExpiry.Milliseconds(), now)
		if err != nil {
			return err
		}
		for _, id := range idle {
			if err := end(ctx, tx, id, now, "idle"); err != nil {
				return err
			}
		}
		var live int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE client_machine = ? AND ended_at IS NULL", name).Scan(&live); err != nil {
			return fmt.Errorf("count machine sessions: %v", err)
		}
		if live >= maxSessions {
			return &MachineLimit{Name: name, MaxSessions: maxSessions}
		}
		issued, err = s.insertSession(ctx, tx, name, source, launchMeta)
		return err
	})
	if err != nil {
		return Issued{}, err
	}
	return issued, nil
}
