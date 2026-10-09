package session

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestRefusedCredentialTakesNoWriteLock holds the file's write lock from another
// connection, as a long transaction would. A malformed, unknown or revoked credential,
// or a live one from the wrong address, is refused at once, so it neither waits for
// the lock nor takes it. Control arm: a valid request waits until the lock is released
// and then binds.
func TestRefusedCredentialTakesNoWriteLock(t *testing.T) {
	c := newClock()
	store, path := open(t, c)
	ctx := context.Background()
	issued, err := store.Issue(ctx, "m", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := store.Issue(ctx, "m", LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, revoked.Token, "client"); err != nil {
		t.Fatal(err)
	}
	query := url.Values{}
	query.Set("_txlock", "immediate")
	other, err := sql.Open("sqlite", fileURI(path, query))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	lock, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		token, source string
		want          error
	}{
		{"not-a-session", LocalSource, ErrUnknownSession},
		{TokenPrefix + strings.Repeat("B", 43), LocalSource, ErrUnknownSession},
		{revoked.Token, LocalSource, ErrUnknownSession},
		{issued.Token, "198.51.100.9", ErrWrongSource},
	} {
		start := time.Now()
		_, err := store.BindFrom(ctx, refused.token, refused.source, accounts)
		if !errors.Is(err, refused.want) || time.Since(start) > time.Second {
			t.Fatalf("%q from %s: err=%v after %v", refused.token, refused.source, err, time.Since(start))
		}
		if err := store.RevokeFrom(ctx, refused.token, refused.source, "client"); !errors.Is(err, refused.want) {
			t.Fatalf("revoke %q from %s: err=%v", refused.token, refused.source, err)
		}
	}
	bound := make(chan error, 1)
	go func() {
		_, err := store.Bind(ctx, issued.Token, accounts)
		bound <- err
	}()
	select {
	case err := <-bound:
		t.Fatalf("valid request did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-bound; err != nil {
		t.Fatal(err)
	}
}
