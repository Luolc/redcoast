package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// TokenPrefix starts every session credential. The gateway issues these credentials
// to its own clients; they are not Anthropic API keys. Only the gateway accepts them,
// and requests it forwards upstream carry the bound account's token instead.
const TokenPrefix = "sk-ant-gws-"

// tokenPattern is the only credential shape accepted: the prefix and 32 random bytes
// in unpadded base64url.
var tokenPattern = regexp.MustCompile(`^sk-ant-gws-[A-Za-z0-9_-]{43}$`)

// maxLaunchMeta bounds the launch metadata saved with a session.
const maxLaunchMeta = 4096

// Errors callers branch on.
var (
	// ErrUnknownSession means the credential is malformed, unknown, revoked or expired.
	ErrUnknownSession = errors.New("unknown session")
	// ErrNoAccount means no candidate account is available for a binding.
	ErrNoAccount = errors.New("no account available")
	// ErrLaunchMeta means the launch metadata is not a JSON object within the size limit.
	ErrLaunchMeta = errors.New("launch metadata must be a JSON object of at most 4096 bytes")
	// ErrWrongSource means the session exists but the request did not come from the
	// address it was issued to.
	ErrWrongSource = errors.New("session used from another address")
)

// LocalSource is the source of a session issued over the unix socket; such a session
// is used from loopback addresses only.
const LocalSource = "local"

// Issued is a newly issued session. Token is returned here once and is not stored.
type Issued struct {
	ID    string
	Token string
}

// Binding is a session's current account and lease.
type Binding struct {
	SessionID      string
	Alias          string
	BoundAt        time.Time
	LeaseExpiresAt time.Time
	Reason         string // new, lease_expired, unavailable or quota_hard
}

// Issue creates a session for clientMachine from sourceAddr and returns its ID and
// credential. The credential is 32 random bytes; only its SHA-256 is stored. launchMeta
// may be nil. Sessions over the network go through IssueForMachine instead.
func (s *Store) Issue(ctx context.Context, clientMachine, sourceAddr string, launchMeta json.RawMessage) (Issued, error) {
	var issued Issued
	err := s.transact(ctx, func(tx *sql.Tx) error {
		var err error
		issued, err = s.insertSession(ctx, tx, clientMachine, sourceAddr, launchMeta)
		return err
	})
	if err != nil {
		return Issued{}, err
	}
	return issued, nil
}

// insertSession generates a session's credential and inserts its row in tx.
func (s *Store) insertSession(ctx context.Context, tx *sql.Tx, clientMachine, sourceAddr string, launchMeta json.RawMessage) (Issued, error) {
	var meta sql.NullString
	if launchMeta != nil {
		if len(launchMeta) > maxLaunchMeta || !json.Valid(launchMeta) || launchMeta[0] != '{' {
			return Issued{}, ErrLaunchMeta
		}
		meta = sql.NullString{String: string(launchMeta), Valid: true}
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Issued{}, fmt.Errorf("issue session: %v", err)
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	id := rand.Text()
	_, err := tx.ExecContext(ctx,
		"INSERT INTO sessions (id, token_hash, client_machine, source_addr, launch_meta, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, hashToken(token), clientMachine, sourceAddr, meta, s.now())
	if err != nil {
		return Issued{}, fmt.Errorf("issue session: %v", err)
	}
	return Issued{ID: id, Token: token}, nil
}

// hashToken returns the hex SHA-256 of a credential, the only form that is stored.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Bind is BindFrom for a request from a loopback address.
func (s *Store) Bind(ctx context.Context, token string, candidates []string) (Binding, error) {
	return s.BindFrom(ctx, token, LocalSource, candidates)
}

// BindFrom evaluates the binding of the session that token identifies for one request
// from source and returns the account to use. source is LocalSource for a loopback
// address and the IP otherwise; it must be the one the session was issued to, or the
// result is ErrWrongSource and nothing is recorded. With ErrNoAccount or QuotaExhausted the returned Binding
// carries only the session's ID. candidates are the accounts the inventory allows (status
// active, with a token reference), in any order; paused accounts and accounts without
// a plan at this time are excluded here, and the rest are tiered by their quota
// readings against the soft and hard thresholds read from the settings for this
// request. The evaluation runs in one write transaction, so concurrent requests of one
// session see the same binding; a credential that is refused never gets that far (see
// refuse). It also records the request as the session's last activity.
func (s *Store) BindFrom(ctx context.Context, token, source string, candidates []string) (Binding, error) {
	if err := s.refuse(ctx, token, source); err != nil {
		return Binding{}, err
	}
	var binding Binding
	// Outcomes that are business results, not failures: the transaction commits so
	// the activity or the end of the session is kept, then the outcome is returned.
	var outcome error
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		id, issuedTo, idle, err := s.liveSession(ctx, tx, token, now)
		if err != nil {
			return err
		}
		if issuedTo != source {
			return ErrWrongSource
		}
		if idle {
			outcome = ErrUnknownSession
			return end(ctx, tx, id, now, "idle")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE id = ?", now, id); err != nil {
			return fmt.Errorf("record session activity: %v", err)
		}
		eligible, blocked, err := unpaused(ctx, tx, candidates, now)
		if err != nil {
			return err
		}
		standings, err := standings(ctx, tx, eligible, now, &blocked)
		if err != nil {
			return err
		}
		binding, err = s.evaluate(ctx, tx, id, standings, blocked, now)
		var exhausted *QuotaExhausted
		if errors.Is(err, ErrNoAccount) || errors.As(err, &exhausted) {
			// The request counts as activity even though no account can serve it, and
			// the caller gets the session's ID for its record.
			binding, outcome = Binding{SessionID: id}, err
			return nil
		}
		return err
	})
	if err != nil {
		return Binding{}, err
	}
	return binding, outcome
}

// evaluate applies the binding rules for session id at now: no binding binds; a bound
// account that is paused, without a plan or at or over hard rebinds at once; an
// expired lease renews on the same account while it is under soft and is otherwise
// re-evaluated by choose, which may land on the same account when nothing better is
// left; anything else keeps the binding.
func (s *Store) evaluate(ctx context.Context, tx *sql.Tx, id string, standings map[string]standing, blocked excluded, now int64) (Binding, error) {
	current := Binding{SessionID: id}
	var boundAt, expires int64
	err := tx.QueryRowContext(ctx, "SELECT alias, bound_at, lease_expires_at, reason FROM bindings WHERE session_id = ?", id).
		Scan(&current.Alias, &boundAt, &expires, &current.Reason)
	st, eligible := standings[current.Alias]
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.rebind(ctx, tx, id, standings, blocked, now, "new")
	case err != nil:
		return Binding{}, fmt.Errorf("read binding: %v", err)
	case !eligible:
		return s.rebind(ctx, tx, id, standings, blocked, now, "unavailable")
	case st.tier == tierExhausted:
		return s.rebind(ctx, tx, id, standings, blocked, now, "quota_hard")
	case expires <= now && st.tier != tierOpen:
		return s.rebind(ctx, tx, id, standings, blocked, now, "lease_expired")
	case expires <= now:
		newExpiry := now + s.cfg.Lease.Milliseconds()
		if _, err := tx.ExecContext(ctx, "UPDATE bindings SET lease_expires_at = ? WHERE session_id = ?", newExpiry, id); err != nil {
			return Binding{}, fmt.Errorf("renew lease: %v", err)
		}
		expires = newExpiry
	}
	current.BoundAt = fromMillis(boundAt)
	current.LeaseExpiresAt = fromMillis(expires)
	return current, nil
}

// rebind binds session id to the account choose picks, closes the previous history row
// and opens a new one. Rebinding to the same account still starts a new lease.
func (s *Store) rebind(ctx context.Context, tx *sql.Tx, id string, standings map[string]standing, blocked excluded, now int64, reason string) (Binding, error) {
	alias, err := choose(ctx, tx, id, standings, blocked, now)
	if err != nil {
		return Binding{}, err
	}
	expires := now + s.cfg.Lease.Milliseconds()
	statements := []struct {
		query string
		args  []any
	}{
		{"UPDATE binding_history SET to_ts = ? WHERE session_id = ? AND to_ts IS NULL", []any{now, id}},
		{"INSERT INTO binding_history (session_id, alias, from_ts, reason) VALUES (?, ?, ?, ?)", []any{id, alias, now, reason}},
		{"INSERT OR REPLACE INTO bindings (session_id, alias, bound_at, lease_expires_at, reason) VALUES (?, ?, ?, ?, ?)", []any{id, alias, now, expires, reason}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return Binding{}, fmt.Errorf("write binding: %v", err)
		}
	}
	return Binding{SessionID: id, Alias: alias, BoundAt: fromMillis(now), LeaseExpiresAt: fromMillis(expires), Reason: reason}, nil
}

// QuotaPauseReason is the pause reason of an account the upstream rate-limited; such
// a pause is a quota exclusion, like a reading at or over hard.
const QuotaPauseReason = "upstream_429"

// unpaused returns the candidates whose account is not paused at now, and the
// exclusions of the others: the end of each quota pause, and whether any candidate is
// out for another reason.
func unpaused(ctx context.Context, tx *sql.Tx, candidates []string, now int64) (map[string]bool, excluded, error) {
	eligible := make(map[string]bool, len(candidates))
	for _, alias := range candidates {
		eligible[alias] = true
	}
	out := excluded{quotaPaused: map[string]int64{}}
	rows, err := tx.QueryContext(ctx, "SELECT alias, paused_until, COALESCE(pause_reason, '') FROM account_state WHERE paused_until IS NOT NULL AND paused_until > ?", now)
	if err != nil {
		return nil, out, fmt.Errorf("read paused accounts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var alias, reason string
		var until int64
		if err := rows.Scan(&alias, &until, &reason); err != nil {
			return nil, out, fmt.Errorf("read paused accounts: %v", err)
		}
		if !eligible[alias] {
			continue
		}
		delete(eligible, alias)
		if reason == QuotaPauseReason {
			out.quotaPaused[alias] = until
		} else {
			out.other = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, out, fmt.Errorf("read paused accounts: %v", err)
	}
	return eligible, out, nil
}

// refuse returns ErrUnknownSession or ErrWrongSource when token cannot be used from
// source, read outside any transaction: every transaction takes the write lock when it
// begins, and a client without a valid credential must not hold it or wait for it. A
// nil result is checked again inside the transaction, because the session may end in
// between.
func (s *Store) refuse(ctx context.Context, token, source string) error {
	_, issuedTo, _, err := s.liveSession(ctx, s.db, token, s.now())
	if err == nil && issuedTo != source {
		return ErrWrongSource
	}
	return err
}

// rowQuerier is what liveSession reads through: a transaction, or the pool for refuse.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// liveSession returns the ID of the session token identifies and the source it was
// issued to, or ErrUnknownSession when the token is malformed, unknown or the session
// has ended. idle reports that the session has had no request for the idle expiry;
// the caller ends it and commits, then treats it as unknown.
func (s *Store) liveSession(ctx context.Context, tx rowQuerier, token string, now int64) (id, source string, idle bool, err error) {
	if !tokenPattern.MatchString(token) {
		return "", "", false, ErrUnknownSession
	}
	var lastActivity int64
	err = tx.QueryRowContext(ctx,
		"SELECT id, source_addr, COALESCE(last_seen_at, created_at) FROM sessions WHERE token_hash = ? AND ended_at IS NULL", hashToken(token)).
		Scan(&id, &source, &lastActivity)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", false, ErrUnknownSession
	case err != nil:
		return "", "", false, fmt.Errorf("look up session: %v", err)
	}
	return id, source, lastActivity+s.cfg.IdleExpiry.Milliseconds() <= now, nil
}

// end marks session id as ended at now with reason and closes its history row.
func end(ctx context.Context, tx *sql.Tx, id string, now int64, reason string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ?", now, reason, id); err != nil {
		return fmt.Errorf("end session: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE binding_history SET to_ts = ? WHERE session_id = ? AND to_ts IS NULL", now, id); err != nil {
		return fmt.Errorf("end session: %v", err)
	}
	return nil
}

// Revoke ends the session token identifies with reason. The credential is refused
// from then on. Revoking an unknown or ended session returns ErrUnknownSession.
func (s *Store) Revoke(ctx context.Context, token, reason string) error {
	return s.RevokeFrom(ctx, token, LocalSource, reason)
}

// RevokeFrom is Revoke for a request from source, which must be the one the session
// was issued to (as for BindFrom); otherwise ErrWrongSource and the session goes on.
func (s *Store) RevokeFrom(ctx context.Context, token, source, reason string) error {
	if err := s.refuse(ctx, token, source); err != nil {
		return err
	}
	var expired bool
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		id, issuedTo, idle, err := s.liveSession(ctx, tx, token, now)
		if err != nil {
			return err
		}
		if issuedTo != source {
			return ErrWrongSource
		}
		if idle {
			expired = true
			reason = "idle"
		}
		return end(ctx, tx, id, now, reason)
	})
	if err != nil {
		return err
	}
	if expired {
		return ErrUnknownSession
	}
	return nil
}

// ExpireIdle ends every session with no request for the idle expiry and returns how
// many it ended. Bind does the same for the one session it evaluates; this covers
// sessions nobody uses any more.
func (s *Store) ExpireIdle(ctx context.Context) (int, error) {
	var ended int
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := s.now()
		rows, err := tx.QueryContext(ctx,
			"SELECT id FROM sessions WHERE ended_at IS NULL AND COALESCE(last_seen_at, created_at) + ? <= ?", s.cfg.IdleExpiry.Milliseconds(), now)
		if err != nil {
			return fmt.Errorf("find idle sessions: %v", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("find idle sessions: %v", err)
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("find idle sessions: %v", err)
		}
		for _, id := range ids {
			if err := end(ctx, tx, id, now, "idle"); err != nil {
				return err
			}
		}
		ended = len(ids)
		return nil
	})
	return ended, err
}

// Pause stops account alias from receiving bindings until until; sessions bound to it
// rebind on their next request. reason is kept for the operator. A pause never
// shortens one still in effect: when responses of in-flight requests arrive out of
// order, a rate limit's pause must not replace the indefinite pause of a refused
// token. Resume is the only way to end a pause early.
func (s *Store) Pause(ctx context.Context, alias string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_state (alias, paused_until, pause_reason, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (alias) DO UPDATE SET
			pause_reason = CASE WHEN paused_until IS NOT NULL AND paused_until > excluded.paused_until THEN pause_reason ELSE excluded.pause_reason END,
			paused_until = CASE WHEN paused_until IS NOT NULL AND paused_until > excluded.paused_until THEN paused_until ELSE excluded.paused_until END,
			updated_at = excluded.updated_at`,
		alias, until.UnixMilli(), reason, s.now())
	if err != nil {
		return fmt.Errorf("pause account: %v", err)
	}
	return nil
}

// Resume clears the pause of account alias.
func (s *Store) Resume(ctx context.Context, alias string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE account_state SET paused_until = NULL, pause_reason = NULL, updated_at = ? WHERE alias = ?", s.now(), alias)
	if err != nil {
		return fmt.Errorf("resume account: %v", err)
	}
	return nil
}

// transact runs fn in one write transaction (BEGIN IMMEDIATE) and commits when it
// returns nil.
func (s *Store) transact(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %v", err)
	}
	return nil
}
