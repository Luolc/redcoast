package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
)

// TestStdinResolver checks which stdin inputs the test entry accepts and that the
// resolver it returns knows exactly the references in the object.
func TestStdinResolver(t *testing.T) {
	valid := `{"op://synthetic/sample-a/token":"synthetic-value-token","op://synthetic/sample-a/proxy_username":"synthetic-value-user"}`
	for _, test := range []struct {
		name  string
		input string
		valid bool
	}{
		{"two_references", valid, true},
		{"empty_object", `{}`, false},
		{"not_an_object", `["op://synthetic/sample-a/token"]`, false},
		{"value_not_a_string", `{"op://synthetic/sample-a/token":1}`, false},
		{"second_object", valid + valid, false},
		{"too_large", `{"op://synthetic/sample-a/token":"` + strings.Repeat("x", maxCredentialsInput) + `"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolve, err := stdinResolver(t.Context(), io.NopCloser(strings.NewReader(test.input)))
			if !test.valid {
				if err == nil || err.Error() != "invalid credentials input" {
					t.Fatalf("got %v, want the fixed diagnostic", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := resolve(t.Context(), "op://synthetic/sample-a/token")
			if err != nil || value != "synthetic-value-token" {
				t.Fatalf("got %q %v, want the value from stdin", value, err)
			}
			if _, err := resolve(t.Context(), "op://synthetic/sample-b/token"); err == nil {
				t.Fatal("unknown reference resolved")
			}
		})
	}
}

// TestStdinResolverCancellation checks that a stalled stdin stops at the deadline.
func TestStdinResolverCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error("private input writer close failed")
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if resolve, err := stdinResolver(ctx, reader); resolve != nil || err == nil || err.Error() != "invalid credentials input" {
		t.Fatal("stalled private input did not stop with a fixed diagnostic")
	}
}

// TestNewResolverToken checks the token reference failures that need no 1Password: a
// missing file and an empty one are refused. Without a token, env:// still resolves and
// op:// is refused.
func TestNewResolverToken(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "missing"), "empty": empty} {
		t.Run(name, func(t *testing.T) {
			resolve, err := newCredentials(config{tokenRef: "file://" + path}).resolver(t.Context(), false)
			if resolve != nil || err == nil || !strings.HasPrefix(err.Error(), "1Password service account token: ") {
				t.Fatalf("got %v, want a token error", err)
			}
		})
	}
	t.Setenv("GATEWAY_TEST_TOKEN", "synthetic-value")
	resolve, err := newCredentials(config{}).resolver(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := resolve(t.Context(), "env://GATEWAY_TEST_TOKEN"); err != nil || value != "synthetic-value" {
		t.Fatalf("env:// without 1Password: %q %v", value, err)
	}
	if _, err := resolve(t.Context(), "op://example-vault/item/field"); err == nil {
		t.Fatal("op:// resolved without 1Password")
	}
}

// syntheticSecrets is a SecretsAPI that reaches no vault.
type syntheticSecrets struct{}

// Resolve returns a synthetic value for any reference.
func (syntheticSecrets) Resolve(context.Context, string) (string, error) {
	return "synthetic-value", nil
}

// ResolveAll is unused here.
func (syntheticSecrets) ResolveAll(context.Context, []string) (onepassword.ResolveAllResponse, error) {
	return onepassword.ResolveAllResponse{}, nil
}

// TestClientResolverKeepsClientAlive checks that the resolver keeps the SDK Client
// reachable: a finalizer on the Client does not run while the resolver is in use. The
// control arm is the method value the SDK hands out, whose Client is collected.
func TestClientResolverKeepsClientAlive(t *testing.T) {
	for _, test := range []struct {
		name          string
		resolver      func(*onepassword.Client) claude.Resolver
		wantFinalized bool
	}{
		{"closure_keeps_client", clientResolver, false},
		{"method_value_drops_client", func(c *onepassword.Client) claude.Resolver { return c.Secrets().Resolve }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			finalized := make(chan struct{})
			resolve := func() claude.Resolver {
				client := &onepassword.Client{SecretsAPI: syntheticSecrets{}}
				runtime.SetFinalizer(client, func(*onepassword.Client) { close(finalized) })
				return test.resolver(client)
			}()
			var got bool
			for range 5 {
				runtime.GC()
				select {
				case <-finalized:
					got = true
				case <-time.After(50 * time.Millisecond):
				}
				if got {
					break
				}
			}
			if value, err := resolve(t.Context(), "op://synthetic/account/credential"); err != nil || value != "synthetic-value" {
				t.Fatalf("resolver unusable: %q %v", value, err)
			}
			if got != test.wantFinalized {
				t.Fatalf("client finalized while the resolver was live: %t, want %t", got, test.wantFinalized)
			}
			runtime.KeepAlive(resolve)
		})
	}
}

// sequence returns a resolver that fails with each of errs in turn, then gives "value",
// and the number of calls it has had.
func sequence(errs ...error) (claude.Resolver, *int) {
	calls := new(int)
	return func(context.Context, string) (string, error) {
		defer func() { *calls++ }()
		if *calls < len(errs) {
			return "", errs[*calls]
		}
		return "value", nil
	}, calls
}

var testBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

// TestRetryRateLimited: a rate limited resolution is retried up to three times and then
// gives the error as it was; other errors are not retried.
func TestRetryRateLimited(t *testing.T) {
	limited := &onepassword.RateLimitExceededError{}
	t.Run("recovers within the retries", func(t *testing.T) {
		inner, calls := sequence(limited, limited, limited)
		got, err := retryRateLimited(inner, testBackoff, 0)(t.Context(), "op://v/i/f")
		if err != nil || got != "value" || *calls != 4 {
			t.Fatalf("got %q, %v after %d calls; want value after 4", got, err, *calls)
		}
	})
	t.Run("gives up after three retries", func(t *testing.T) {
		inner, calls := sequence(limited, limited, limited, limited, limited)
		_, err := retryRateLimited(inner, testBackoff, 0)(t.Context(), "op://v/i/f")
		if err != limited || *calls != 4 {
			t.Fatalf("got %v after %d calls; want the rate limit error after 4", err, *calls)
		}
	})
	t.Run("other errors are not retried", func(t *testing.T) {
		other := errors.New("no such item")
		inner, calls := sequence(other)
		if _, err := retryRateLimited(inner, testBackoff, 0)(t.Context(), "op://v/i/f"); err != other || *calls != 1 {
			t.Fatalf("got %v after %d calls; want the error after 1", err, *calls)
		}
	})
	t.Run("after the retries ran out, later references are not retried for a while", func(t *testing.T) {
		inner, calls := sequence(limited, limited, limited, limited, limited, limited)
		resolve := retryRateLimited(inner, testBackoff, time.Hour)
		_, _ = resolve(t.Context(), "op://v/i/a")
		if _, err := resolve(t.Context(), "op://v/i/b"); err != limited || *calls != 5 {
			t.Fatalf("got %v after %d calls; want the rate limit error after 5 (4 and 1)", err, *calls)
		}
	})
	t.Run("a canceled context ends the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		inner, calls := sequence(limited, limited)
		cancelling := func(ctx context.Context, ref string) (string, error) {
			cancel()
			return inner(ctx, ref)
		}
		start := time.Now()
		_, err := retryRateLimited(cancelling, []time.Duration{time.Hour}, 0)(ctx, "op://v/i/f")
		if !errors.Is(err, context.Canceled) || !isRateLimited(err) || *calls != 1 || time.Since(start) > time.Minute {
			t.Fatalf("got %v after %d calls in %v; want a cancel at once after 1", err, *calls, time.Since(start))
		}
	})
}

// TestReadErrorMarksRateLimit: the store's reads tell a rate limit from other failures.
func TestReadErrorMarksRateLimit(t *testing.T) {
	if err := readError(fmt.Errorf("wrapped: %w", &onepassword.RateLimitExceededError{})); !errors.Is(err, credential.ErrRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}
	other := errors.New("other")
	if err := readError(other); err != other {
		t.Fatalf("other: %v", err)
	}
}
