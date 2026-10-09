package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheck checks the shapes each scheme accepts and refuses.
func TestCheck(t *testing.T) {
	for _, test := range []struct {
		reference string
		valid     bool
	}{
		{"op://example-vault/token-example/credential", true},
		{"op://example-vault/aws-backup-writer/section/access key id", true},
		{"env://GATEWAY_TOKEN", true},
		{"file:///run/credentials/redcoast.service/op-sa-token", true},
		{"op://example-vault/item", false},
		{"op://example-vault/item/ field", false},
		{"op://example-vault/item/access\tkey", false},
		{"op://example-vault/item/access\nkey", false},
		{"env://", false},
		{"env://1TOKEN", false},
		{"file://relative/path", false},
		{"file:///run/../etc/shadow", false},
		{"file:///", false},
		{"https://example.test/token", false},
		{"token-example", false},
	} {
		if err := Check(test.reference); (err == nil) != test.valid {
			t.Errorf("%q: got %v, want valid=%v", test.reference, err, test.valid)
		}
	}
}

// TestResolve resolves one reference of each scheme and refuses the failures of each.
func TestResolve(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "blank")
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIAL_TEST_SET", "env-value")
	t.Setenv("CREDENTIAL_TEST_EMPTY", "")
	vault := func(_ context.Context, reference string) (string, error) {
		if reference == "op://example-vault/item/field" {
			return "op-value", nil
		}
		return "", errors.New("not in the vault")
	}
	resolve := New(vault)
	for _, test := range []struct {
		reference, want string
	}{
		{"op://example-vault/item/field", "op-value"},
		{"env://CREDENTIAL_TEST_SET", "env-value"},
		{"file://" + token, "file-value"},
	} {
		if got, err := resolve(t.Context(), test.reference); err != nil || got != test.want {
			t.Errorf("%s: got %q %v, want %q", test.reference, got, err, test.want)
		}
	}
	for _, reference := range []string{
		"op://example-vault/item/other",
		"env://CREDENTIAL_TEST_EMPTY",
		"env://CREDENTIAL_TEST_UNSET",
		"file://" + filepath.Join(dir, "missing"),
		"file://" + blank,
		"file://" + large,
		"token",
	} {
		if got, err := resolve(t.Context(), reference); err == nil {
			t.Errorf("%s: resolved to %q", reference, got)
		}
	}
	if _, err := New(nil)(t.Context(), "op://example-vault/item/field"); err == nil {
		t.Error("op:// resolved without 1Password")
	}
}
