package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
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

// prefixForm is a credential form for tests: the issued credential behind a prefix.
type prefixForm struct{}

func (prefixForm) Wrap(credential string) string { return "wrapped." + credential }

func (prefixForm) Unwrap(presented string) (string, bool) {
	return strings.CutPrefix(presented, "wrapped.")
}

// TestCredentialForm checks that a store with a credential form hands out the wrapped
// credential, accepts it, refuses the bare one as not a session credential, and keeps
// only the hash of the bare one; a store without the form refuses the wrapped one.
func TestCredentialForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	store, err := Open(t.Context(), path, Config{Form: prefixForm{}})
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, store, "sample-a", "pro")
	issued, err := store.Issue(t.Context(), "machine-a", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	bare, wrapped := strings.CutPrefix(issued.Token, "wrapped.")
	if !wrapped || !regexp.MustCompile(`^sk-ant-gws-[A-Za-z0-9_-]{43}$`).MatchString(bare) {
		t.Fatal("the issued credential is not the wrapped form of a session credential")
	}
	if _, err := store.Bind(t.Context(), issued.Token, accounts); err != nil {
		t.Fatalf("wrapped credential: %v", err)
	}
	if _, err := store.Bind(t.Context(), bare, accounts); !errors.Is(err, ErrNotSessionCredential) {
		t.Fatalf("bare credential: %v", err)
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
	if bytes.Contains(content, []byte(bare)) || !bytes.Contains(content, []byte(hashToken(bare))) {
		t.Fatal("the file does not keep only the hash of the bare credential")
	}
	plain, err := Open(t.Context(), path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if _, err := plain.Bind(t.Context(), issued.Token, accounts); !errors.Is(err, ErrNotSessionCredential) {
		t.Fatalf("wrapped credential without the form: %v", err)
	}
	if _, err := plain.Bind(t.Context(), bare, accounts); err != nil {
		t.Fatalf("control arm, bare credential without the form: %v", err)
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
		// Callers that check only for ErrUnknownSession must refuse it too.
		if _, err := store.Bind(ctx, token, accounts); !errors.Is(err, ErrNotSessionCredential) || !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("got %v for malformed credential %q, want ErrNotSessionCredential wrapping ErrUnknownSession", err, token)
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

// TestAccountsTableMigration checks that a version-8 file gets an empty, uninitialized
// accounts table, and that the rows imported later come back as they went in. Control
// arm: the version-8 file has no such table before the migration.
func TestAccountsTableMigration(t *testing.T) {
	store, path := open(t, newClock())
	ctx := context.Background()
	for _, statement := range append(slices.Clone(undoV9), "PRAGMA user_version = 8") {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("SELECT 1 FROM accounts"); err == nil {
		t.Fatal("control arm: the version-8 file has an accounts table")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	if rows, initialized, err := migrated.Accounts(ctx); err != nil || initialized || len(rows) != 0 {
		t.Fatalf("after the migration: %d rows, initialized %v, %v", len(rows), initialized, err)
	}
	row := AccountRow{Alias: "sample-a", Email: "a@example.test", Access: "gateway", Status: "active", TokenRef: "op://example-vault/token-a/credential",
		ProxyHost: "proxy.example.test", ProxyPort: 8080, ExpectedIP: "192.0.2.1", UsernameRef: "op://example-proxies/p/username", PasswordRef: "op://example-proxies/p/password"}
	if imported, err := migrated.ImportAccounts(ctx, []AccountRow{row}); err != nil || !imported {
		t.Fatalf("import = %v, %v", imported, err)
	}
	rows, initialized, err := migrated.Accounts(ctx)
	if err != nil || !initialized || len(rows) != 1 {
		t.Fatalf("after the import: %d rows, initialized %v, %v", len(rows), initialized, err)
	}
	got := rows[0]
	if got.Rev != 1 || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("row = %+v", got)
	}
	got.Rev, got.CreatedAt, got.UpdatedAt = 0, time.Time{}, time.Time{}
	if got != row {
		t.Fatalf("row came back as %+v, went in as %+v", got, row)
	}
}

// TestAccountWrites checks the store's account writes: a create is revision 1 with an
// audit row of its fields; an update at a stale revision is refused with nothing
// written, so of two updates made at one revision only the first lands; a change of the
// exit voids the checks and an email change does not; a verification keeps the
// revision, is refused for a stale one, and an update that changes nothing writes
// nothing.
func TestAccountWrites(t *testing.T) {
	store, _ := open(t, newClock())
	ctx := context.Background()
	entry := AuditEntry{SourceIP: "192.0.2.50", Action: "account-update"}
	row := AccountRow{Alias: "sample-a", Email: "a@example.test", Access: "gateway", Status: "testing", ProxyHost: "proxy.example.test", ProxyPort: 8080, ExpectedIP: "192.0.2.1"}
	created, err := store.CreateAccount(ctx, row, AuditEntry{SourceIP: "192.0.2.50", Action: "account-create"})
	if err != nil || created.Rev != 1 {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if _, err := store.CreateAccount(ctx, row, entry); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("second create: %v", err)
	}
	set := func(email string) func(*AccountRow) error {
		return func(r *AccountRow) error { r.Email = email; return nil }
	}
	if _, err := store.UpdateAccount(ctx, "sample-a", 7, entry, set("x@example.test")); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("update at a stale revision: %v", err)
	}
	if _, err := store.UpdateAccount(ctx, "sample-none", 1, entry, set("x@example.test")); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("update of no account: %v", err)
	}
	// Two updates at revision 1: one lands, the other conflicts and writes nothing.
	first, err := store.UpdateAccount(ctx, "sample-a", 1, entry, set("first@example.test"))
	if err != nil || first.Rev != 2 {
		t.Fatalf("first update = %+v, %v", first, err)
	}
	if _, err := store.UpdateAccount(ctx, "sample-a", 1, entry, set("second@example.test")); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("second update at revision 1: %v", err)
	}
	if got, _ := store.Account(ctx, "sample-a"); got.Email != "first@example.test" || got.Rev != 2 {
		t.Fatalf("account = %+v", got)
	}
	// An update that changes nothing writes no revision and no audit row.
	if same, err := store.UpdateAccount(ctx, "sample-a", 2, entry, set("first@example.test")); err != nil || same.Rev != 2 {
		t.Fatalf("no-op update = %+v, %v", same, err)
	}
	// A verification keeps the revision and is refused for a stale one.
	yes, no := true, false
	checked, err := store.SetVerification(ctx, "sample-a", 2, Verification{TokenOK: &yes, EgressOK: &no, At: time.UnixMilli(1000)}, AuditEntry{SourceIP: "192.0.2.50", Action: "account-verify"})
	if err != nil || checked.Rev != 2 || checked.Verify.TokenOK == nil || !*checked.Verify.TokenOK || *checked.Verify.EgressOK || checked.Verify.ProbeOK != nil {
		t.Fatalf("verification = %+v, %v", checked, err)
	}
	if _, err := store.SetVerification(ctx, "sample-a", 1, Verification{TokenOK: &no}, entry); !errors.Is(err, ErrRevConflict) {
		t.Fatalf("verification at a stale revision: %v", err)
	}
	// An email change keeps the checks; a change of the exit voids them.
	if kept, err := store.UpdateAccount(ctx, "sample-a", 2, entry, set("kept@example.test")); err != nil || kept.Verify.TokenOK == nil {
		t.Fatalf("email change = %+v, %v", kept, err)
	}
	voided, err := store.UpdateAccount(ctx, "sample-a", 3, entry, func(r *AccountRow) error { r.ProxyPort = 9090; return nil })
	if err != nil || voided.Verify.TokenOK != nil || voided.Verify.EgressOK != nil || !voided.Verify.At.IsZero() {
		t.Fatalf("exit change = %+v, %v", voided, err)
	}
	// The audit: the create, the first update, the verification, two updates after it.
	audit, err := store.AccountAudit(ctx, "sample-a")
	if err != nil || len(audit) != 5 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if audit[1].Changes["email"] != (FieldChange{Old: "a@example.test", New: "first@example.test"}) || audit[2].Changes["token_ok"] != (FieldChange{Old: "", New: "pass"}) || audit[2].Changes["egress_ok"].New != "fail" {
		t.Fatalf("audit changes = %+v", audit)
	}
	if audit[4].Changes["proxy_port"] != (FieldChange{Old: "8080", New: "9090"}) || audit[4].Changes["token_ok"] != (FieldChange{Old: "pass", New: ""}) {
		t.Fatalf("audit of the exit change = %+v", audit[4].Changes)
	}
	// An apply that fails writes nothing.
	failure := errors.New("refused")
	if _, err := store.UpdateAccount(ctx, "sample-a", 4, entry, func(*AccountRow) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("failing apply: %v", err)
	}
}

// v13Columns are the columns of the accounts table of version 13.
var v13Columns = strings.Replace(accountColumns, "observation_since, ", "", 1)

// toV13 turns a version-14 file back into a version-13 one: the accounts table as
// versions 9, 10 and 13 left it, its rows kept, and user_version 13.
var toV13 = []string{
	"ALTER TABLE accounts RENAME TO accounts_v14_copy",
	schemaV9,
	"ALTER TABLE accounts ADD COLUMN token_ok INTEGER",
	"ALTER TABLE accounts ADD COLUMN egress_ok INTEGER",
	"ALTER TABLE accounts ADD COLUMN probe_ok INTEGER",
	"ALTER TABLE accounts ADD COLUMN verified_at INTEGER",
	schemaV13,
	"INSERT INTO accounts (" + v13Columns + ") SELECT " + v13Columns + " FROM accounts_v14_copy",
	"DROP TABLE accounts_v14_copy",
	"PRAGMA user_version = 13",
}

// TestMigrateToV14: a version-13 file gets the status kyc and the column
// observation_since; every row keeps every value, the checks and the audit included, with
// no observation date; kyc and a date can then be written, and an unknown status or a
// date of another shape is still refused. A migration that fails leaves the version-13
// file as it was. Control arm: the version-13 file refuses kyc and has no such column.
func TestMigrateToV14(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	store, path := open(t, c)
	pass := true
	rows := []AccountRow{
		{Alias: "sample-a", Email: "a@example.test", Access: "gateway", Status: "active", TokenRef: "op://example-vault/t/credential", ProxyHost: "proxy.example.test", ProxyPort: 8001,
			ExpectedIP: "192.0.2.1", UsernameRef: "op://p/u/username", PasswordRef: "op://p/u/password", DeviceID: "d", AccountUUID: "u", ReceiverEmail: "r@example.test", ProxyProvider: "example-provider", ProxyType: "residential"},
		{Alias: "sample-b", Email: "b@example.test", Access: "direct", Status: "retired"},
	}
	if _, err := store.ImportAccounts(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetVerification(ctx, "sample-a", 1, Verification{TokenOK: &pass, EgressOK: &pass, At: fromMillis(c.Now().UnixMilli())}, AuditEntry{SourceIP: "192.0.2.50", Action: "account-verify"}); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range toV13 {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := store.db.Exec("UPDATE accounts SET status = 'kyc' WHERE alias = 'sample-b'"); err == nil {
		t.Fatal("control arm: the version-13 file takes kyc")
	}
	if _, err := store.db.Exec("SELECT observation_since FROM accounts"); err == nil {
		t.Fatal("control arm: the version-13 file has the column")
	}
	// A migration that fails on the way rolls back: the name it builds the new table
	// under is taken.
	if _, err := store.db.Exec("CREATE TABLE accounts_v14 (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if failed, err := Open(ctx, path, Config{Now: c.Now}); err == nil {
		_ = failed.Close()
		t.Fatal("the migration did not fail")
	}
	old, err := sql.Open("sqlite", fileURI(path, nil))
	if err != nil {
		t.Fatal(err)
	}
	var version, count int
	if err := old.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		t.Fatalf("after a failed migration: version %d, %v", version, err)
	}
	if err := old.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count); err != nil || count != 2 {
		t.Fatalf("after a failed migration: %d rows, %v", count, err)
	}
	if _, err := old.Exec("DROP TABLE accounts_v14"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path, Config{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
		t.Fatalf("version %d, %v", version, err)
	}
	after, initialized, err := migrated.Accounts(ctx)
	if err != nil || !initialized || !reflect.DeepEqual(after, before) {
		t.Fatalf("rows after the migration:\n%+v\nbefore:\n%+v\n%v", after, before, err)
	}
	if audit, err := migrated.AccountAudit(ctx, "sample-a"); err != nil || len(audit) != 1 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	var tables int
	if err := migrated.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name LIKE 'accounts%'").Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("%d tables named accounts*, %v", tables, err)
	}
	updated, err := migrated.UpdateAccount(ctx, "sample-b", 1, AuditEntry{SourceIP: "192.0.2.50", Action: "account-update"}, func(r *AccountRow) error {
		r.Status, r.ObservationSince = "kyc", "2026-11-13"
		return nil
	})
	if err != nil || updated.Status != "kyc" || updated.ObservationSince != "2026-11-13" {
		t.Fatalf("written row = %+v, %v", updated, err)
	}
	if again, err := migrated.Account(ctx, "sample-b"); err != nil || again.Status != "kyc" || again.ObservationSince != "2026-11-13" {
		t.Fatalf("read back = %+v, %v", again, err)
	}
	for _, statement := range []string{"UPDATE accounts SET status = 'gone' WHERE alias = 'sample-b'", "UPDATE accounts SET observation_since = '2026-1-13' WHERE alias = 'sample-b'"} {
		if _, err := migrated.db.Exec(statement); err == nil {
			t.Fatalf("%s: taken", statement)
		}
	}
}
