package session

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestRecordTraffic writes an inference row and a CONNECT row and reads both back
// from the file, including the NULLs of the row without a session.
func TestRecordTraffic(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	rows := []Traffic{
		{At: c.Now(), SessionID: "s1", Alias: "sample-a", Kind: "inference", Host: "api.anthropic.com", Port: 443, BytesUp: 10, BytesDown: 20, Duration: 1500 * time.Millisecond, Result: "ok", Status: 200, ClaudeSession: "11111111-1111-4111-8111-111111111111", ClaudeAgent: "agent-1"},
		{At: c.Now(), Kind: "connect", Host: "example.invalid", Port: 443, Route: "account", Result: "auth_failed"},
	}
	for _, row := range rows {
		if err := store.RecordTraffic(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordTraffic(ctx, Traffic{Kind: "other", Host: "h", Result: "x"}); err == nil {
		t.Fatal("kind outside the schema's CHECK was accepted")
	}
	type stored struct {
		session, alias, kind, result, claudeSession, claudeAgent, route *string
		status                                                          *int64
		durationMs, bytesUp, bytesDown                                  int64
	}
	var got []stored
	result, err := store.db.Query("SELECT session_id, alias, kind, result, claude_session_id, claude_agent_id, route, status, duration_ms, bytes_up, bytes_down FROM traffic ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Close() }()
	for result.Next() {
		var row stored
		if err := result.Scan(&row.session, &row.alias, &row.kind, &row.result, &row.claudeSession, &row.claudeAgent, &row.route, &row.status, &row.durationMs, &row.bytesUp, &row.bytesDown); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := result.Err(); err != nil || len(got) != 2 {
		t.Fatalf("rows=%d err=%v", len(got), err)
	}
	first, second := got[0], got[1]
	if *first.session != "s1" || *first.alias != "sample-a" || *first.kind != "inference" || *first.result != "ok" || *first.claudeSession != rows[0].ClaudeSession || *first.claudeAgent != "agent-1" || first.route != nil || *first.status != 200 || first.durationMs != 1500 || first.bytesUp != 10 || first.bytesDown != 20 {
		t.Fatalf("inference row changed: %+v", first)
	}
	if second.session != nil || second.alias != nil || second.status != nil || second.claudeSession != nil || second.claudeAgent != nil || *second.kind != "connect" || *second.result != "auth_failed" || *second.route != "account" {
		t.Fatalf("row without a session should hold NULLs: %+v", second)
	}
}

// undoV8 turns a version-8 file back into a version-7 one, but for user_version.
var undoV8 = []string{"ALTER TABLE traffic DROP COLUMN failure"}

// undoV7 turns a version-8 file back into a version-6 one, but for user_version.
var undoV7 = append(slices.Clone(undoV8),
	"ALTER TABLE traffic DROP COLUMN model", "ALTER TABLE traffic DROP COLUMN input_tokens", "ALTER TABLE traffic DROP COLUMN output_tokens",
	"ALTER TABLE traffic DROP COLUMN cache_read_tokens", "ALTER TABLE traffic DROP COLUMN cache_creation_tokens", "DROP TABLE quota_history",
)

// TestMigrateAddsRoute opens a version-4 file, written before the direct route
// existed: its CONNECT rows with a target become account, and the row with an invalid
// target and the inference row stay NULL. Control arm: the same rows in a file
// already at version 5 are left as they are; both arms migrate through version 6.
func TestMigrateAddsRoute(t *testing.T) {
	for _, arm := range []struct {
		name    string
		version int
		want    []string
	}{{"version_4", 4, []string{"", "account", "account", ""}}, {"version_5", 5, []string{"", "", "direct", ""}}} {
		t.Run(arm.name, func(t *testing.T) {
			c := newClock()
			store, path := open(t, c)
			ctx := context.Background()
			for _, statement := range append(undoV7, "ALTER TABLE traffic DROP COLUMN request") {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if arm.version < 5 {
				if _, err := store.db.Exec("ALTER TABLE traffic DROP COLUMN route"); err != nil {
					t.Fatal(err)
				}
			}
			insert := "INSERT INTO traffic (ts, kind, host, port, bytes_up, bytes_down, duration_ms, result) VALUES (0, ?, ?, 443, 0, 0, 0, 'ok')"
			for _, row := range [][2]string{{"inference", "api.anthropic.com"}, {"connect", "github.com"}, {"connect", "pypi.org"}, {"connect", ""}} {
				if _, err := store.db.Exec(insert, row[0], row[1]); err != nil {
					t.Fatal(err)
				}
			}
			if arm.version == 5 {
				if _, err := store.db.Exec("UPDATE traffic SET route = 'direct' WHERE host = 'pypi.org'"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", arm.version)); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path, Config{Now: c.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			rows, err := reopened.db.Query("SELECT COALESCE(route, '') FROM traffic ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var got []string
			for rows.Next() {
				var route string
				if err := rows.Scan(&route); err != nil {
					t.Fatal(err)
				}
				got = append(got, route)
			}
			if err := rows.Err(); err != nil || !slices.Equal(got, arm.want) {
				t.Fatalf("routes=%q err=%v, want %q", got, err, arm.want)
			}
			if err := reopened.RecordTraffic(ctx, Traffic{Kind: "connect", Host: "h", Route: "other", Result: "ok"}); err == nil {
				t.Fatal("route outside the CHECK was accepted")
			}
		})
	}
}

// TestVersion5WriterAfterMigration stands in for a handoff: the old process holds a
// version-5 file open while the new one opens it and migrates it. The old
// process's insert, which names the version-5 columns, still succeeds on its open
// connection and leaves request NULL; the new process records a request.
func TestVersion5WriterAfterMigration(t *testing.T) {
	c := newClock()
	old, path := open(t, c)
	ctx := context.Background()
	for _, statement := range append(undoV7, "ALTER TABLE traffic DROP COLUMN request", "PRAGMA user_version = 5") {
		if _, err := old.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// The insert of the version-5 RecordTraffic, verbatim.
	const v5Insert = `
		INSERT INTO traffic (ts, session_id, alias, kind, host, port, bytes_up, bytes_down, duration_ms, result, status, claude_session_id, claude_agent_id, route)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertV5 := func() error {
		_, err := old.db.ExecContext(ctx, v5Insert, 0, nil, nil, "inference", "api.anthropic.com", 443, 0, 0, 0, "auth_failed", nil, nil, nil, nil)
		return err
	}
	if err := insertV5(); err != nil {
		t.Fatal(err)
	}
	// Control arm: the old connection sees the new column only after the migration, so
	// the second insert below runs against the version-6 table.
	if _, err := old.db.Exec("SELECT request FROM traffic"); err == nil {
		t.Fatal("request column present before the migration")
	}
	migrated, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	if _, err := old.db.Exec("SELECT request FROM traffic"); err != nil {
		t.Fatalf("old connection does not see the migration: %v", err)
	}
	if err := insertV5(); err != nil {
		t.Fatalf("version-5 insert after the migration: %v", err)
	}
	if err := migrated.RecordTraffic(ctx, Traffic{Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "forbidden", Request: "GET /v1/models"}); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	rows, err := migrated.db.Query("SELECT result, COALESCE(request, '') FROM traffic ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var result, request string
		if err := rows.Scan(&result, &request); err != nil {
			t.Fatal(err)
		}
		got = append(got, result+"|"+request)
	}
	if want := []string{"auth_failed|", "auth_failed|", "forbidden|GET /v1/models"}; rows.Err() != nil || !slices.Equal(got, want) {
		t.Fatalf("rows=%q err=%v, want %q", got, rows.Err(), want)
	}
}

// TestRecordUsage writes rows with and without usage: reported counts are stored as
// they are, a zero count as zero and an empty model as NULL; a row without usage keeps
// NULL in every usage column.
func TestRecordUsage(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	for _, usage := range []*Usage{
		{Model: "claude-sample-1", InputTokens: 10, OutputTokens: 42, CacheRead: 3, CacheCreation: 2},
		{InputTokens: 5},
		nil,
	} {
		if err := store.RecordTraffic(ctx, Traffic{At: c.Now(), Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "ok", Usage: usage}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.db.Query(`SELECT COALESCE(model, 'NULL'), COALESCE(CAST(input_tokens AS TEXT), 'NULL'), COALESCE(CAST(output_tokens AS TEXT), 'NULL'),
		COALESCE(CAST(cache_read_tokens AS TEXT), 'NULL'), COALESCE(CAST(cache_creation_tokens AS TEXT), 'NULL') FROM traffic ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var model, input, output, cacheRead, cacheCreation string
		if err := rows.Scan(&model, &input, &output, &cacheRead, &cacheCreation); err != nil {
			t.Fatal(err)
		}
		got = append(got, model+" "+input+" "+output+" "+cacheRead+" "+cacheCreation)
	}
	want := []string{"claude-sample-1 10 42 3 2", "NULL 5 0 0 0", "NULL NULL NULL NULL NULL"}
	if err := rows.Err(); err != nil || !slices.Equal(got, want) {
		t.Fatalf("rows=%q err=%v, want %q", got, err, want)
	}
}

// TestMigrateFromVersion6 opens a version-6 file while the process that wrote it still
// holds it open, as in a handoff: the old rows have no usage and the quota history
// starts empty; the old process's insert, which names the version-6 columns, still
// succeeds; the new process records usage and samples its quota readings. Control arm:
// before the migration the version-6 file has no usage column.
func TestMigrateFromVersion6(t *testing.T) {
	c := newClock()
	old, path := open(t, c)
	ctx := context.Background()
	for _, statement := range append(undoV7, "PRAGMA user_version = 6") {
		if _, err := old.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// The insert of the version-6 RecordTraffic, verbatim.
	const v6Insert = `
		INSERT INTO traffic (ts, session_id, alias, kind, host, port, bytes_up, bytes_down, duration_ms, result, status, claude_session_id, claude_agent_id, route, request)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertV6 := func() error {
		_, err := old.db.ExecContext(ctx, v6Insert, 0, nil, "sample-a", "inference", "api.anthropic.com", 443, 0, 0, 0, "ok", 200, nil, nil, nil, nil)
		return err
	}
	if err := insertV6(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec("SELECT input_tokens FROM traffic"); err == nil {
		t.Fatal("usage column present before the migration")
	}
	migrated, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	var version int
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM quota_history"); got != 0 {
		t.Fatalf("quota history after the migration = %d rows", got)
	}
	if err := insertV6(); err != nil {
		t.Fatalf("version-6 insert after the migration: %v", err)
	}
	if err := migrated.RecordTraffic(ctx, Traffic{Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "ok", Usage: &Usage{Model: "claude-sample-1", InputTokens: 7}}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM traffic WHERE input_tokens IS NULL"); got != 2 {
		t.Fatalf("rows without usage = %d, want the two version-6 rows", got)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM traffic WHERE model = 'claude-sample-1' AND input_tokens = 7"); got != 1 {
		t.Fatalf("rows with usage = %d, want 1", got)
	}
	if err := migrated.RecordQuota(ctx, "sample-a", []Reading{{Window: Window5h, Utilization: 0.5, Source: "response"}}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM quota_history"); got != 1 {
		t.Fatalf("quota history after a reading = %d rows, want 1", got)
	}
}

// TestMigrateFromVersion7 opens a version-7 file while the process that wrote it still
// holds it open, as in a handoff: the old rows have no failure; the old process's
// insert, which names the version-7 columns, still succeeds and leaves it NULL; the new
// process records one. Control arm: before the migration the version-7 file has no
// failure column.
func TestMigrateFromVersion7(t *testing.T) {
	c := newClock()
	old, path := open(t, c)
	ctx := context.Background()
	for _, statement := range append(undoV8, "PRAGMA user_version = 7") {
		if _, err := old.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// The insert of the version-7 RecordTraffic, verbatim.
	const v7Insert = `
		INSERT INTO traffic (ts, session_id, alias, kind, host, port, bytes_up, bytes_down, duration_ms, result, status, claude_session_id, claude_agent_id, route, request,
			model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertV7 := func() error {
		_, err := old.db.ExecContext(ctx, v7Insert, 0, nil, "sample-a", "inference", "api.anthropic.com", 443, 0, 0, 0, "upstream_failed", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		return err
	}
	if err := insertV7(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec("SELECT failure FROM traffic"); err == nil {
		t.Fatal("failure column present before the migration")
	}
	migrated, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	var version int
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := insertV7(); err != nil {
		t.Fatalf("version-7 insert after the migration: %v", err)
	}
	if err := migrated.RecordTraffic(ctx, Traffic{Kind: "inference", Host: "api.anthropic.com", Port: 443, Result: "upstream_failed", Failure: "reset"}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM traffic WHERE failure IS NULL"); got != 2 {
		t.Fatalf("rows without failure = %d, want the two version-7 rows", got)
	}
	if got := count(t, migrated, "SELECT COUNT(*) FROM traffic WHERE failure = 'reset'"); got != 1 {
		t.Fatalf("rows with failure = %d, want 1", got)
	}
}
