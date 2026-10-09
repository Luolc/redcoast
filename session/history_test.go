package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestSessionsFilters covers the cwd prefix, the machine, the window's two ends and
// EndedOnly over sessions started a minute apart.
func TestSessionsFilters(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	start := c.Now()
	// issue starts a session, binds it and ends it when ended, then moves the clock on
	// by a minute.
	issue := func(machine, meta string, ended bool) Issued {
		t.Helper()
		issued, err := store.Issue(ctx, machine, LocalSource, json.RawMessage(meta))
		if err != nil {
			t.Fatal(err)
		}
		if ended {
			if _, err := store.Bind(ctx, issued.Token, []string{"sample-a"}); err != nil {
				t.Fatal(err)
			}
			if err := store.Revoke(ctx, issued.Token, "client"); err != nil {
				t.Fatal(err)
			}
		}
		c.Advance(time.Minute)
		return issued
	}
	smoke := issue("client-a", `{"cwd":"/srv/wt/repo-a/smoke","launcher_pid":4100}`, true)
	other := issue("client-a", `{"cwd":"/srv/wt/repo-b","launcher_pid":4101}`, true)
	remote := issue("client-b", `{"cwd":"/srv/wt/repo-a/smoke-2","launcher_pid":"not a number"}`, true)
	live := issue("client-a", `{"cwd":"/srv/wt/repo-a/live"}`, false)
	ids := func(q Query) []string {
		t.Helper()
		list, err := store.Sessions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if list.Truncated {
			t.Fatal("truncated")
		}
		var got []string
		for _, r := range list.Sessions {
			got = append(got, r.ID)
		}
		return got
	}
	all := Query{Since: start, Until: start.Add(time.Hour)}
	minute := func(n int) time.Time { return start.Add(time.Duration(n) * time.Minute) }
	for _, test := range []struct {
		name string
		q    Query
		want []string
	}{
		{"everything_newest_first", all, []string{live.ID, remote.ID, other.ID, smoke.ID}},
		{"ended_only", Query{Since: all.Since, Until: all.Until, EndedOnly: true}, []string{remote.ID, other.ID, smoke.ID}},
		{"prefix_hits", Query{Since: all.Since, Until: all.Until, CwdPrefix: "/srv/wt/repo-a/smoke"}, []string{remote.ID, smoke.ID}},
		{"prefix_misses", Query{Since: all.Since, Until: all.Until, CwdPrefix: "/srv/wt/repo-c"}, nil},
		{"prefix_is_not_a_pattern", Query{Since: all.Since, Until: all.Until, CwdPrefix: "/srv/wt/repo-_"}, nil},
		{"machine", Query{Since: all.Since, Until: all.Until, Machine: "client-b"}, []string{remote.ID}},
		{"since_is_included", Query{Since: minute(1), Until: minute(2)}, []string{other.ID}},
		{"until_is_excluded", Query{Since: minute(0), Until: minute(1)}, []string{smoke.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ids(test.q)
			if len(got) != len(test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("got %v, want %v", got, test.want)
				}
			}
		})
	}
	list, err := store.Sessions(ctx, Query{Since: minute(0), Until: minute(1)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sessions":[{"id":"` + smoke.ID + `","machine":"client-a","cwd":"/srv/wt/repo-a/smoke","launcher_pid":4100,` +
		`"started_at":"2026-10-06T12:00:00Z","ended_at":"2026-10-06T12:00:00Z","end_reason":"client","alias":"sample-a"}],"truncated":false}`
	if string(encoded) != want {
		t.Fatalf("got  %s\nwant %s", encoded, want)
	}
	list, err = store.Sessions(ctx, Query{Since: minute(2), Until: minute(4)})
	if err != nil {
		t.Fatal(err)
	}
	if r := list.Sessions[0]; r.ID != live.ID || r.EndedAt != nil || r.EndReason != nil || r.Alias != nil || r.LauncherPID != nil {
		t.Fatalf("live session %+v", r)
	}
	if r := list.Sessions[1]; r.ID != remote.ID || r.LauncherPID != nil || *r.Cwd != "/srv/wt/repo-a/smoke-2" {
		t.Fatalf("session with a string launcher_pid %+v", r)
	}
	if _, err := store.Sessions(ctx, Query{Since: start, Until: start}); err == nil {
		t.Fatal("an empty window was accepted")
	}
}

// TestSessionsTruncates checks that one more session than the cap is reported as
// truncated, keeping the newest.
func TestSessionsTruncates(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	start := c.Now()
	var newest string
	for range MaxRecords + 1 {
		issued, err := store.Issue(ctx, "client-a", LocalSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		newest = issued.ID
		c.Advance(time.Second)
	}
	list, err := store.Sessions(ctx, Query{Since: start, Until: c.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !list.Truncated || len(list.Sessions) != MaxRecords || list.Sessions[0].ID != newest {
		t.Fatalf("truncated %v, %d sessions", list.Truncated, len(list.Sessions))
	}
	list, err = store.Sessions(ctx, Query{Since: start.Add(time.Second), Until: c.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if list.Truncated || len(list.Sessions) != MaxRecords {
		t.Fatalf("exactly the cap: truncated %v, %d sessions", list.Truncated, len(list.Sessions))
	}
}
