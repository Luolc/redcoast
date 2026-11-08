package claude

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Luolc/redcoast/credential"
)

// readLog records the references a resolver was asked to read from the secret store.
type readLog struct {
	mu   sync.Mutex
	refs []string
}

func (l *readLog) wrap(inner Resolver) Resolver {
	return func(ctx context.Context, reference string) (string, error) {
		l.mu.Lock()
		l.refs = append(l.refs, reference)
		l.mu.Unlock()
		return inner(ctx, reference)
	}
}

// taken returns the sorted references read since the last call and forgets them.
func (l *readLog) taken() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	refs := slices.Clone(l.refs)
	l.refs = nil
	slices.Sort(refs)
	return refs
}

// memoWired wires h's write path as the gateway wires it, over a counting resolver: a
// reload reads what it has not read, a write reads the references it concerns again. With
// memoized false every resolver reads everything it is asked for, as before. The served
// set is loaded the way a start loads it, and the reads of that are forgotten.
func memoWired(t *testing.T, h *setupHarness, memoized bool) *readLog {
	t.Helper()
	reads := &readLog{}
	secrets := func(ctx context.Context, reference string) (string, error) {
		if value, ok := h.secrets.value(reference); ok {
			return value, nil
		}
		return writeResolve(ctx, reference)
	}
	counted := reads.wrap(secrets)
	memo := credential.NewMemo()
	cached, fresh := Resolver(counted), Resolver(counted)
	if memoized {
		cached, fresh = memo.Cached(counted), memo.Fresh(counted)
	}
	h.writer.cfg.Resolve = func(context.Context) (Resolver, error) { return cached, nil }
	h.writer.cfg.ResolveFresh = func(context.Context) (Resolver, error) { return fresh, nil }
	h.writer.cfg.Reload = func(ctx context.Context) (Reload, error) {
		report, err := h.accounts.ReloadFrom(ctx, StoreSource(h.store, h.dir), cached, h.egress.Check)
		if err == nil {
			memo.Prune()
		}
		return report, err
	}
	set, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, h.dir), Claude, cached, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	h.accounts.set.Store(set)
	reads.taken()
	return reads
}

// TestWritesReadOnlyWhatTheyConcern checks how often the secret store is read: an
// edit of a served account, a handover, an activation and a reload with nothing new each
// read only the references of the one account written, and the reload each of them ends
// with reads none. The control arm resolves everything every time, as before, and reads
// the whole set on the reload alone.
func TestWritesReadOnlyWhatTheyConcern(t *testing.T) {
	const (
		user, pass = "op://example-proxies/proxy-account/username", "op://example-proxies/proxy-account/password"
		tokenNew   = "op://example-vault/token-sample-new/credential"
		tokenA     = "op://example-vault/token-sample-a/credential"
	)
	for _, memoized := range []bool{true, false} {
		t.Run(fmt.Sprintf("memoized=%v", memoized), func(t *testing.T) {
			h := newSetupHarness(t, true)
			reads := memoWired(t, h, memoized)
			// want says what a step reads; the control arm reads at least the references of
			// the step's reload besides.
			expect := func(step string, want []string) {
				t.Helper()
				slices.Sort(want)
				got := reads.taken()
				if memoized && !slices.Equal(got, want) {
					t.Fatalf("%s read %v, want exactly %v", step, got, want)
				}
				if !memoized && len(got) <= len(want) && len(want) > 0 {
					t.Fatalf("control arm: %s read only %v", step, got)
				}
			}

			if status, _, body := h.serve(t, request("PATCH", "/accounts/sample-a", `{"rev":1,"device_id":"`+testDeviceID2+`"}`)); status != 200 {
				t.Fatalf("edit: %d %s", status, body)
			}
			expect("an edit of a served account", []string{pass, tokenA, user})

			id := h.running(t, "sample-a")
			if status, _, body := h.token(t, "sample-a", id, setupRun, setupToken); status != 200 {
				t.Fatalf("handover: %d %s", status, body)
			}
			// The read-back of the token reference is the one read; the proxy login the probe
			// needs is not read again.
			expect("a handover", []string{tokenA})
			// The reload after the handover serves the token that was read back, not the one
			// the process read before.
			a := h.accounts.set.Load().lookup("sample-a")
			if c, ok := a.credential.(claudeCredential); !ok || c.token != setupToken {
				t.Fatal("the served account does not hold the handed-over token")
			}

			h.created(t)
			h.echo.mu.Lock()
			h.echo.ip = "192.0.2.2"
			h.echo.mu.Unlock()
			expect("create", nil)
			if status, _, body := h.serve(t, request("POST", "/accounts/sample-new/activate", `{"rev":1}`)); status != 200 {
				t.Fatalf("activate: %d %s", status, body)
			}
			// The three checks read the account's token and its exit's login; the reload that
			// follows, with sample-a and sample-new in it, reads nothing more.
			expect("activate", []string{pass, tokenNew, user})

			if _, err := h.writer.cfg.Reload(t.Context()); err != nil {
				t.Fatal(err)
			}
			expect("a reload with nothing new", nil)
		})
	}
}

// TestCannotResolveKeepsTheCause checks that a reference the resolver fails on is
// reported by alias and field as before, with the resolver's error underneath, so that a
// caller can tell a rate limit from a missing item.
func TestCannotResolveKeepsTheCause(t *testing.T) {
	limited := errors.New("synthetic rate limit")
	dir := writeInventory(t, map[string]string{"sample-a.yaml": inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8080")})
	resolve := func(_ context.Context, reference string) (string, error) {
		if strings.Contains(reference, "proxy") {
			return "", limited
		}
		return writeResolve(t.Context(), reference)
	}
	_, err := LoadAccounts(t.Context(), dir, Claude, resolve, DefaultAccountConcurrency)
	if !errors.Is(err, limited) {
		t.Fatalf("the resolver's error is not under %v", err)
	}
	if fields := fieldsOf(err); !slices.Equal(fields, []string{"proxy.username_ref", "proxy.password_ref"}) || !strings.Contains(err.Error(), "sample-a: proxy.username_ref: cannot resolve: synthetic rate limit") {
		t.Fatalf("fields %v, error %q", fields, err)
	}
	// The token reference is reported the same way.
	_, err = LoadAccounts(t.Context(), dir, Claude, func(context.Context, string) (string, error) { return "", limited }, DefaultAccountConcurrency)
	if !errors.Is(err, limited) || !slices.Contains(fieldsOf(err), "oauth_token") {
		t.Fatalf("token: %v", err)
	}
}

// TestRotationNeedsVerifyThenReload checks the way to rotate a token in place: a reload
// alone does not read the changed value, `verify` reads it again but does not reload, and
// the reload after it serves it and reads nothing.
func TestRotationNeedsVerifyThenReload(t *testing.T) {
	const rotated = "sk-ant-oat01-ROTATED-0123456789-abcdef"
	h := newSetupHarness(t, true)
	reads := memoWired(t, h, true)
	served := func() string {
		c, _ := h.accounts.set.Load().lookup("sample-a").credential.(claudeCredential)
		return c.token
	}
	before := served()
	h.secrets.mu.Lock()
	for _, it := range h.secrets.items {
		if it.info.Title == "token-sample-a" {
			for i := range it.fields {
				if it.fields[i].title == "credential" {
					it.fields[i].value = rotated
				}
			}
		}
	}
	h.secrets.mu.Unlock()
	if _, err := h.writer.cfg.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := reads.taken(); len(got) != 0 || served() != before || before == rotated {
		t.Fatalf("a reload alone read %v and serves the rotated token: %v", got, served() == rotated)
	}
	if status, _, body := h.serve(t, request("POST", "/accounts/sample-a/verify", `{"rev":1}`)); status != 200 {
		t.Fatalf("verify: %d %s", status, body)
	}
	if got := reads.taken(); len(got) != 3 || served() != before {
		t.Fatalf("verify read %v; the served token changed: %v", got, served() != before)
	}
	if _, err := h.writer.cfg.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := reads.taken(); len(got) != 0 || served() != rotated {
		t.Fatalf("the reload after verify read %v, serves the rotated token: %v", got, served() == rotated)
	}
}
