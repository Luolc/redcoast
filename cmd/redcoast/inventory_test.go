package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Luolc/redcoast/claude"
)

// inventoryFiles are two served accounts of an inventory directory, with references
// that only a synthetic resolver knows.
var inventoryFiles = map[string]string{
	"sample-a.yaml": "email: a@example.test\naccess: gateway\nstatus: active\noauth_token: op://example-vault/token-a/credential\nproxy:\n  host: proxy.example.test\n  port: 8080\n  expected_egress_ip: 192.0.2.1\n  username_ref: op://example-proxies/p/username\n  password_ref: op://example-proxies/p/password\n",
	"sample-b.yaml": "email: b@example.test\naccess: gateway\nstatus: paused\noauth_token: op://example-vault/token-b/credential\nproxy:\n  host: proxy.example.test\n  port: 8081\n  expected_egress_ip: 192.0.2.2\n  username_ref: op://example-proxies/p/username\n  password_ref: op://example-proxies/p/password\n",
}

// syntheticResolve resolves every reference to a made-up value.
func syntheticResolve(_ context.Context, reference string) (string, error) {
	if strings.HasPrefix(reference, "op://example-") {
		return "synthetic-value-" + strings.ReplaceAll(reference[len("op://"):], "/", "-"), nil
	}
	return "", errors.New("unknown reference")
}

// writeInventoryDir writes inventoryFiles into a fresh directory.
func writeInventoryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range inventoryFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLoadAccountsImportsOnce checks that the Claude process fills the accounts table
// from the inventory directory at its first start and reads the table afterwards, and
// that the Codex process reads the directory and leaves the table alone.
func TestLoadAccountsImportsOnce(t *testing.T) {
	dir := writeInventoryDir(t)
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "gateway.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := config{provider: "claude", inventory: dir, accountConcurrency: 1}
	set, err := loadAccounts(t.Context(), cfg, claude.Claude, store, syntheticResolve)
	if err != nil || !slices.Equal(set.Candidates(), []string{"sample-a"}) {
		t.Fatalf("first start serves %v, %v", set, err)
	}
	// The directory now describes another set; the table, already imported, wins.
	if err := os.Remove(filepath.Join(dir, "sample-a.yaml")); err != nil {
		t.Fatal(err)
	}
	set, err = loadAccounts(t.Context(), cfg, claude.Claude, store, syntheticResolve)
	if err != nil || !slices.Equal(set.Candidates(), []string{"sample-a"}) {
		t.Fatalf("second start serves %v, %v", set, err)
	}
	// Control: the Codex configuration reads the directory, where sample-a is gone.
	if _, err := loadAccounts(t.Context(), config{provider: "codex", inventory: dir, accountConcurrency: 1}, claude.Claude, store, syntheticResolve); err == nil {
		t.Fatal("the directory without a served account loaded")
	}
}

// TestLoadAccountsRefusedImportWritesNothing checks that an inventory directory that
// does not load stops the first start and leaves the table uninitialized.
func TestLoadAccountsRefusedImportWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample-a.yaml"), []byte("email: a@example.test\naccess: gateway\nstatus: active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.Context(), filepath.Join(t.TempDir(), "gateway.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := loadAccounts(t.Context(), config{provider: "claude", inventory: dir, accountConcurrency: 1}, claude.Claude, store, syntheticResolve); err == nil {
		t.Fatal("an inventory with a served account without a token loaded")
	}
	if _, initialized, err := store.Accounts(t.Context()); err != nil || initialized {
		t.Fatalf("table initialized = %v, %v", initialized, err)
	}
}

// TestInventoryExport checks the command line and the export over a store's file.
func TestInventoryExport(t *testing.T) {
	for _, arguments := range [][]string{{"export"}, {"export", "dir"}, {"export", "--session-db", "x"}, {"import", "--session-db", "x", "dir"}, {"export", "--session-db", "x", "dir", "more"}, {"export", "--session-db", "x", ""}, nil} {
		if _, err := parseInventoryCommand("claude", arguments); !errors.Is(err, errInventoryUsage) {
			t.Errorf("%q: got %v, want the usage", arguments, err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "gateway.sqlite")
	store, err := openStore(t.Context(), dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	out := filepath.Join(t.TempDir(), "export")
	cmd, err := parseInventoryCommand("claude", []string{"export", "--session-db", dbPath, out})
	if err != nil {
		t.Fatal(err)
	}
	// Before the import there is nothing to export, and nothing is written.
	if err := runInventory(t.Context(), cmd, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not imported") {
		t.Fatalf("export of an uninitialized table: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("export created its directory without a table")
	}
	if _, err := claude.ImportInventory(t.Context(), store, writeInventoryDir(t), claude.Claude); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := runInventory(t.Context(), cmd, &stdout); err != nil || !strings.Contains(stdout.String(), "exported 2 accounts") {
		t.Fatalf("export: %q, %v", stdout.String(), err)
	}
	exported, err := os.ReadFile(filepath.Join(out, "sample-b.yaml"))
	if err != nil || string(exported) != "email: b@example.test\naccess: gateway\nstatus: paused\noauth_token: op://example-vault/token-b/credential\nproxy:\n  host: proxy.example.test\n  port: 8081\n  expected_egress_ip: 192.0.2.2\n  username_ref: op://example-proxies/p/username\n  password_ref: op://example-proxies/p/password\n" {
		t.Fatalf("exported file: %q, %v", exported, err)
	}
	// The Codex process has no table to export.
	codexCmd := cmd
	codexCmd.provider = "codex"
	if err := runInventory(t.Context(), codexCmd, &bytes.Buffer{}); err == nil {
		t.Fatal("codex export ran")
	}
}

// TestAccountWriterIsClaudeOnly checks that the Claude process gets the dashboard's write
// path, built with the configured addresses and template, and the Codex process none.
func TestAccountWriterIsClaudeOnly(t *testing.T) {
	if writer := accountWriter(config{provider: "codex"}, nil, nil, nil, nil, nil, nil); writer != nil {
		t.Fatal("the Codex process has a write path")
	}
	if writer := accountWriter(config{provider: "claude", tokenRefTemplate: "op://example-vault/token-{alias}/credential"}, nil, nil, nil, nil, syntheticResolve, nil); writer == nil {
		t.Fatal("the Claude process has no write path")
	}
}
