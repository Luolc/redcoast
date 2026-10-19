package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Luolc/redcoast/session"
)

// TestPiLaunch checks pi's environment and its models.json: the agent directory,
// PI_OFFLINE, the openai-codex provider at the gateway with the credential named by
// its variable, never written to a file, and the arguments unchanged.
func TestPiLaunch(t *testing.T) {
	store, l, out := launchClient(t, "pi", "-p", "Hi there")
	dir := filepath.Join(l.home, ".config", "pi")
	writeFile(t, filepath.Join(dir, "models.json"), `{"providers": {"openai-codex": {"apiKey": "stale"}}}`)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if status := l.run(context.Background()); status != 0 {
		t.Fatalf("exit status %d; stderr %s", status, stderrOf(t, l))
	}
	env, args := recorded(t, out)
	token := checkCommonEnvironment(t, env)
	if env["PI_CODING_AGENT_DIR"] != dir || env["PI_OFFLINE"] != "1" {
		t.Fatalf("PI_CODING_AGENT_DIR = %q, PI_OFFLINE = %q", env["PI_CODING_AGENT_DIR"], env["PI_OFFLINE"])
	}
	if !slices.Equal(args, []string{"-p", "Hi there"}) {
		t.Fatalf("pi got %q", args)
	}
	data, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Providers map[string]map[string]string `json:"providers"`
	}
	if err := json.Unmarshal(data, &models); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{"openai-codex": {"baseUrl": "http://127.0.0.1:8789/backend-api", "apiKey": "${CODEX_GATEWAY_TOKEN}"}}
	if len(models.Providers) != 1 || len(models.Providers["openai-codex"]) != 2 || models.Providers["openai-codex"]["baseUrl"] != want["openai-codex"]["baseUrl"] || models.Providers["openai-codex"]["apiKey"] != want["openai-codex"]["apiKey"] {
		t.Fatalf("models.json %s", data)
	}
	noFileHolds(t, dir, token)
	if _, err := store.Bind(context.Background(), token, []string{"a"}); !errors.Is(err, session.ErrUnknownSession) {
		t.Fatalf("the session outlived pi: %v", err)
	}
}

// TestPiPreflight checks that a login pi would use or send instead of the session
// credential, an auth.json pi cannot read as an object, and --api-key are refused
// without showing a value, and that other logins and arguments are not.
func TestPiPreflight(t *testing.T) {
	tests := []struct {
		name string
		auth string // auth.json in the agent directory; empty for none
		args []string
		want string // substring of exactly one override; empty for none
	}{
		{"no_auth_file", "", []string{"-p", "Hi"}, ""},
		{"empty_auth_file", "{}", nil, ""},
		{"other_login", `{"anthropic": {"type": "api_key", "key": "synthetic-value"}}`, nil, ""},
		{"codex_login", `{"openai-codex": {"type": "oauth", "access": "synthetic-value"}}`, nil, "openai-codex login in HOME/.config/pi/auth.json"},
		{"openai_login", `{"openai": {"type": "api_key", "key": "synthetic-value"}}`, nil, "openai login in HOME/.config/pi/auth.json"},
		{"radius_login", `{"radius": {"type": "oauth", "access": "synthetic-value"}}`, nil, "radius login in HOME/.config/pi/auth.json"},
		{"not_an_object", `["synthetic-value"]`, nil, "HOME/.config/pi/auth.json is not a JSON object"},
		{"api_key", "", []string{"--api-key", "synthetic-value"}, "--api-key on the command line"},
		{"api_key_attached", "", []string{"--api-key=synthetic-value"}, "--api-key on the command line"},
		{"api_key_after_double_dash", "", []string{"--", "--api-key"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.auth != "" {
				writeFile(t, filepath.Join(home, ".config", "pi", "auth.json"), test.auth)
			}
			overrides, conflicts := piPreflight(nil, t.TempDir(), home, test.args)
			want := strings.ReplaceAll(test.want, "HOME", home)
			switch {
			case len(conflicts) != 0:
				t.Fatalf("conflicts %q", conflicts)
			case want == "" && len(overrides) != 0:
				t.Fatalf("overrides %q, want none", overrides)
			case want != "" && (len(overrides) != 1 || !strings.Contains(overrides[0], want)):
				t.Fatalf("overrides %q, want exactly one containing %q", overrides, want)
			case strings.Contains(strings.Join(overrides, "\n"), "synthetic-value"):
				t.Fatalf("overrides %q show a value", overrides)
			}
		})
	}
}

// TestPiRefusesLogin checks the whole launcher: a login in pi's agent directory stops
// the start before a session is issued, and pi never runs.
func TestPiRefusesLogin(t *testing.T) {
	_, l, out := launchClient(t, "pi")
	writeFile(t, filepath.Join(l.home, ".config", "pi", "auth.json"), `{"openai-codex": {"type": "oauth"}}`)
	if status := l.run(context.Background()); status != exitLauncherFailure {
		t.Fatalf("exit status %d, want %d", status, exitLauncherFailure)
	}
	if msg := stderrOf(t, l); !strings.Contains(msg, "refusing to start pi; the session credential would be overridden by:\n  - openai-codex login in ") {
		t.Fatalf("stderr %q", msg)
	}
	if _, err := os.Stat(out + ".env"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pi was started")
	}
}
