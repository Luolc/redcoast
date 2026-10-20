package session

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDashboardClients checks that live sessions carry the client program and version
// the launcher reported, that sessions and requests are counted by client machine and
// client program, and that a missing or malformed report, or metadata SQLite cannot
// parse, counts as unknown without any of its text, or any other launch metadata,
// reaching the snapshot.
func TestDashboardClients(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	now := c.Now()
	issue := func(machine, meta string) Issued {
		t.Helper()
		var raw json.RawMessage
		if meta != "" {
			raw = json.RawMessage(meta)
		}
		issued, err := store.Issue(ctx, machine, LocalSource, raw)
		if err != nil {
			t.Fatal(err)
		}
		return issued
	}
	request := func(s Issued, at time.Time) {
		t.Helper()
		if err := store.RecordTraffic(ctx, Traffic{At: at, SessionID: s.ID, Kind: "inference", Host: "api.example.com", Port: 443, Result: "ok", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	claude := issue("devbox-1", `{"launcher_pid":1,"cwd":"/home/someone/private-project","client":"claude-code","client_version":"2.1.0"}`)
	pi := issue("devbox-1", `{"client":"pi","client_version":"1.2.0-beta.1"}`)
	unreported := issue("devbox-2", "")
	forged := issue("devbox-2", `{"client":"<b>forged</b>","client_version":"9.9.9"}`)
	noisy := issue("devbox-2", `{"client":"codex-cli","client_version":"0.46.0 <b>noise</b>"}`)
	numeric := issue("devbox-2", `{"client":7,"client_version":1.5}`)
	// Valid for Go's json.Valid, too deep for SQLite's JSON parser.
	nested := issue("devbox-2", `{"client":"pi","a":`+strings.Repeat("[", 1500)+strings.Repeat("]", 1500)+`}`)
	old := issue("devbox-1", `{"client":"claude-code","client_version":"2.0.0"}`)
	if _, err := store.db.Exec("UPDATE sessions SET created_at = ?, ended_at = ? WHERE id = ?", now.Add(-8*24*time.Hour).UnixMilli(), now.Add(-8*24*time.Hour).UnixMilli(), old.ID); err != nil {
		t.Fatal(err)
	}
	request(claude, now.Add(-90*time.Minute))
	request(claude, now.Add(-time.Minute))
	request(pi, now.Add(-time.Minute))
	request(forged, now.Add(-time.Minute))
	request(numeric, now.Add(-time.Minute))
	request(nested, now.Add(-time.Minute))
	if err := store.Revoke(ctx, pi.Token, "client_exit"); err != nil {
		t.Fatal(err)
	}

	snap, err := openReader(t, path, c).Snapshot(ctx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	type reported struct{ client, version string }
	live := map[string]reported{}
	for _, s := range snap.Sessions {
		live[s.ID] = reported{s.Client, s.ClientVersion}
	}
	want := map[string]reported{
		claude.ID[:shortID]:     {"claude-code", "2.1.0"},
		unreported.ID[:shortID]: {"", ""},
		forged.ID[:shortID]:     {"", ""},
		noisy.ID[:shortID]:      {"codex-cli", ""},
		numeric.ID[:shortID]:    {"", ""},
		nested.ID[:shortID]:     {"", ""},
	}
	if len(live) != len(want) {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	for id, w := range want {
		if live[id] != w {
			t.Fatalf("session %s reports %+v, want %+v", id, live[id], w)
		}
	}
	wantCounts := []ClientCount{
		{Machine: "devbox-1", Client: "claude-code", Live: 1, Sessions: 1},
		{Machine: "devbox-1", Client: "pi", Live: 0, Sessions: 1},
		{Machine: "devbox-2", Client: "", Live: 4, Sessions: 4},
		{Machine: "devbox-2", Client: "codex-cli", Live: 1, Sessions: 1},
	}
	if !slices.Equal(snap.Clients, wantCounts) {
		t.Fatalf("clients = %+v, want %+v", snap.Clients, wantCounts)
	}
	hour := now.Truncate(time.Hour)
	wantHourly := []ClientHour{
		{Hour: hour.Add(-2 * time.Hour), Machine: "devbox-1", Client: "claude-code", Requests: 1},
		{Hour: hour.Add(-time.Hour), Machine: "devbox-1", Client: "claude-code", Requests: 1},
		{Hour: hour.Add(-time.Hour), Machine: "devbox-1", Client: "pi", Requests: 1},
		{Hour: hour.Add(-time.Hour), Machine: "devbox-2", Client: "", Requests: 3},
	}
	if !slices.Equal(snap.ClientHourly, wantHourly) {
		t.Fatalf("client_hourly = %+v, want %+v", snap.ClientHourly, wantHourly)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"private-project", "forged", "noise", "launcher_pid"} {
		if strings.Contains(string(body), leaked) {
			t.Fatalf("snapshot carries %q from the launch metadata: %s", leaked, body)
		}
	}
	// Control arm: the valid report is there, so the search above read the field.
	if !strings.Contains(string(body), `"client":"claude-code","client_version":"2.1.0"`) {
		t.Fatalf("snapshot lacks the reported client: %s", body)
	}
}
