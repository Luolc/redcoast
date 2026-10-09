package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxRecords bounds the sessions one Sessions call returns.
const MaxRecords = 500

// Query selects sessions by when they started: Since <= start < Until. A
// non-empty CwdPrefix keeps the sessions whose launch_meta cwd starts with it, as a
// plain string prefix; a non-empty Machine keeps that client machine's. EndedOnly
// leaves out the live ones.
type Query struct {
	Since, Until time.Time
	CwdPrefix    string
	Machine      string
	EndedOnly    bool
}

// Record is one session for the operator. The credential and its hash are
// never here. Alias is the account the session was last bound to.
type Record struct {
	ID          string     `json:"id"`
	Machine     string     `json:"machine"`
	Cwd         *string    `json:"cwd"`          // null when launch_meta has no string cwd
	LauncherPID *int64     `json:"launcher_pid"` // null when launch_meta has no integer launcher_pid
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"` // null while live
	EndReason   *string    `json:"end_reason"`
	Alias       *string    `json:"alias"` // null when never bound
}

// List is the answer of Sessions: newest first, at most MaxRecords;
// Truncated reports that more sessions matched.
type List struct {
	Sessions  []Record `json:"sessions"`
	Truncated bool     `json:"truncated"`
}

// Sessions lists the sessions q selects. It reads outside any transaction, so it
// never takes the write lock.
func (s *Store) Sessions(ctx context.Context, q Query) (List, error) {
	if !q.Since.Before(q.Until) {
		return List{}, errors.New("since must be before until")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.client_machine,
			CASE WHEN json_type(s.launch_meta, '$.cwd') = 'text' THEN json_extract(s.launch_meta, '$.cwd') END,
			CASE WHEN json_type(s.launch_meta, '$.launcher_pid') = 'integer' THEN json_extract(s.launch_meta, '$.launcher_pid') END,
			s.created_at, s.ended_at, s.end_reason, b.alias
		FROM sessions s LEFT JOIN bindings b ON b.session_id = s.id
		WHERE s.created_at >= ? AND s.created_at < ?
			AND (? = '' OR substr(json_extract(s.launch_meta, '$.cwd'), 1, length(?)) = ?)
			AND (? = '' OR s.client_machine = ?)
			AND (NOT ? OR s.ended_at IS NOT NULL)
		ORDER BY s.created_at DESC, s.id
		LIMIT ?`,
		q.Since.UnixMilli(), q.Until.UnixMilli(), q.CwdPrefix, q.CwdPrefix, q.CwdPrefix, q.Machine, q.Machine, q.EndedOnly, MaxRecords+1)
	if err != nil {
		return List{}, fmt.Errorf("list sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := List{Sessions: []Record{}}
	for rows.Next() {
		var r Record
		var cwd, reason, alias sql.Null[string]
		var pid sql.Null[int64]
		var ended sql.NullInt64
		var started int64
		if err := rows.Scan(&r.ID, &r.Machine, &cwd, &pid, &started, &ended, &reason, &alias); err != nil {
			return List{}, fmt.Errorf("list sessions: %v", err)
		}
		if len(out.Sessions) == MaxRecords {
			out.Truncated = true
			break
		}
		r.Cwd, r.LauncherPID, r.EndReason, r.Alias = orNil(cwd), orNil(pid), orNil(reason), orNil(alias)
		r.StartedAt = fromMillis(started)
		if ended.Valid {
			at := fromMillis(ended.Int64)
			r.EndedAt = &at
		}
		out.Sessions = append(out.Sessions, r)
	}
	if err := rows.Err(); err != nil {
		return List{}, fmt.Errorf("list sessions: %v", err)
	}
	return out, nil
}

// orNil returns a pointer to v's value, or nil when v is NULL.
func orNil[T any](v sql.Null[T]) *T {
	if !v.Valid {
		return nil
	}
	return &v.V
}
