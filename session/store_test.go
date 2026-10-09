package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

// clock is an injectable clock for lease and expiry tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// open opens a store in a fresh temporary directory with a lease of 10 s and an idle
// expiry of 30 s on the given clock, puts the two test accounts on the pro plan, and
// returns it with the file's path.
func open(t *testing.T, c *clock) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	store, err := Open(context.Background(), path, Config{Lease: 10 * time.Second, IdleExpiry: 30 * time.Second, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, alias := range accounts {
		schedule(t, store, alias, "pro")
	}
	return store, path
}

// schedule puts alias on plan from the epoch on, replacing any period starting there.
func schedule(t *testing.T, store *Store, alias, plan string) {
	t.Helper()
	if err := store.SetPlanSchedule(context.Background(), PlanPeriod{Alias: alias, Plan: plan, From: time.UnixMilli(0), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

// binding reads the stored binding of session id directly from the file.
func binding(t *testing.T, store *Store, id string) (alias string, expires int64) {
	t.Helper()
	err := store.db.QueryRow("SELECT alias, lease_expires_at FROM bindings WHERE session_id = ?", id).Scan(&alias, &expires)
	if err != nil {
		t.Fatal(err)
	}
	return alias, expires
}

// count runs a COUNT query against the file.
func count(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var accounts = []string{"sample-b", "sample-a"}

// TestIssueConcurrentUnique checks that credentials issued concurrently are all
// distinct, have the documented shape, and that each session row has a distinct hash.
func TestIssueConcurrentUnique(t *testing.T) {
	store, _ := open(t, newClock())
	const n = 32
	results := make(chan Issued, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			issued, err := store.Issue(context.Background(), "machine-a", LocalSource, nil)
			if err != nil {
				results <- Issued{}
				return
			}
			results <- issued
		})
	}
	wg.Wait()
	close(results)
	shape := regexp.MustCompile(`^sk-ant-gws-[A-Za-z0-9_-]{43}$`)
	tokens := make(map[string]bool)
	ids := make(map[string]bool)
	for issued := range results {
		if !shape.MatchString(issued.Token) {
			t.Fatalf("credential has the wrong shape or issue failed: %q", issued.Token)
		}
		tokens[issued.Token] = true
		ids[issued.ID] = true
	}
	if len(tokens) != n || len(ids) != n {
		t.Fatalf("got %d distinct credentials and %d distinct IDs, want %d", len(tokens), len(ids), n)
	}
	if got := count(t, store, "SELECT COUNT(DISTINCT token_hash) FROM sessions"); got != n {
		t.Fatalf("got %d distinct hashes stored, want %d", got, n)
	}
}

// TestTokenNotStored checks that the credential's value is nowhere in the SQLite file
// while its hash is (the control arm).
func TestTokenNotStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	store, err := Open(context.Background(), path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, store, "sample-a", "pro")
	issued, err := store.Issue(context.Background(), "machine-a", LocalSource, []byte(`{"pid": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(context.Background(), issued.Token, accounts); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var content []byte
	for _, name := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		content = append(content, data...)
	}
	if hits := bytes.Count(content, []byte(issued.Token)); hits != 0 {
		t.Fatalf("credential appears %d times in the file", hits)
	}
	if hits := bytes.Count(content, []byte(hashToken(issued.Token))); hits == 0 {
		t.Fatal("credential hash does not appear in the file; the search would not have found the credential either")
	}
}

// TestRevoke checks that a revoked credential is refused by Bind and by a second
// Revoke, and that the row is kept with its end reason.
func TestRevoke(t *testing.T) {
	store, _ := open(t, newClock())
	ctx := context.Background()
	issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, issued.Token, accounts); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, issued.Token, "client"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, issued.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("got %v after revoke, want ErrUnknownSession", err)
	}
	if err := store.Revoke(ctx, issued.Token, "client"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("got %v on second revoke, want ErrUnknownSession", err)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ? AND ended_at IS NOT NULL AND end_reason = 'client'", issued.ID); got != 1 {
		t.Fatal("session row was not marked ended with the client reason")
	}
	if got := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND to_ts IS NULL", issued.ID); got != 0 {
		t.Fatal("binding history still has an open row after revoke")
	}
	for _, token := range []string{"", "sk-ant-gws-short", "Bearer " + issued.Token, TokenPrefix + "x\n" + issued.Token[12:]} {
		if _, err := store.Bind(ctx, token, accounts); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("got %v for malformed credential %q, want ErrUnknownSession", err, token)
		}
	}
}

// TestLeaseRenewal is E3.4's renewal pair: after the lease expires, a request keeps
// the account and pushes the expiry by one lease; before it expires (the control arm)
// the expiry does not move. Both arms stay on the same account.
func TestLeaseRenewal(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Alias != "sample-a" || first.Reason != "new" {
		t.Fatalf("first binding is %+v, want sample-a (alias order on a tie) with reason new", first)
	}
	c.Advance(9 * time.Second) // control arm: lease not yet expired
	before, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if alias, expires := binding(t, store, issued.ID); alias != "sample-a" || expires != first.LeaseExpiresAt.UnixMilli() || !before.LeaseExpiresAt.Equal(first.LeaseExpiresAt) {
		t.Fatalf("binding changed before expiry: alias %s expiry %d, want sample-a %d", alias, expires, first.LeaseExpiresAt.UnixMilli())
	}
	c.Advance(2 * time.Second) // 11 s after binding: expired
	after, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil {
		t.Fatal(err)
	}
	want := c.Now().Add(10 * time.Second).UnixMilli()
	if alias, expires := binding(t, store, issued.ID); alias != "sample-a" || expires != want || after.LeaseExpiresAt.UnixMilli() != want {
		t.Fatalf("after expiry: alias %s expiry %d, want sample-a renewed to %d", alias, expires, want)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ?", issued.ID); got != 1 {
		t.Fatalf("renewal wrote %d history rows, want 1 (no rebind)", got)
	}
}

// TestRebindOnPause is E3.4's switch pair: pausing the bound account moves the next
// request to the other account with a fresh lease; without the pause (the control
// arm) the same request stays where it was.
func TestRebindOnPause(t *testing.T) {
	for _, pause := range []bool{false, true} {
		c := newClock()
		store, _ := open(t, c)
		ctx := context.Background()
		issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		first, err := store.Bind(ctx, issued.Token, accounts)
		if err != nil {
			t.Fatal(err)
		}
		c.Advance(3 * time.Second) // inside the lease in both arms
		if pause {
			if err := store.Pause(ctx, first.Alias, c.Now().Add(time.Hour), "synthetic 401"); err != nil {
				t.Fatal(err)
			}
		}
		next, err := store.Bind(ctx, issued.Token, accounts)
		if err != nil {
			t.Fatal(err)
		}
		alias, expires := binding(t, store, issued.ID)
		history := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ?", issued.ID)
		closed := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND to_ts IS NOT NULL", issued.ID)
		if !pause {
			if alias != first.Alias || expires != first.LeaseExpiresAt.UnixMilli() || history != 1 {
				t.Fatalf("control arm: binding moved to %s expiry %d history %d", alias, expires, history)
			}
			continue
		}
		want := c.Now().Add(10 * time.Second).UnixMilli()
		if alias != "sample-b" || next.Alias != "sample-b" || next.Reason != "unavailable" || expires != want || history != 2 || closed != 1 {
			t.Fatalf("pause arm: alias %s reason %s expiry %d (want sample-b unavailable %d), history %d closed %d", alias, next.Reason, expires, want, history, closed)
		}
		if err := store.Resume(ctx, first.Alias); err != nil {
			t.Fatal(err)
		}
		if again, err := store.Bind(ctx, issued.Token, accounts); err != nil || again.Alias != "sample-b" {
			t.Fatalf("after resume the session left its lease: %+v %v", again, err)
		}
	}
}

// TestConcurrentRequestsShareBinding checks that concurrent requests of one unbound
// session all land on one account and write one binding.
func TestConcurrentRequestsShareBinding(t *testing.T) {
	store, _ := open(t, newClock())
	ctx := context.Background()
	issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	aliases := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			b, err := store.Bind(ctx, issued.Token, accounts)
			if err != nil {
				aliases <- err.Error()
				return
			}
			aliases <- b.Alias
		})
	}
	wg.Wait()
	close(aliases)
	seen := make(map[string]int)
	for alias := range aliases {
		seen[alias]++
	}
	if len(seen) != 1 || seen["sample-a"] != n {
		t.Fatalf("concurrent requests saw %v, want all sample-a", seen)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM binding_history WHERE session_id = ?", issued.ID); got != 1 {
		t.Fatalf("concurrent requests wrote %d history rows, want 1", got)
	}
}

// TestChooseLeastActive checks the stage-3 selection: the account with the fewest
// active sessions wins, idle and ended sessions do not count, and with no eligible
// account Bind returns ErrNoAccount.
func TestChooseLeastActive(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	bind := func(candidates []string) (Issued, Binding, error) {
		issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := store.Bind(ctx, issued.Token, candidates)
		return issued, b, err
	}
	_, first, err := bind(accounts)
	if err != nil || first.Alias != "sample-a" {
		t.Fatalf("first session got %v %v, want sample-a", first.Alias, err)
	}
	_, second, err := bind(accounts)
	if err != nil || second.Alias != "sample-b" {
		t.Fatalf("second session got %v %v, want sample-b (sample-a has one active session)", second.Alias, err)
	}
	// Both accounts have one active session; the tie goes to alias order.
	_, third, err := bind(accounts)
	if err != nil || third.Alias != "sample-a" {
		t.Fatalf("third session got %v %v, want sample-a", third.Alias, err)
	}
	if _, _, err := bind(nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("got %v with no candidates, want ErrNoAccount", err)
	}
	for _, alias := range accounts {
		if err := store.Pause(ctx, alias, c.Now().Add(time.Hour), "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := bind(accounts); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("got %v with every candidate paused, want ErrNoAccount", err)
	}
}

// TestIdleExpiry checks that a session without requests for the idle expiry is ended
// with reason idle, both by its own next request and by ExpireIdle, while a recently
// seen session (the control arm) survives both.
func TestIdleExpiry(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	stale, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(29 * time.Second)
	if _, err := store.Bind(ctx, fresh.Token, accounts); err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Second) // stale: 31 s without a request; fresh: 2 s
	if _, err := store.Bind(ctx, stale.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("got %v for the idle session, want ErrUnknownSession", err)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ? AND end_reason = 'idle'", stale.ID); got != 1 {
		t.Fatal("idle session was not ended with reason idle")
	}
	if _, err := store.Bind(ctx, fresh.Token, accounts); err != nil {
		t.Fatalf("recently seen session refused: %v", err)
	}
	third, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(30 * time.Second)
	ended, err := store.ExpireIdle(ctx)
	if err != nil || ended != 2 {
		t.Fatalf("ExpireIdle ended %d sessions (%v), want 2 (fresh and third)", ended, err)
	}
	for _, issued := range []Issued{fresh, third} {
		if _, err := store.Bind(ctx, issued.Token, accounts); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("session %s still accepted after ExpireIdle: %v", issued.ID, err)
		}
	}
}

// TestNoAccountKeepsActivity is the regression for the rolled-back activity update:
// a session whose every request finds no available account is still active, so it
// is not idle-expired and works again once an account is resumed; a session that
// really sends nothing (the control arm) expires in the same period.
func TestNoAccountKeepsActivity(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	busy, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	silent, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range accounts {
		if err := store.Pause(ctx, alias, c.Now().Add(time.Hour), "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []time.Duration{10 * time.Second, 10 * time.Second, 9 * time.Second, 2 * time.Second} {
		c.Advance(step) // requests at 10, 20, 29 and 31 s after issue
		if _, err := store.Bind(ctx, busy.Token, accounts); !errors.Is(err, ErrNoAccount) {
			t.Fatalf("got %v with every account paused, want ErrNoAccount", err)
		}
		var lastSeen int64
		if err := store.db.QueryRow("SELECT last_seen_at FROM sessions WHERE id = ?", busy.ID).Scan(&lastSeen); err != nil || lastSeen != c.Now().UnixMilli() {
			t.Fatalf("last_seen_at is %d (%v), want %d: the request was not recorded", lastSeen, err, c.Now().UnixMilli())
		}
	}
	if count(t, store, "SELECT COUNT(*) FROM sessions WHERE id = ? AND ended_at IS NOT NULL", busy.ID) != 0 {
		t.Fatal("session with continuous requests was ended")
	}
	if _, err := store.Bind(ctx, silent.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("control arm: silent session got %v after 31 s, want ErrUnknownSession", err)
	}
	if err := store.Resume(ctx, "sample-a"); err != nil {
		t.Fatal(err)
	}
	if b, err := store.Bind(ctx, busy.Token, accounts); err != nil || b.Alias != "sample-a" {
		t.Fatalf("after resume got %+v %v, want sample-a", b, err)
	}
}

// TestReopenKeepsState checks that sessions and bindings survive closing and reopening
// the file, and that the settings defaults are written once.
func TestReopenKeepsState(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	cfg := Config{Lease: 10 * time.Second, Now: c.Now}
	ctx := context.Background()
	store, err := Open(ctx, path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range accounts {
		schedule(t, store, alias, "pro")
	}
	issued, err := store.Issue(ctx, "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	again, err := store.Bind(ctx, issued.Token, accounts)
	if err != nil || again.Alias != first.Alias || !again.LeaseExpiresAt.Equal(first.LeaseExpiresAt) {
		t.Fatalf("binding after reopen is %+v (%v), want %+v", again, err, first)
	}
	var soft, hard string
	if err := store.db.QueryRow("SELECT value FROM settings WHERE key = 'soft'").Scan(&soft); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT value FROM settings WHERE key = 'hard'").Scan(&hard); err != nil {
		t.Fatal(err)
	}
	if soft != DefaultSoft || hard != DefaultHard || count(t, store, "SELECT COUNT(*) FROM settings") != 2 {
		t.Fatalf("settings are soft=%s hard=%s, want %s / %s and nothing else", soft, hard, DefaultSoft, DefaultHard)
	}
	var mode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode is %q (%v), want wal", mode, err)
	}
}
