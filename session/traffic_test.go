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
			if _, err := store.db.Exec("ALTER TABLE traffic DROP COLUMN request"); err != nil {
				t.Fatal(err)
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
// version-5 file open while the new one opens it and migrates it to version 6. The old
// process's insert, which names the version-5 columns, still succeeds on its open
// connection and leaves request NULL; the new process records a request.
func TestVersion5WriterAfterMigration(t *testing.T) {
	c := newClock()
	old, path := open(t, c)
	ctx := context.Background()
	for _, statement := range []string{"ALTER TABLE traffic DROP COLUMN request", "PRAGMA user_version = 5"} {
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
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
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
