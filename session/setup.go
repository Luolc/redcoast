package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The states of an account's setup task.
const (
	SetupNotStarted = "not_started"
	SetupRequested  = "requested"
	SetupRunning    = "running"
	SetupNeedsHuman = "needs_human"
	SetupDone       = "done"
	SetupFailed     = "failed"
)

// Timeouts of a running task.
const (
	SetupReportTimeout   = 30 * time.Minute
	SetupRunTimeout      = 60 * time.Minute
	SetupHandoverTimeout = 5 * time.Minute
)

// SetupTokenYears is how many calendar years after a write the gateway dates the token's
// expiry in the secret store item.
const SetupTokenYears = 1

// Reasons the store itself gives for a task it ends.
const (
	SetupReasonTimeout     = "timeout"
	SetupReasonStatus      = "account_status"
	SetupReasonDirect      = "direct_account"
	SetupReasonInterrupted = "interrupted"
)

// The wait for a person at a captcha page: a login program may ask for it once per task
// and for at most SetupMaxWait from the report.
const (
	SetupStepCaptcha = "awaiting_captcha"
	SetupMaxWait     = 20 * time.Minute
)

// Errors of the setup tasks.
var (
	ErrSetupSourceMismatch = errors.New("the task came from another source")
	ErrSetupTaskNotFound   = errors.New("no such task")
	ErrSetupNotSlack       = errors.New("the task was not requested from Slack")
	ErrSetupNotRequested   = errors.New("the task is not waiting to be handed out")
	ErrSetupWaitUntil      = errors.New("wait_until is not allowed")
	ErrSetupOpen           = errors.New("the account has a requested or running task")
	ErrSetupStatus         = errors.New("the account is neither testing nor active")
	ErrSetupDirect         = errors.New("the account has no proxy exit")
	ErrSetupNoOpenTask     = errors.New("no running task for the account")
	ErrSetupIDsMismatch    = errors.New("request_id or run_id does not match the task")
)

// schemaV11 adds the setup tasks: one row per account holding its latest task and the
// time its token was set. A row holds states, IDs, codes and times, never a token.
const schemaV11 = `
CREATE TABLE account_setup (
	alias TEXT PRIMARY KEY,
	state TEXT NOT NULL CHECK (state IN ('not_started', 'requested', 'running', 'needs_human', 'done', 'failed')),
	request_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	step TEXT NOT NULL,
	reason TEXT NOT NULL,
	action TEXT NOT NULL,
	requested_at INTEGER,
	started_at INTEGER,
	reported_at INTEGER,
	claimed_at INTEGER,
	updated_at INTEGER,
	token_set_at INTEGER
) STRICT;
`

// schemaV12 records where each audited write and each setup task came from, and the
// wait a login program asked for at a captcha page. The columns only add to a row; the
// rows already there are the dashboard's.
const schemaV12 = `
ALTER TABLE account_audit ADD COLUMN source_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE account_audit ADD COLUMN source_thread TEXT NOT NULL DEFAULT '';
ALTER TABLE account_audit ADD COLUMN source_ts TEXT NOT NULL DEFAULT '';
ALTER TABLE account_setup ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'dashboard';
ALTER TABLE account_setup ADD COLUMN source_thread TEXT NOT NULL DEFAULT '';
ALTER TABLE account_setup ADD COLUMN source_ts TEXT NOT NULL DEFAULT '';
ALTER TABLE account_setup ADD COLUMN wait_until INTEGER;
ALTER TABLE account_setup ADD COLUMN wait_until_set INTEGER NOT NULL DEFAULT 0;
`

// migrateToV12 adds the source and wait columns.
func migrateToV12(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV12); err != nil {
		return fmt.Errorf("session store migration v12: %v", err)
	}
	return nil
}

// migrateToV11 creates the setup table.
func migrateToV11(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaV11); err != nil {
		return fmt.Errorf("session store migration v11: %v", err)
	}
	return nil
}

// SetupRow is the latest setup task of an account and when its token was set. An empty
// string and a zero time are a field the task does not have yet.
type SetupRow struct {
	Alias     string
	State     string
	RequestID string
	RunID     string
	Step      string
	Reason    string
	Action    string

	RequestedAt time.Time
	StartedAt   time.Time // when a login machine took it
	ReportedAt  time.Time // the last report, or StartedAt before the first
	ClaimedAt   time.Time // when its handover began
	UpdatedAt   time.Time

	// TokenSetAt is when the gateway wrote the token; zero for a token it did not write.
	TokenSetAt time.Time

	// Source is where the request came from: the dashboard (Kind SourceDashboard, no
	// thread) or a Slack message.
	Source Source
	// WaitUntil is when a login program said it would be done waiting for a person at a
	// captcha page, zero when it is not waiting; WaitUntilSet stays true once a task has
	// set it, since a task sets it once.
	WaitUntil    time.Time
	WaitUntilSet bool
}

// SetupTask is a task handed to a login machine with the account it is for.
type SetupTask struct {
	SetupRow
	Account AccountRow
}

// setupColumns are the columns of a SetupRow.
const setupColumns = `alias, state, request_id, run_id, step, reason, action, requested_at, started_at, reported_at, claimed_at, updated_at, token_set_at, source_kind, source_thread, source_ts, wait_until, wait_until_set`

func scanSetup(r scanner) (SetupRow, error) {
	var row SetupRow
	var requested, started, reported, claimed, updated, tokenSet, wait sql.NullInt64
	if err := r.Scan(&row.Alias, &row.State, &row.RequestID, &row.RunID, &row.Step, &row.Reason, &row.Action,
		&requested, &started, &reported, &claimed, &updated, &tokenSet,
		&row.Source.Kind, &row.Source.Thread, &row.Source.TS, &wait, &row.WaitUntilSet); err != nil {
		return SetupRow{}, err
	}
	for _, t := range []struct {
		from sql.NullInt64
		to   *time.Time
	}{{requested, &row.RequestedAt}, {started, &row.StartedAt}, {reported, &row.ReportedAt}, {claimed, &row.ClaimedAt}, {updated, &row.UpdatedAt}, {tokenSet, &row.TokenSetAt}, {wait, &row.WaitUntil}} {
		if t.from.Valid {
			*t.to = fromMillis(t.from.Int64)
		}
	}
	return row, nil
}

// setupRow reads the row of alias inside tx; a missing row is the not_started row.
func setupRow(ctx context.Context, tx *sql.Tx, alias string) (SetupRow, error) {
	row, err := scanSetup(tx.QueryRowContext(ctx, "SELECT "+setupColumns+" FROM account_setup WHERE alias = ?", alias))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SetupRow{Alias: alias, State: SetupNotStarted}, nil
	case err != nil:
		return SetupRow{}, fmt.Errorf("read setup: %v", err)
	}
	return row, nil
}

// putSetup writes row, replacing the account's.
func putSetup(ctx context.Context, tx *sql.Tx, row SetupRow) error {
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO account_setup (`+setupColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Alias, row.State, row.RequestID, row.RunID, row.Step, row.Reason, row.Action,
		nullableTime(row.RequestedAt), nullableTime(row.StartedAt), nullableTime(row.ReportedAt), nullableTime(row.ClaimedAt), nullableTime(row.UpdatedAt),
		nullableTime(row.TokenSetAt), row.Source.Kind, row.Source.Thread, row.Source.TS, nullableTime(row.WaitUntil), row.WaitUntilSet)
	if err != nil {
		return fmt.Errorf("write setup: %v", err)
	}
	return nil
}

// end moves row to a closed state at now, keeping its step.
func (r *SetupRow) end(state, reason string, now time.Time) {
	r.State, r.Reason, r.UpdatedAt = state, reason, now
}

// readSetups reads the rows a query selects inside tx.
func readSetups(ctx context.Context, tx *sql.Tx, query string) ([]SetupRow, error) {
	result, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read setup: %v", err)
	}
	defer func() { _ = result.Close() }()
	var rows []SetupRow
	for result.Next() {
		row, err := scanSetup(result)
		if err != nil {
			return nil, fmt.Errorf("read setup: %v", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read setup: %v", err)
	}
	return rows, nil
}

// ineligible returns why an account cannot have a setup, as the fixed code a task ends
// with, or "" when it can: a setup is for an account that is testing or active and has a
// proxy exit, neither the gateway's own address (direct) nor no exit at all (no proxy
// host).
func ineligible(status string, direct bool, proxyHost string) string {
	switch {
	case status != "testing" && status != "active":
		return SetupReasonStatus
	case direct || proxyHost == "":
		return SetupReasonDirect
	}
	return ""
}

// expiry says how a requested or running task ends at now: the state and reason, or
// empty strings when it stands. A requested task also ends with an account that cannot
// have a setup any more, ineligible being the code for it.
func (r SetupRow) expiry(now time.Time, ineligible string) (state, reason string) {
	switch {
	case r.State == SetupRunning && !r.ClaimedAt.IsZero() && !now.Before(r.ClaimedAt.Add(SetupHandoverTimeout)):
		return SetupFailed, SetupReasonTimeout
	case r.State == SetupRunning && r.ClaimedAt.IsZero() && (!now.Before(r.ReportedAt.Add(SetupReportTimeout)) || !now.Before(r.StartedAt.Add(SetupRunTimeout))):
		return SetupFailed, SetupReasonTimeout
	case (r.State == SetupRequested || r.State == SetupRunning && r.ClaimedAt.IsZero()) && ineligible != "":
		return SetupFailed, ineligible
	}
	return "", ""
}

// expireSetups ends, inside tx, a running task that has outlived its time and a
// requested or running task, not yet claimed by a handover, whose account cannot have a
// setup any more.
func expireSetups(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := readSetups(ctx, tx, "SELECT "+setupColumns+" FROM account_setup WHERE state IN ('requested', 'running')")
	if err != nil {
		return err
	}
	for _, row := range rows {
		why := ""
		var status string
		var direct bool
		var host string
		switch err := tx.QueryRowContext(ctx, "SELECT status, proxy_direct, proxy_host FROM accounts WHERE alias = ?", row.Alias).Scan(&status, &direct, &host); {
		case err == nil:
			why = ineligible(status, direct, host)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("read account: %v", err)
		}
		state, reason := row.expiry(now, why)
		if state == "" {
			continue
		}
		row.end(state, reason, now)
		if err := putSetup(ctx, tx, row); err != nil {
			return err
		}
	}
	return nil
}

// ExpireSetups applies the timeouts now.
func (s *Store) ExpireSetups(ctx context.Context) error {
	return s.transact(ctx, func(tx *sql.Tx) error { return expireSetups(ctx, tx, fromMillis(s.now())) })
}

// InterruptSetups ends the tasks whose handover was under way when the process that
// ran it stopped: they are failed with interrupted. It returns their number.
func (s *Store) InterruptSetups(ctx context.Context) (int, error) {
	n := 0
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := fromMillis(s.now())
		result, err := tx.ExecContext(ctx, `UPDATE account_setup SET state = 'failed', reason = ?, updated_at = ? WHERE state = 'running' AND claimed_at IS NOT NULL`, SetupReasonInterrupted, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("write setup: %v", err)
		}
		count, _ := result.RowsAffected()
		n = int(count)
		return nil
	})
	return n, err
}

// RequestSetup records a task for account alias as requested. With a rev the account
// must be at it. It fails with ErrAccountNotFound, ErrRevConflict, ErrSetupStatus and
// ErrSetupDirect (an account that cannot have a setup) and ErrSetupOpen, in that order.
// It returns the new row and the state the account's setup was in before.
func (s *Store) RequestSetup(ctx context.Context, alias, action string, rev *int64, requestID string, source Source) (SetupRow, string, error) {
	// The timeouts are applied in a transaction of their own, so that they stand when the
	// operation that follows is refused.
	if err := s.ExpireSetups(ctx); err != nil {
		return SetupRow{}, "", err
	}
	var out SetupRow
	var before string
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := fromMillis(s.now())
		account, err := accountRow(ctx, tx, alias)
		if err != nil {
			return err
		}
		if rev != nil && account.Rev != *rev {
			return ErrRevConflict
		}
		switch ineligible(account.Status, account.Direct, account.ProxyHost) {
		case SetupReasonStatus:
			return ErrSetupStatus
		case SetupReasonDirect:
			return ErrSetupDirect
		}
		row, err := setupRow(ctx, tx, alias)
		if err != nil {
			return err
		}
		if row.State == SetupRequested || row.State == SetupRunning {
			return ErrSetupOpen
		}
		before = row.State
		if source.Kind == "" {
			source.Kind = SourceDashboard
		}
		out = SetupRow{Alias: alias, State: SetupRequested, RequestID: requestID, Action: action,
			RequestedAt: now, UpdatedAt: now, TokenSetAt: row.TokenSetAt, Source: source}
		return putSetup(ctx, tx, out)
	})
	return out, before, err
}

// NextSetup hands the next requested task that the dashboard asked for out, or returns
// nil: they go in the order requested, and none is handed out while another is running.
// A task requested from Slack is never handed out here. The task becomes running with no
// step, and the time is when the run's timeouts start.
func (s *Store) NextSetup(ctx context.Context) (*SetupTask, error) {
	return s.nextSetup(ctx, "")
}

// NextSetupByID hands out the task with this request ID, which must be the latest task
// of its account, requested from Slack and waiting; nil when it is but another task is
// running. It fails with ErrSetupTaskNotFound, ErrSetupNotSlack and
// ErrSetupNotRequested, in that order.
func (s *Store) NextSetupByID(ctx context.Context, requestID string) (*SetupTask, error) {
	return s.nextSetup(ctx, requestID)
}

func (s *Store) nextSetup(ctx context.Context, requestID string) (*SetupTask, error) {
	// The timeouts are applied in a transaction of their own, so that they stand when the
	// operation that follows is refused.
	if err := s.ExpireSetups(ctx); err != nil {
		return nil, err
	}
	var out *SetupTask
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := fromMillis(s.now())
		row, err := pickSetup(ctx, tx, requestID)
		if err != nil || row == nil {
			return err
		}
		var running int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_setup WHERE state = 'running'").Scan(&running); err != nil {
			return fmt.Errorf("read setup: %v", err)
		}
		if running > 0 {
			return nil
		}
		account, err := accountRow(ctx, tx, row.Alias)
		if err != nil {
			return err
		}
		row.State, row.StartedAt, row.ReportedAt, row.UpdatedAt = SetupRunning, now, now, now
		if err := putSetup(ctx, tx, *row); err != nil {
			return err
		}
		out = &SetupTask{SetupRow: *row, Account: account}
		return nil
	})
	return out, err
}

// pickSetup selects the task nextSetup would hand out: the oldest requested task the
// dashboard asked for, or, with a request ID, that task if it is the account's latest,
// from Slack and requested. A nil row with a nil error is no task to hand out.
func pickSetup(ctx context.Context, tx *sql.Tx, requestID string) (*SetupRow, error) {
	if requestID == "" {
		row, err := scanSetup(tx.QueryRowContext(ctx, `SELECT `+setupColumns+` FROM account_setup WHERE state = 'requested' AND source_kind = ?
			ORDER BY requested_at, alias LIMIT 1`, SourceDashboard))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read setup: %v", err)
		}
		return &row, nil
	}
	row, err := scanSetup(tx.QueryRowContext(ctx, `SELECT `+setupColumns+` FROM account_setup WHERE request_id = ?`, requestID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrSetupTaskNotFound
	case err != nil:
		return nil, fmt.Errorf("read setup: %v", err)
	case row.Source.Kind != SourceSlack:
		return nil, ErrSetupNotSlack
	case row.State != SetupRequested:
		return nil, ErrSetupNotRequested
	}
	return &row, nil
}

// runningUnclaimed reads the row of alias inside tx and checks it is a running task
// that no handover has claimed, with these IDs. runID may be empty only when the
// caller allows the first report to set it.
func runningTask(ctx context.Context, tx *sql.Tx, alias, requestID string) (SetupRow, error) {
	row, err := setupRow(ctx, tx, alias)
	if err != nil {
		return SetupRow{}, err
	}
	if row.State != SetupRunning {
		return SetupRow{}, ErrSetupNoOpenTask
	}
	if row.RequestID != requestID {
		return SetupRow{}, ErrSetupIDsMismatch
	}
	return row, nil
}

// ReportSetup records one step a login machine reports for the running task: the first
// report sets the run ID, which every later one carries. A task that a handover
// claimed takes no report, and neither does one that came from another source than
// sourceKind (ErrSetupSourceMismatch, after the IDs are checked). A wait until is
// allowed only with the running state and the step SetupStepCaptcha, once per task, and
// within SetupMaxWait after now (ErrSetupWaitUntil, nothing written); a report with a
// different state or step clears it, and one without it leaves it.
func (s *Store) ReportSetup(ctx context.Context, alias, requestID, runID, sourceKind, state, step, reason string, waitUntil *time.Time) (SetupRow, error) {
	// The timeouts are applied in a transaction of their own, so that they stand when the
	// operation that follows is refused.
	if err := s.ExpireSetups(ctx); err != nil {
		return SetupRow{}, err
	}
	var out SetupRow
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := fromMillis(s.now())
		row, err := runningTask(ctx, tx, alias, requestID)
		if err != nil {
			return err
		}
		if !row.ClaimedAt.IsZero() {
			return ErrSetupNoOpenTask
		}
		if row.RunID != "" && row.RunID != runID {
			return ErrSetupIDsMismatch
		}
		if row.Source.Kind != sourceKind {
			return ErrSetupSourceMismatch
		}
		switch {
		case waitUntil != nil:
			if state != SetupRunning || step != SetupStepCaptcha || row.WaitUntilSet || !waitUntil.After(now) || waitUntil.After(now.Add(SetupMaxWait)) {
				return ErrSetupWaitUntil
			}
			row.WaitUntil, row.WaitUntilSet = waitUntil.UTC(), true
		case state != SetupRunning || step != SetupStepCaptcha:
			row.WaitUntil = time.Time{}
		}
		row.RunID, row.Step, row.Reason, row.State = runID, step, reason, state
		row.ReportedAt, row.UpdatedAt = now, now
		out = row
		return putSetup(ctx, tx, row)
	})
	return out, err
}

// CheckHandover says whether a handover for these IDs would be taken now, without
// claiming it.
func (s *Store) CheckHandover(ctx context.Context, alias, requestID, runID, sourceKind string) error {
	// The timeouts are applied in a transaction of their own, so that they stand when the
	// operation that follows is refused.
	if err := s.ExpireSetups(ctx); err != nil {
		return err
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		row, err := handoverTask(ctx, tx, alias, requestID, runID)
		if err == nil && row.Source.Kind != sourceKind {
			err = ErrSetupSourceMismatch
		}
		return err
	})
}

// handoverTask is the running, unclaimed task of alias whose run ID a report has set,
// with these IDs.
func handoverTask(ctx context.Context, tx *sql.Tx, alias, requestID, runID string) (SetupRow, error) {
	row, err := runningTask(ctx, tx, alias, requestID)
	if err != nil {
		return SetupRow{}, err
	}
	if !row.ClaimedAt.IsZero() {
		return SetupRow{}, ErrSetupNoOpenTask
	}
	if row.RunID == "" || row.RunID != runID {
		return SetupRow{}, ErrSetupIDsMismatch
	}
	return row, nil
}

// ClaimHandover claims the task for a handover, at most once: from then on the task
// takes no other handover and no report, and the handover's own time limit applies. It
// returns the task with its account.
func (s *Store) ClaimHandover(ctx context.Context, alias, requestID, runID, sourceKind string) (SetupTask, error) {
	// The timeouts are applied in a transaction of their own, so that they stand when the
	// operation that follows is refused.
	if err := s.ExpireSetups(ctx); err != nil {
		return SetupTask{}, err
	}
	var out SetupTask
	err := s.transact(ctx, func(tx *sql.Tx) error {
		now := fromMillis(s.now())
		row, err := handoverTask(ctx, tx, alias, requestID, runID)
		if err != nil {
			return err
		}
		if row.Source.Kind != sourceKind {
			return ErrSetupSourceMismatch
		}
		account, err := accountRow(ctx, tx, alias)
		if err != nil {
			return err
		}
		row.ClaimedAt, row.UpdatedAt = now, now
		out = SetupTask{SetupRow: row, Account: account}
		return putSetup(ctx, tx, row)
	})
	return out, err
}

// claimedTask reads the claimed running task of alias with this request ID.
func claimedTask(ctx context.Context, tx *sql.Tx, alias, requestID string) (SetupRow, error) {
	row, err := runningTask(ctx, tx, alias, requestID)
	if err != nil {
		return SetupRow{}, err
	}
	if row.ClaimedAt.IsZero() {
		return SetupRow{}, ErrSetupNoOpenTask
	}
	return row, nil
}

// SetupStep records the step of a claimed handover.
func (s *Store) SetupStep(ctx context.Context, alias, requestID, step string) error {
	return s.transact(ctx, func(tx *sql.Tx) error {
		row, err := claimedTask(ctx, tx, alias, requestID)
		if err != nil {
			return err
		}
		row.Step, row.UpdatedAt = step, fromMillis(s.now())
		return putSetup(ctx, tx, row)
	})
}

// NoteTokenWritten records that the gateway wrote the token of a claimed handover now.
func (s *Store) NoteTokenWritten(ctx context.Context, alias, requestID string) error {
	return s.transact(ctx, func(tx *sql.Tx) error {
		row, err := claimedTask(ctx, tx, alias, requestID)
		if err != nil {
			return err
		}
		now := fromMillis(s.now())
		row.TokenSetAt, row.UpdatedAt = now, now
		return putSetup(ctx, tx, row)
	})
}

// FinishSetup ends a claimed handover as done or failed. A task that a timeout or a
// restart ended meanwhile is not changed: ErrSetupNoOpenTask.
func (s *Store) FinishSetup(ctx context.Context, alias, requestID, state, step, reason string) (SetupRow, error) {
	var out SetupRow
	err := s.transact(ctx, func(tx *sql.Tx) error {
		row, err := claimedTask(ctx, tx, alias, requestID)
		if err != nil {
			return err
		}
		row.Step = step
		row.end(state, reason, fromMillis(s.now()))
		out = row
		return putSetup(ctx, tx, row)
	})
	return out, err
}

// SetupRows returns the rows of the setup table by alias.
func (s *Store) SetupRows(ctx context.Context) (map[string]SetupRow, error) {
	result, err := s.db.QueryContext(ctx, "SELECT "+setupColumns+" FROM account_setup")
	if err != nil {
		return nil, fmt.Errorf("read setup: %v", err)
	}
	defer func() { _ = result.Close() }()
	rows := map[string]SetupRow{}
	for result.Next() {
		row, err := scanSetup(result)
		if err != nil {
			return nil, fmt.Errorf("read setup: %v", err)
		}
		rows[row.Alias] = row
	}
	return rows, result.Err()
}

// SetupRows returns the setup table through the read-only connection.
func (r *Reader) SetupRows(ctx context.Context) (map[string]SetupRow, error) {
	return r.store.SetupRows(ctx)
}
