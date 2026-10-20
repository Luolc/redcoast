package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Traffic is one row of the traffic table: an inference request or a CONNECT tunnel
// as the gateway carried it. Bodies and credentials never appear in it.
type Traffic struct {
	At            time.Time
	SessionID     string // empty when the request carried no recognized session
	Alias         string // the account the request was carried on; empty when none was
	Kind          string // inference or connect
	Host          string
	Port          int
	Route         string // a CONNECT's: direct or account; empty for inference and an invalid target
	BytesUp       int64
	BytesDown     int64
	Duration      time.Duration
	Result        string
	Status        int    // the upstream's or exit's HTTP status; 0 when there was none
	ClaudeSession string // x-claude-code-session-id of an inference request, if any
	ClaudeAgent   string // x-claude-code-agent-id of an inference request, if any
	Request       string // method and path of a request the reverse proxy refused to forward; empty otherwise
	Failure       string // why the upstream could not be reached when the gateway answered in its place; empty otherwise
	Usage         *Usage // what the response of an inference request reported of its model and tokens; nil when nothing
}

// Usage is the model and token counts an upstream response reported, as the provider
// counts them: Claude's input tokens leave out the ones read from or written to its
// prompt cache, which it counts apart; Codex counts no cache tokens apart, and they
// stay zero.
type Usage struct {
	Model         string // empty when the response named none
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
}

// RecordTraffic appends t to the traffic table.
func (s *Store) RecordTraffic(ctx context.Context, t Traffic) error {
	var model sql.NullString
	var tokens [4]sql.NullInt64
	if u := t.Usage; u != nil {
		model = nullable(u.Model)
		for i, count := range []int64{u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheCreation} {
			tokens[i] = sql.NullInt64{Int64: count, Valid: true}
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO traffic (ts, session_id, alias, kind, host, port, bytes_up, bytes_down, duration_ms, result, status, claude_session_id, claude_agent_id, route, request,
			model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, failure)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.At.UnixMilli(), nullable(t.SessionID), nullable(t.Alias), t.Kind, t.Host, t.Port, t.BytesUp, t.BytesDown, t.Duration.Milliseconds(),
		t.Result, nullableInt(t.Status), nullable(t.ClaudeSession), nullable(t.ClaudeAgent), nullable(t.Route), nullable(t.Request),
		model, tokens[0], tokens[1], tokens[2], tokens[3], nullable(t.Failure))
	if err != nil {
		return fmt.Errorf("record traffic: %v", err)
	}
	return nil
}

// nullable stores an empty string as NULL.
func nullable(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// nullableInt stores zero as NULL.
func nullableInt(value int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(value), Valid: value != 0}
}
