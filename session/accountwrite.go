package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Errors of the account writes.
var (
	ErrAccountExists   = errors.New("account exists")
	ErrAccountNotFound = errors.New("no such account")
	ErrRevConflict     = errors.New("account changed since it was read")
)

// schemaV10 adds what the account writes keep: the result of each of the three checks
// of an account (NULL when not run, 0 failed, 1 passed) with the time of the last run,
// and the audit table. An audit row holds the old and new values of the fields a write
// changed, as JSON; those fields are references and plain settings, never a credential
// value.
const schemaV10 = `
ALTER TABLE accounts ADD COLUMN token_ok INTEGER;
ALTER TABLE accounts ADD COLUMN egress_ok INTEGER;
ALTER TABLE accounts ADD COLUMN probe_ok INTEGER;
ALTER TABLE accounts ADD COLUMN verified_at INTEGER;
CREATE TABLE account_audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	source_ip TEXT NOT NULL,
	action TEXT NOT NULL,
	alias TEXT NOT NULL,
	changes TEXT NOT NULL
) STRICT;
CREATE INDEX account_audit_alias ON account_audit(alias, id);
`

// migrateToV10 adds the verification columns and the audit table.
func migrateToV10(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV10); err != nil {
		return fmt.Errorf("session store migration v10: %v", err)
	}
	return nil
}

// Verification is the outcome of the three checks of an account: nil for a check not
// run or voided, otherwise whether it passed. At is when they last ran, zero for never.
type Verification struct {
	TokenOK  *bool     `json:"token_ok"`
	EgressOK *bool     `json:"egress_ok"`
	ProbeOK  *bool     `json:"probe_ok"`
	At       time.Time `json:"at,omitzero"`
}

// Equal reports whether two verifications hold the same results and time.
func (v Verification) Equal(o Verification) bool {
	same := func(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	return same(v.TokenOK, o.TokenOK) && same(v.EgressOK, o.EgressOK) && same(v.ProbeOK, o.ProbeOK) && v.At.Equal(o.At)
}

// Source says where a request came from when a person did not make it on the dashboard:
// the kind (SourceSlack) and, for a Slack message, its thread key CHANNEL/TS and its ts.
// It is a record, never proof that anyone agreed to anything. The zero Source is the
// dashboard's, whose requests are recorded by their source address.
type Source struct {
	Kind, Thread, TS string
}

// The kinds of Source. A request without a Source is the dashboard's.
const (
	SourceDashboard = "dashboard"
	SourceSlack     = "slack"
)

// AuditEntry says who wrote and what: the source address, the action name and, for a
// request through the management socket, its Source.
type AuditEntry struct {
	SourceIP string
	Action   string
	Source   Source
}

// AuditRow is one row of the audit table; Changes maps a field name to its old and new
// value, a check result as pass, fail or empty.
type AuditRow struct {
	Time     time.Time
	SourceIP string
	Action   string
	Alias    string
	Source   Source
	Changes  map[string]FieldChange
}

// FieldChange is the old and new value of one field.
type FieldChange struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// fieldValues lists the fields an audit row can name with their values in row, in the
// names the accounts table has.
func fieldValues(row AccountRow) map[string]string {
	return map[string]string{
		"email": row.Email, "access": row.Access, "status": row.Status, "token_ref": row.TokenRef,
		"proxy_direct": strconv.FormatBool(row.Direct), "proxy_host": row.ProxyHost, "proxy_port": strconv.Itoa(row.ProxyPort),
		"expected_egress_ip": row.ExpectedIP, "proxy_username_ref": row.UsernameRef, "proxy_password_ref": row.PasswordRef,
		"device_id": row.DeviceID, "account_uuid": row.AccountUUID,
		"receiver_email": row.ReceiverEmail, "proxy_provider": row.ProxyProvider, "proxy_type": row.ProxyType, "observation_since": row.ObservationSince,
		"token_ok": checkWord(row.Verify.TokenOK), "egress_ok": checkWord(row.Verify.EgressOK), "probe_ok": checkWord(row.Verify.ProbeOK),
	}
}

// checkWord names a check result for the audit.
func checkWord(ok *bool) string {
	switch {
	case ok == nil:
		return ""
	case *ok:
		return "pass"
	}
	return "fail"
}

// ChangedFields returns the fields whose value differs between before and after.
func ChangedFields(before, after AccountRow) map[string]FieldChange {
	changes := map[string]FieldChange{}
	old, now := fieldValues(before), fieldValues(after)
	for name, value := range now {
		if old[name] != value {
			changes[name] = FieldChange{Old: old[name], New: value}
		}
	}
	return changes
}

// voidsChecks reports whether a change from before to after makes the verification
// stale: the exit, the expected exit IP or a reference changed.
func voidsChecks(before, after AccountRow) bool {
	return before.Direct != after.Direct || before.ProxyHost != after.ProxyHost || before.ProxyPort != after.ProxyPort ||
		before.ExpectedIP != after.ExpectedIP || before.UsernameRef != after.UsernameRef || before.PasswordRef != after.PasswordRef ||
		before.TokenRef != after.TokenRef
}

// CreateAccount inserts row, as revision 1 with no check run, and the audit row of its
// creation, in one transaction. ErrAccountExists when the alias is taken.
func (s *Store) CreateAccount(ctx context.Context, row AccountRow, audit AuditEntry) (AccountRow, error) {
	row.Rev, row.Verify = 1, Verification{}
	err := s.transact(ctx, func(tx *sql.Tx) error {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE alias = ?", row.Alias).Scan(&one); {
		case err == nil:
			return ErrAccountExists
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("create account: %v", err)
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (alias, email, access, status, token_ref, proxy_direct, proxy_host, proxy_port,
			expected_egress_ip, proxy_username_ref, proxy_password_ref, device_id, account_uuid, receiver_email, proxy_provider, proxy_type, observation_since, rev, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			row.Alias, row.Email, row.Access, row.Status, row.TokenRef, row.Direct, row.ProxyHost, row.ProxyPort,
			row.ExpectedIP, row.UsernameRef, row.PasswordRef, row.DeviceID, row.AccountUUID,
			row.ReceiverEmail, row.ProxyProvider, row.ProxyType, nullableString(row.ObservationSince), now, now); err != nil {
			return fmt.Errorf("create account: %v", err)
		}
		row.CreatedAt, row.UpdatedAt = fromMillis(now), fromMillis(now)
		return insertAudit(ctx, tx, now, audit, row.Alias, ChangedFields(AccountRow{}, row))
	})
	return row, err
}

// UpdateAccount applies apply to account alias at revision rev: ErrAccountNotFound for
// no such account and ErrRevConflict for another revision. An error of apply ends the
// write with that error. A change of the exit, the expected exit IP or a reference
// voids the three checks, unless apply set them. The write, a revision bump and the
// audit row are one transaction; apply that changes nothing writes nothing and returns
// the row as it is.
func (s *Store) UpdateAccount(ctx context.Context, alias string, rev int64, audit AuditEntry, apply func(row *AccountRow) error) (AccountRow, error) {
	var out AccountRow
	err := s.transact(ctx, func(tx *sql.Tx) error {
		before, err := accountRow(ctx, tx, alias)
		if err != nil {
			return err
		}
		if before.Rev != rev {
			return ErrRevConflict
		}
		after := before
		if err := apply(&after); err != nil {
			return err
		}
		after.Alias, after.Rev, after.CreatedAt = before.Alias, before.Rev, before.CreatedAt
		if voidsChecks(before, after) && after.Verify.Equal(before.Verify) {
			after.Verify = Verification{}
		}
		changes := ChangedFields(before, after)
		if len(changes) == 0 {
			out = before
			return nil
		}
		now := s.now()
		after.Rev, after.UpdatedAt = before.Rev+1, fromMillis(now)
		if err := writeRow(ctx, tx, after); err != nil {
			return err
		}
		out = after
		return insertAudit(ctx, tx, now, audit, alias, changes)
	})
	return out, err
}

// writeRow overwrites the row of after.Alias.
func writeRow(ctx context.Context, tx *sql.Tx, row AccountRow) error {
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET email = ?, status = ?, token_ref = ?, proxy_direct = ?, proxy_host = ?, proxy_port = ?,
		expected_egress_ip = ?, proxy_username_ref = ?, proxy_password_ref = ?, device_id = ?, account_uuid = ?,
		receiver_email = ?, proxy_provider = ?, proxy_type = ?, observation_since = ?, token_ok = ?, egress_ok = ?, probe_ok = ?, verified_at = ?, rev = ?, updated_at = ? WHERE alias = ?`,
		row.Email, row.Status, row.TokenRef, row.Direct, row.ProxyHost, row.ProxyPort,
		row.ExpectedIP, row.UsernameRef, row.PasswordRef, row.DeviceID, row.AccountUUID,
		row.ReceiverEmail, row.ProxyProvider, row.ProxyType, nullableString(row.ObservationSince),
		nullableBool(row.Verify.TokenOK), nullableBool(row.Verify.EgressOK), nullableBool(row.Verify.ProbeOK), nullableTime(row.Verify.At),
		row.Rev, row.UpdatedAt.UnixMilli(), row.Alias)
	if err != nil {
		return fmt.Errorf("write account: %v", err)
	}
	return nil
}

// nullableBool stores nil as NULL.
func nullableBool(b *bool) sql.NullBool {
	return sql.NullBool{Bool: b != nil && *b, Valid: b != nil}
}

// SetVerification stores the result of the three checks of account alias at revision
// rev, which is not bumped, with the audit row of the change. ErrRevConflict when the
// account changed while the checks ran.
func (s *Store) SetVerification(ctx context.Context, alias string, rev int64, v Verification, audit AuditEntry) (AccountRow, error) {
	return s.UpdateAccountKeepRev(ctx, alias, rev, audit, func(row *AccountRow) { row.Verify = v })
}

// UpdateAccountKeepRev is UpdateAccount for a change that is no edit of the account's
// settings: the revision stays, so an edit that began before it is still current.
func (s *Store) UpdateAccountKeepRev(ctx context.Context, alias string, rev int64, audit AuditEntry, apply func(row *AccountRow)) (AccountRow, error) {
	var out AccountRow
	err := s.transact(ctx, func(tx *sql.Tx) error {
		before, err := accountRow(ctx, tx, alias)
		if err != nil {
			return err
		}
		if before.Rev != rev {
			return ErrRevConflict
		}
		after := before
		apply(&after)
		changes := ChangedFields(before, after)
		out = after
		if len(changes) == 0 && before.Verify.At.Equal(after.Verify.At) {
			return nil
		}
		now := s.now()
		after.UpdatedAt = fromMillis(now)
		out = after
		if err := writeRow(ctx, tx, after); err != nil {
			return err
		}
		return insertAudit(ctx, tx, now, audit, alias, changes)
	})
	return out, err
}

// insertAudit writes one audit row.
func insertAudit(ctx context.Context, tx *sql.Tx, now int64, audit AuditEntry, alias string, changes map[string]FieldChange) error {
	data, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("audit: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO account_audit (ts, source_ip, action, alias, changes, source_kind, source_thread, source_ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		now, audit.SourceIP, audit.Action, alias, string(data), audit.Source.Kind, audit.Source.Thread, audit.Source.TS); err != nil {
		return fmt.Errorf("audit: %v", err)
	}
	return nil
}

// AccountAudit returns the audit rows of account alias, oldest first.
func (s *Store) AccountAudit(ctx context.Context, alias string) ([]AuditRow, error) {
	rows, _, err := s.AccountAuditLast(ctx, alias, -1)
	return rows, err
}

// AccountAuditLast returns the last limit audit rows of account alias, oldest first, and
// whether older ones were left out; a limit below 0 returns all of them.
func (s *Store) AccountAuditLast(ctx context.Context, alias string, limit int) ([]AuditRow, bool, error) {
	result, err := s.db.QueryContext(ctx, `SELECT ts, source_ip, action, alias, changes, source_kind, source_thread, source_ts FROM account_audit
		WHERE alias = ? ORDER BY id DESC LIMIT ?`, alias, limit+boolToInt(limit >= 0))
	if err != nil {
		return nil, false, fmt.Errorf("read audit: %v", err)
	}
	defer func() { _ = result.Close() }()
	var rows []AuditRow
	for result.Next() {
		var row AuditRow
		var ts int64
		var changes string
		if err := result.Scan(&ts, &row.SourceIP, &row.Action, &row.Alias, &changes, &row.Source.Kind, &row.Source.Thread, &row.Source.TS); err != nil {
			return nil, false, fmt.Errorf("read audit: %v", err)
		}
		row.Time = fromMillis(ts)
		if err := json.Unmarshal([]byte(changes), &row.Changes); err != nil {
			return nil, false, fmt.Errorf("read audit: %v", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, false, fmt.Errorf("read audit: %v", err)
	}
	truncated := limit >= 0 && len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	slices.Reverse(rows)
	return rows, truncated, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PlanQuotaUnit returns the quota unit of plan at time at, ErrNoPlan when the plan has
// none then.
func (s *Store) PlanQuotaUnit(ctx context.Context, plan string, at time.Time) (float64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return planUnitAt(ctx, tx, plan, at.UnixMilli())
}

// AccountCreated returns the audit row that records the creation of account alias, nil
// for an account that was imported or whose row is gone.
func (s *Store) AccountCreated(ctx context.Context, alias string) (*AuditRow, error) {
	var row AuditRow
	var ts int64
	var changes string
	err := s.db.QueryRowContext(ctx, `SELECT ts, source_ip, action, alias, changes, source_kind, source_thread, source_ts FROM account_audit
		WHERE alias = ? AND action = 'account-create' ORDER BY id LIMIT 1`, alias).
		Scan(&ts, &row.SourceIP, &row.Action, &row.Alias, &changes, &row.Source.Kind, &row.Source.Thread, &row.Source.TS)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read audit: %v", err)
	}
	row.Time = fromMillis(ts)
	if err := json.Unmarshal([]byte(changes), &row.Changes); err != nil {
		return nil, fmt.Errorf("read audit: %v", err)
	}
	return &row, nil
}

// RecordAudit writes one audit row for a change made outside the accounts table, such as
// a plan change, with the old and new values of what it changed.
func (s *Store) RecordAudit(ctx context.Context, alias string, audit AuditEntry, changes map[string]FieldChange) error {
	return s.transact(ctx, func(tx *sql.Tx) error {
		return insertAudit(ctx, tx, s.now(), audit, alias, changes)
	})
}
