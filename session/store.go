// Package session keeps the gateway's state in one SQLite file: sessions and their
// credentials, each session's binding to an account, the binding history, account
// pauses, the traffic table and the adjustable settings. It issues session credentials,
// evaluates the binding for every request and revokes sessions.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Defaults for the durations in Config.
const (
	DefaultLease      = time.Hour
	DefaultIdleExpiry = 7 * 24 * time.Hour
	// activeWindow is how recently a session must have sent a request to count as
	// active when accounts are compared.
	activeWindow = time.Hour
)

// Thresholds are the initial soft and hard quota thresholds written to the settings
// table; Bind reads the current values for every request.
const (
	DefaultSoft = "0.8"
	DefaultHard = "0.9"
)

// schemaVersion is stored in PRAGMA user_version. Each version's tables are created by
// migrate when the file's version is lower.
const schemaVersion = 6

// schema creates the tables of version 1. Timestamps are Unix milliseconds in UTC.
const schema = `
CREATE TABLE account_state (
	alias TEXT PRIMARY KEY,
	paused_until INTEGER,
	pause_reason TEXT,
	updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE quota_latest (
	alias TEXT NOT NULL,
	window TEXT NOT NULL CHECK (window IN ('5h', '7d')),
	utilization REAL NOT NULL,
	status TEXT,
	reset_at INTEGER,
	observed_at INTEGER NOT NULL,
	source TEXT NOT NULL,
	PRIMARY KEY (alias, window)
) STRICT;
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	client_machine TEXT NOT NULL,
	source_addr TEXT NOT NULL,
	launch_meta TEXT,
	created_at INTEGER NOT NULL,
	last_seen_at INTEGER,
	ended_at INTEGER,
	end_reason TEXT
) STRICT;
CREATE TABLE bindings (
	session_id TEXT PRIMARY KEY REFERENCES sessions(id),
	alias TEXT NOT NULL,
	bound_at INTEGER NOT NULL,
	lease_expires_at INTEGER NOT NULL,
	reason TEXT NOT NULL
) STRICT;
CREATE INDEX bindings_alias ON bindings(alias);
CREATE TABLE binding_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	alias TEXT NOT NULL,
	from_ts INTEGER NOT NULL,
	to_ts INTEGER,
	reason TEXT NOT NULL
) STRICT;
CREATE INDEX binding_history_session ON binding_history(session_id);
CREATE TABLE traffic (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	session_id TEXT,
	alias TEXT,
	kind TEXT NOT NULL CHECK (kind IN ('inference', 'connect')),
	host TEXT NOT NULL,
	port INTEGER NOT NULL,
	bytes_up INTEGER NOT NULL,
	bytes_down INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	result TEXT NOT NULL,
	status INTEGER,
	claude_session_id TEXT,
	claude_agent_id TEXT
) STRICT;
CREATE INDEX traffic_session_ts ON traffic(session_id, ts);
CREATE INDEX traffic_ts ON traffic(ts);
CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;
`

// schemaV2 adds the plan tables of version 2: the quota unit of each subscription plan
// over time, and each account's plan over time. An open-ended period has a NULL
// effective_to.
const schemaV2 = `
CREATE TABLE plans (
	plan TEXT NOT NULL,
	effective_from INTEGER NOT NULL,
	effective_to INTEGER,
	quota_unit REAL NOT NULL,
	monthly_usd REAL NOT NULL,
	PRIMARY KEY (plan, effective_from)
) STRICT;
CREATE TABLE plan_schedule (
	alias TEXT NOT NULL,
	effective_from INTEGER NOT NULL,
	effective_to INTEGER,
	plan TEXT NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('manual', 'auto')),
	note TEXT,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (alias, effective_from)
) STRICT;
`

// schemaV3 adds the machines of version 3: the client machines allowed to ask for
// sessions over the network, each with the SHA-256 of its credential and its session
// limit. A revoked machine keeps its row with revoked_at set.
const schemaV3 = `
CREATE TABLE machines (
	name TEXT PRIMARY KEY,
	cred_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER,
	max_sessions INTEGER NOT NULL
) STRICT;
`

// seedPlans are the initial quota units, in effect since the epoch: third-party tests
// put a plan's quota roughly in proportion to its monthly list price. Later changes go
// in as new periods, not edits.
var seedPlans = []PlanUnit{
	{Plan: "pro", QuotaUnit: 1, MonthlyUSD: 20, From: time.UnixMilli(0).UTC()},
	{Plan: "max-5x", QuotaUnit: 5, MonthlyUSD: 100, From: time.UnixMilli(0).UTC()},
	{Plan: "max-20x", QuotaUnit: 10, MonthlyUSD: 200, From: time.UnixMilli(0).UTC()},
}

// Config adjusts the store's durations and clock. Zero values take the defaults.
type Config struct {
	Lease      time.Duration    // how long a binding lasts before it is re-evaluated
	IdleExpiry time.Duration    // a session with no request for this long is ended
	Now        func() time.Time // nil uses time.Now; tests inject a clock
}

// Store is the open SQLite file. Its methods are safe for concurrent use.
type Store struct {
	db  *sql.DB
	cfg Config

	mu            sync.Mutex
	lastRetention RetentionRun
}

// Open opens or creates the SQLite file at path in WAL mode, creates the schema when
// the file is new and writes the default settings. The file is created with the
// process's umask; the directory holding it is the caller's to protect.
func Open(ctx context.Context, path string, cfg Config) (*Store, error) {
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.IdleExpiry <= 0 {
		cfg.IdleExpiry = DefaultIdleExpiry
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Every transaction takes the write lock when it begins, so concurrent requests
	// serialize on it instead of failing with SQLITE_BUSY after reading.
	query := url.Values{}
	query.Set("_txlock", "immediate")
	query.Set("_busy_timeout", "10000")
	query["_pragma"] = []string{"journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"}
	db, err := sql.Open("sqlite", fileURI(path, query))
	if err != nil {
		return nil, fmt.Errorf("open session store: %v", err)
	}
	s := &Store{db: db, cfg: cfg}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// fileURI returns the SQLite URI of the file at path with query. The path is escaped:
// a '#' or '?' in it (Go names a repeated subtest "<name>#01", which t.TempDir()
// carries) would otherwise end the path early and open another file.
func fileURI(path string, query url.Values) string {
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	if !filepath.IsAbs(path) {
		dsn.Opaque = (&url.URL{Path: path}).EscapedPath()
	}
	return dsn.String()
}

// Close closes the file. Transactions still running finish first.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close session store: %v", err)
	}
	return nil
}

// migrate brings the file to schemaVersion.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("session store migration: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("session store version: %v", err)
	}
	if version > schemaVersion {
		return errors.New("session store was written by a newer gateway")
	}
	if version == schemaVersion {
		return nil
	}
	for _, step := range []struct {
		version int
		apply   func(context.Context, *sql.Tx) error
	}{{1, s.migrateToV1}, {2, migrateToV2}, {3, migrateToV3}, {4, migrateToV4}, {5, migrateToV5}, {6, migrateToV6}} {
		if version < step.version {
			if err := step.apply(ctx, tx); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("session store version: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("session store migration: %v", err)
	}
	return nil
}

// migrateToV1 creates the version-1 tables and the default settings.
func (s *Store) migrateToV1(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("session store schema: %v", err)
	}
	now := s.now()
	for key, value := range map[string]string{"soft": DefaultSoft, "hard": DefaultHard} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)", key, value, now); err != nil {
			return fmt.Errorf("session store settings: %v", err)
		}
	}
	return nil
}

// migrateToV2 creates the plan tables and seeds the quota units.
func migrateToV2(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
		return fmt.Errorf("session store schema v2: %v", err)
	}
	for _, unit := range seedPlans {
		if err := insertPlanUnit(ctx, tx, unit); err != nil {
			return err
		}
	}
	return nil
}

// migrateToV3 creates the machines table.
func migrateToV3(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
		return fmt.Errorf("session store schema v3: %v", err)
	}
	return nil
}

// migrateToV4 rescales the stored quota readings. Gateways before version 4 read the
// utilization header as a percentage and stored a hundredth of it; the header is the
// fraction itself, so each stored reading is multiplied by 100. Every release deployed
// with a store parsed the header that way.
func migrateToV4(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "UPDATE quota_latest SET utilization = utilization * 100"); err != nil {
		return fmt.Errorf("session store migration v4: %v", err)
	}
	return nil
}

// migrateToV5 adds the route of each CONNECT row: direct or account. Before version 5
// every CONNECT with a valid target went through the account's exit, so those rows are
// account; rows with an invalid target have an empty host and stay NULL, as new ones do.
func migrateToV5(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		"ALTER TABLE traffic ADD COLUMN route TEXT CHECK (route IN ('direct', 'account'))",
		"UPDATE traffic SET route = 'account' WHERE kind = 'connect' AND host != ''",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("session store migration v5: %v", err)
		}
	}
	return nil
}

// migrateToV6 adds the method and path of each inference request the reverse proxy
// refused because they are not on its list. Older rows and other results stay NULL.
func migrateToV6(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "ALTER TABLE traffic ADD COLUMN request TEXT"); err != nil {
		return fmt.Errorf("session store migration v6: %v", err)
	}
	return nil
}

// now returns the clock's time in Unix milliseconds.
func (s *Store) now() int64 {
	return s.cfg.Now().UnixMilli()
}

// fromMillis converts a stored timestamp back to a time.
func fromMillis(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}
