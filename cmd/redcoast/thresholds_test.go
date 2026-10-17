package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Luolc/redcoast/session"
)

// TestThresholdsCommand checks that the thresholds subcommand writes both values the
// operator gives, and that the store's refusal of a bad pair reaches the command.
func TestThresholdsCommand(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gateway.sqlite")
	ctx := context.Background()
	for _, arguments := range [][]string{
		{"--session-db", db, "0.85", "0.97"},
		{"0.9", "0.95", "--session-db", db},
	} {
		cmd, err := parseThresholdsCommand(arguments)
		if err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if err := runThresholds(ctx, cmd); err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
	}
	cmd, err := parseThresholdsCommand([]string{"--session-db", db, "0.95", "0.9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runThresholds(ctx, cmd); err == nil {
		t.Fatal("soft above hard accepted")
	}
	store, err := session.Open(ctx, db, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if soft, hard, err := store.Thresholds(ctx); err != nil || soft != 0.9 || hard != 0.95 {
		t.Fatalf("soft=%v hard=%v err=%v", soft, hard, err)
	}
	for _, bad := range [][]string{nil, {"0.9", "0.95"}, {"--session-db", db, "0.9"}, {"--session-db", db, "0.9", "0.95", "1"}} {
		if _, err := parseThresholdsCommand(bad); !errors.Is(err, errThresholdsUsage) {
			t.Fatalf("%v: got %v, want the usage", bad, err)
		}
	}
}
