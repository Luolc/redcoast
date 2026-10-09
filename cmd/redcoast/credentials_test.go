package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
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
			resolve, err := newResolver(t.Context(), config{tokenRef: "file://" + path})
			if resolve != nil || err == nil || !strings.HasPrefix(err.Error(), "1Password service account token: ") {
				t.Fatalf("got %v, want a token error", err)
			}
		})
	}
	t.Setenv("GATEWAY_TEST_TOKEN", "synthetic-value")
	resolve, err := newResolver(t.Context(), config{})
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
