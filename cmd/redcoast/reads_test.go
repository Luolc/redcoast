package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// sdkLog stands in for the 1Password SDK: it counts the clients opened and the
// references read, and never reaches a vault.
type sdkLog struct {
	mu       sync.Mutex
	connects int
	refs     []string
}

func (l *sdkLog) connect(context.Context, string) (*onepassword.Client, claude.Resolver, error) {
	l.mu.Lock()
	l.connects++
	l.mu.Unlock()
	return nil, func(ctx context.Context, reference string) (string, error) {
		l.mu.Lock()
		l.refs = append(l.refs, reference)
		l.mu.Unlock()
		return syntheticResolve(ctx, reference)
	}, nil
}

// taken returns the sorted references read since the last call and forgets them.
func (l *sdkLog) taken() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	refs := slices.Clone(l.refs)
	l.refs = nil
	slices.Sort(refs)
	return refs
}

// syntheticCredentials is credentials over a counting SDK with a service account token
// in a file, which tests can change.
func syntheticCredentials(t *testing.T) (*credentials, *sdkLog, string) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("synthetic-service-token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := newCredentials(config{tokenRef: "file://" + tokenFile})
	sdk := &sdkLog{}
	creds.connect = sdk.connect
	return creds, sdk, tokenFile
}

// TestCredentialsReuseTheClientAndTheValues checks that one client serves every
// resolver and the secret store while the service account token is the same, and a new
// one is opened when the token changes; that a resolver reads an op:// reference once and
// a fresh resolver reads it again; and that env:// is read each time.
func TestCredentialsReuseTheClientAndTheValues(t *testing.T) {
	creds, sdk, tokenFile := syntheticCredentials(t)
	const ref = "op://example-vault/item/field"
	for i := range 3 {
		want := fmt.Sprintf("synthetic-env-%d", i)
		t.Setenv("GATEWAY_TEST_VALUE", want)
		resolve, err := creds.resolver(t.Context(), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if value, err := resolve(t.Context(), "env://GATEWAY_TEST_VALUE"); err != nil || value != want {
			t.Fatalf("env: %q %v, want %q", value, err, want)
		}
	}
	if _, err := creds.secretStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := sdk.taken(); !slices.Equal(got, []string{ref}) || sdk.connects != 1 {
		t.Fatalf("three resolvers and the store: reads %v, clients %d; want one read and one client", got, sdk.connects)
	}
	fresh, err := creds.resolver(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if got := sdk.taken(); !slices.Equal(got, []string{ref}) {
		t.Fatalf("a fresh resolver read %v, want the reference again", got)
	}
	if err := os.WriteFile(tokenFile, []byte("synthetic-service-token-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.resolver(t.Context(), false); err != nil || sdk.connects != 2 {
		t.Fatalf("a rotated service account token: %v, clients %d; want 2", err, sdk.connects)
	}
}

// reloadFixture is an inventory of three served accounts behind refused exits (the
// egress check of an account that joins fails fast and sets it aside), loaded the way a
// start loads it.
type reloadFixture struct {
	cfg      config
	store    *session.Store
	accounts *claude.Accounts
	reload   func(context.Context) (claude.Reload, error)
	sdk      *sdkLog
}

func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	dir := t.TempDir()
	for i, alias := range []string{"a", "b", "c"} {
		doc := fmt.Sprintf("email: %s@example.test\naccess: gateway\nstatus: active\noauth_token: op://example-vault/token-%s/credential\nproxy:\n  host: 127.0.0.1\n  port: 1\n  expected_egress_ip: 192.0.2.%d\n  username_ref: op://example-proxies/p/username\n  password_ref: op://example-proxies/p/password\n", alias, alias, i+1)
		if err := os.WriteFile(filepath.Join(dir, "sample-"+alias+".yaml"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "gateway.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &reloadFixture{store: store}
	f.cfg = config{provider: "claude", inventory: dir, accountConcurrency: 1}
	creds, sdk, _ := syntheticCredentials(t)
	f.sdk = sdk
	f.cfg.tokenRef = creds.cfg.tokenRef
	creds.cfg = f.cfg
	resolve, err := creds.resolver(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadAccounts(t.Context(), f.cfg, claude.Claude, store, resolve)
	if err != nil {
		t.Fatal(err)
	}
	f.accounts = claude.NewAccounts(set, store)
	upstream, _ := url.Parse("https://upstream.example.test")
	f.reload = reloader(f.cfg, store, f.accounts, creds, claude.NewEgress(f.accounts, store, upstream))
	return f
}

// setStatus changes the status of an account of the table.
func (f *reloadFixture) setStatus(t *testing.T, alias, status string) {
	t.Helper()
	row, err := f.store.Account(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateAccount(t.Context(), alias, row.Rev, session.AuditEntry{Action: "test"}, func(r *session.AccountRow) error { r.Status = status; return nil }); err != nil {
		t.Fatal(err)
	}
}

// TestReloadReadsOnlyWhatIsNew checks that a reload reads from 1Password only the
// references that no earlier load read: the start reads the three tokens and the proxy
// login once; a reload with nothing new and a reload that sets an account aside read
// nothing; an account that joins again reads its own token, because the reload that
// dropped it forgot the value, and not the login the others still use.
func TestReloadReadsOnlyWhatIsNew(t *testing.T) {
	f := newReloadFixture(t)
	const (
		user, pass = "op://example-proxies/p/username", "op://example-proxies/p/password"
		tokenB     = "op://example-vault/token-b/credential"
	)
	if got := f.sdk.taken(); len(got) != 5 {
		t.Fatalf("the start read %v, want the three tokens and the proxy login", got)
	}
	if report, err := f.reload(t.Context()); err != nil || len(report.Added)+len(report.Removed)+len(report.Changed) != 0 {
		t.Fatalf("reload with nothing new: %+v %v", report, err)
	}
	if got := f.sdk.taken(); len(got) != 0 {
		t.Fatalf("a reload with nothing new read %v", got)
	}
	f.setStatus(t, "sample-b", "paused")
	if report, err := f.reload(t.Context()); err != nil || !slices.Equal(report.Removed, []string{"sample-b"}) {
		t.Fatalf("reload without b: %+v %v", report, err)
	}
	if got := f.sdk.taken(); len(got) != 0 {
		t.Fatalf("a reload that set an account aside read %v", got)
	}
	f.setStatus(t, "sample-b", "active")
	if _, err := f.reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.sdk.taken(); !slices.Equal(got, []string{tokenB}) {
		t.Fatalf("an account that joined again: read %v, want only %s", got, tokenB)
	}
}

// TestStartupExitStatus checks that a startup that fails because 1Password was asked too
// often exits with the status a unit can keep from restarting on, whichever read failed
// and however the error was wrapped, and that any other failure exits with 1.
func TestStartupExitStatus(t *testing.T) {
	limited := &onepassword.RateLimitExceededError{}
	other := errors.New("synthetic failure")
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "gateway.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	dir := writeInventoryDir(t)
	cfg := config{provider: "claude", inventory: dir, accountConcurrency: 1}
	backupCfg := config{backup: &backupConfig{Endpoint: "https://s3.us-east-1.amazonaws.com", Bucket: "bucket", Region: "us-east-1", AccessKeyID: "op://example-vault/b/id", SecretAccessKey: "op://example-vault/b/key"}}
	codexCfg := config{provider: "codex", codex: &codexFile{AccountsDir: "/accounts", SessionKey: "op://example-vault/k/key"}}
	// The resolver as the gateway has it: retries first, the rate limit unchanged once they run out.
	exhausted := retryRateLimited(func(context.Context, string) (string, error) { return "", limited }, []time.Duration{time.Millisecond}, 0)
	for _, test := range []struct {
		name    string
		resolve claude.Resolver
		want    int
	}{
		{"rate_limited_after_retries", exhausted, exitOnePasswordQuota},
		{"rate_limited_sentinel", func(context.Context, string) (string, error) { return "", credential.ErrRateLimited }, exitOnePasswordQuota},
		{"other_failure", func(context.Context, string) (string, error) { return "", other }, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, loadErr := loadAccounts(t.Context(), cfg, claude.Claude, store, test.resolve)
			_, _, providerErr := newProvider(t.Context(), codexCfg, test.resolve)
			_, backupErr := newBackup(t.Context(), backupCfg, store, test.resolve)
			for step, err := range map[string]error{"load accounts": errors.Join(loadErr, errors.New("synthetic store close failure")), "codex key": providerErr, "backup key": backupErr} {
				if err == nil {
					t.Fatalf("%s did not fail", step)
				}
				if got := exitStatus(err); got != test.want {
					t.Errorf("%s: status %d, want %d (%v)", step, got, test.want, err)
				}
			}
		})
	}
}
