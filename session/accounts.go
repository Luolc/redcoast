package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// accountsImportedKey is the settings row that records the first import of the
// inventory into the accounts table. Without it the table has never been initialized;
// with it the table is the source of the accounts and the inventory directory is not read.
const accountsImportedKey = "accounts_imported"

// schemaV9 adds the accounts table of version 9: the Claude accounts the gateway serves,
// one row per alias, with the references of their credentials and never a value. A
// row of status other than active, or of access other than gateway, is not served.
const schemaV9 = `
CREATE TABLE accounts (
	alias TEXT PRIMARY KEY,
	email TEXT NOT NULL,
	access TEXT NOT NULL CHECK (access IN ('direct', 'gateway')),
	status TEXT NOT NULL CHECK (status IN ('testing', 'active', 'paused', 'suspended', 'retired')),
	token_ref TEXT NOT NULL,
	proxy_direct INTEGER NOT NULL CHECK (proxy_direct IN (0, 1)),
	proxy_host TEXT NOT NULL,
	proxy_port INTEGER NOT NULL,
	expected_egress_ip TEXT NOT NULL,
	proxy_username_ref TEXT NOT NULL,
	proxy_password_ref TEXT NOT NULL,
	device_id TEXT NOT NULL,
	account_uuid TEXT NOT NULL,
	rev INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;
`

// AccountRow is one account of the accounts table. Its fields are the ones the inventory
// file carries that the gateway consumes; the *Ref fields are credential references
// (op://, env:// or file://), never credential values. An empty string or a zero
// port is a field the account does not have.
type AccountRow struct {
	Alias       string    `json:"alias"`
	Email       string    `json:"email"`
	Access      string    `json:"access"` // direct or gateway
	Status      string    `json:"status"` // testing, active, paused, suspended or retired
	TokenRef    string    `json:"token_ref"`
	Direct      bool      `json:"direct"` // the proxy block's direct: the gateway's own address is the exit
	ProxyHost   string    `json:"proxy_host"`
	ProxyPort   int       `json:"proxy_port"`
	ExpectedIP  string    `json:"expected_egress_ip"`
	UsernameRef string    `json:"proxy_username_ref"`
	PasswordRef string    `json:"proxy_password_ref"`
	DeviceID    string    `json:"device_id"`
	AccountUUID string    `json:"account_uuid"`
	Rev         int64     `json:"rev"` // starts at 1; the version an edit is made against
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	Verify Verification `json:"verify"` // the last run of the three checks, voided by a change of exit or reference
}

// accountColumns are the columns of an AccountRow, in the order scanRow reads them.
const accountColumns = `alias, email, access, status, token_ref, proxy_direct, proxy_host, proxy_port, expected_egress_ip,
	proxy_username_ref, proxy_password_ref, device_id, account_uuid, rev, created_at, updated_at, token_ok, egress_ok, probe_ok, verified_at`

// scanner is a row of a query, *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanRow reads one AccountRow selected by accountColumns.
func scanRow(r scanner) (AccountRow, error) {
	var row AccountRow
	var created, updated int64
	var tokenOK, egressOK, probeOK sql.NullBool
	var verified sql.NullInt64
	if err := r.Scan(&row.Alias, &row.Email, &row.Access, &row.Status, &row.TokenRef, &row.Direct, &row.ProxyHost, &row.ProxyPort,
		&row.ExpectedIP, &row.UsernameRef, &row.PasswordRef, &row.DeviceID, &row.AccountUUID, &row.Rev, &created, &updated,
		&tokenOK, &egressOK, &probeOK, &verified); err != nil {
		return AccountRow{}, err
	}
	row.CreatedAt, row.UpdatedAt = fromMillis(created), fromMillis(updated)
	row.Verify = Verification{TokenOK: boolPointer(tokenOK), EgressOK: boolPointer(egressOK), ProbeOK: boolPointer(probeOK)}
	if verified.Valid {
		row.Verify.At = fromMillis(verified.Int64)
	}
	return row, nil
}

// boolPointer returns nil for NULL.
func boolPointer(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	return &b.Bool
}

// accountRow reads account alias inside tx, ErrAccountNotFound when there is none.
func accountRow(ctx context.Context, tx *sql.Tx, alias string) (AccountRow, error) {
	row, err := scanRow(tx.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE alias = ?", alias))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccountRow{}, ErrAccountNotFound
	case err != nil:
		return AccountRow{}, fmt.Errorf("read account: %v", err)
	}
	return row, nil
}

// migrateToV9 creates the accounts table. It starts empty and uninitialized: the gateway
// imports the inventory once, at its first start with this version.
func migrateToV9(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
		return fmt.Errorf("session store migration v9: %v", err)
	}
	return nil
}

// ImportAccounts writes rows into the accounts table and the marker that the table is
// initialized, in one transaction, and reports true. When the marker is already there it
// writes nothing and reports false: the table, not the inventory, is the source.
// Nothing is written when a row is refused.
func (s *Store) ImportAccounts(ctx context.Context, rows []AccountRow) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("import accounts: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var marker string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", accountsImportedKey).Scan(&marker); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("import accounts: %v", err)
	}
	now := s.now()
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (alias, email, access, status, token_ref, proxy_direct, proxy_host, proxy_port,
			expected_egress_ip, proxy_username_ref, proxy_password_ref, device_id, account_uuid, rev, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			row.Alias, row.Email, row.Access, row.Status, row.TokenRef, row.Direct, row.ProxyHost, row.ProxyPort,
			row.ExpectedIP, row.UsernameRef, row.PasswordRef, row.DeviceID, row.AccountUUID, now, now); err != nil {
			return false, fmt.Errorf("import accounts: %s: %v", row.Alias, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)", accountsImportedKey, fmt.Sprintf("%d accounts", len(rows)), now); err != nil {
		return false, fmt.Errorf("import accounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import accounts: %v", err)
	}
	return true, nil
}

// Accounts returns the rows of the accounts table sorted by alias, and whether the
// table is initialized, that is whether the inventory was imported.
func (s *Store) Accounts(ctx context.Context) (rows []AccountRow, initialized bool, err error) {
	var marker string
	switch err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", accountsImportedKey).Scan(&marker); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read accounts: %v", err)
	}
	result, err := s.db.QueryContext(ctx, "SELECT "+accountColumns+" FROM accounts ORDER BY alias")
	if err != nil {
		return nil, false, fmt.Errorf("read accounts: %v", err)
	}
	defer func() { _ = result.Close() }()
	for result.Next() {
		row, err := scanRow(result)
		if err != nil {
			return nil, false, fmt.Errorf("read accounts: %v", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, false, fmt.Errorf("read accounts: %v", err)
	}
	return rows, true, nil
}

// Accounts returns the accounts table through the read-only connection, as
// Store.Accounts does.
func (r *Reader) Accounts(ctx context.Context) ([]AccountRow, bool, error) {
	return r.store.Accounts(ctx)
}

// Account returns the row of account alias, ErrAccountNotFound when there is none.
func (s *Store) Account(ctx context.Context, alias string) (AccountRow, error) {
	row, err := scanRow(s.db.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE alias = ?", alias))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccountRow{}, ErrAccountNotFound
	case err != nil:
		return AccountRow{}, fmt.Errorf("read account: %v", err)
	}
	return row, nil
}
