package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Luolc/redcoast/session"
)

// fakeRecorder is the stand-in for pi and codex: it writes its environment and its
// arguments, one per line, to the files named by FAKE_OUT and exits 0.
const fakeRecorder = "#!/bin/sh\n[ \"$1\" = --version ] && exit 0\nenv > \"$FAKE_OUT.env\"\nprintf '%s\\n' \"$@\" > \"$FAKE_OUT.args\"\n"

// inheritedForCodex are variables a parent shell may hold that must not reach pi or
// codex as they are.
var inheritedForCodex = []string{
	"HTTPS_PROXY=http://stale", "https_proxy=http://stale", "ALL_PROXY=socks5://stale", "all_proxy=socks5://stale", "http_proxy=http://stale", "NO_PROXY=old", "no_proxy=old",
	"PI_CODING_AGENT_DIR=/elsewhere/pi", "PI_OFFLINE=0", "CODEX_HOME=/elsewhere/codex", gatewayTokenVariable + "=stale",
	"EDITOR=vi",
}

// launchClient is launch for pi or codex: the launcher talks to the Codex process's
// variables, its executable is fakeRecorder, and the parent environment holds
// inheritedForCodex.
func launchClient(t *testing.T, name string, args ...string) (*session.Store, *launcher, string) {
	t.Helper()
	store, l, _, out := launch(t, true, args...)
	executable := filepath.Join(l.cwd, name)
	writeFile(t, executable, fakeRecorder)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	l.client = clients[name]
	var environ []string
	for _, entry := range l.environ {
		switch key, value, _ := strings.Cut(entry, "="); key {
		case "REDCOAST_CLIENT_SOCKET":
			environ = append(environ, "REDCOAST_CLIENT_CODEX_SOCKET="+value)
		case "REDCOAST_CLIENT_CLAUDE":
			environ = append(environ, "REDCOAST_CLIENT_"+strings.ToUpper(name)+"="+executable)
		default:
			environ = append(environ, entry)
		}
	}
	l.environ = append(environ, inheritedForCodex...)
	return store, l, out
}

// recorded returns the environment and arguments the fake client wrote.
func recorded(t *testing.T, out string) (map[string]string, []string) {
	t.Helper()
	env := environment(strings.Split(strings.TrimSpace(waitFile(t, out+".env")), "\n"))
	data, err := os.ReadFile(out + ".args")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		args = nil
	}
	return env, args
}

// checkCommonEnvironment checks what pi and codex share: the session credential in
// gatewayTokenVariable and in the forward proxy, NO_PROXY for the gateway and loopback,
// no inherited proxy variable, and the unrelated variables kept.
func checkCommonEnvironment(t *testing.T, env map[string]string) string {
	t.Helper()
	token := env[gatewayTokenVariable]
	if !strings.HasPrefix(token, session.TokenPrefix) {
		t.Fatalf("%s is not the session credential", gatewayTokenVariable)
	}
	proxy := "http://session:" + token + "@127.0.0.1:8791"
	want := map[string]string{"HTTPS_PROXY": proxy, "https_proxy": proxy, "NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost", "EDITOR": "vi"}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	for _, key := range []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		if _, present := env[key]; present {
			t.Errorf("%s reached the client", key)
		}
	}
	return token
}

// noFileHolds fails when a file under dir contains token.
func noFileHolds(t *testing.T, dir, token string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), token) {
			t.Errorf("%s holds the session credential", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCodexLaunch checks codex's environment and arguments: its own CODEX_HOME, the
// session credential only in the environment, --no-daemon and every -c before the
// arguments the launcher got. Started twice from the same CODEX_HOME, as two TUIs one
// after the other, the second gets its own session's credential, and both sessions end.
func TestCodexLaunch(t *testing.T) {
	store, l, out := launchClient(t, "codex", "exec", "Hi there")
	var tokens []string
	for run := range 2 {
		if status := l.run(context.Background()); status != 0 {
			t.Fatalf("run %d: exit status %d; stderr %s", run, status, stderrOf(t, l))
		}
		env, args := recorded(t, out)
		token := checkCommonEnvironment(t, env)
		dir := filepath.Join(l.home, ".config", "codex")
		if env["CODEX_HOME"] != dir {
			t.Fatalf("CODEX_HOME = %q, want %q", env["CODEX_HOME"], dir)
		}
		want := []string{
			"--no-daemon",
			"-c", `model_provider="gateway"`,
			"-c", `model_providers.gateway.name="gateway"`,
			"-c", `model_providers.gateway.base_url="http://127.0.0.1:8789/backend-api/codex"`,
			"-c", `model_providers.gateway.wire_api="responses"`,
			"-c", `model_providers.gateway.env_key="CODEX_GATEWAY_TOKEN"`,
			"-c", "model_providers.gateway.supports_websockets=false",
			"-c", "features.plugins=false",
			"-c", "features.shell_snapshot=false",
			"-c", "analytics.enabled=false",
			"-c", `otel.metrics_exporter="none"`,
			"-c", "check_for_update_on_startup=false",
			"-c", "feedback.enabled=false",
			"-c", `cli_auth_credentials_store="file"`,
			"exec", "Hi there",
		}
		if !slices.Equal(args, want) {
			t.Fatalf("codex got %q, want %q", args, want)
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("CODEX_HOME %v, %v; want mode 0700", info, err)
		}
		noFileHolds(t, dir, token)
		tokens = append(tokens, token)
	}
	if tokens[0] == tokens[1] {
		t.Fatal("the second start reused the first session's credential")
	}
	for _, token := range tokens {
		if _, err := store.Bind(context.Background(), token, []string{"a"}); !errors.Is(err, session.ErrUnknownSession) {
			t.Fatalf("a session outlived its codex: %v", err)
		}
	}
}

// TestCodexPrivateDirectory checks that a CODEX_HOME others can enter stops the start
// before codex runs, and ends the session the launcher got.
func TestCodexPrivateDirectory(t *testing.T) {
	_, l, out := launchClient(t, "codex")
	dir := filepath.Join(l.home, ".config", "codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if status := l.run(context.Background()); status != exitLauncherFailure {
		t.Fatalf("exit status %d, want %d", status, exitLauncherFailure)
	}
	if msg := stderrOf(t, l); !strings.Contains(msg, "is accessible by others; make it mode 0700") || strings.Contains(msg, "not revoked") {
		t.Fatalf("stderr %q", msg)
	}
	if _, err := os.Stat(out + ".env"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("codex was started")
	}
}

// TestCodexPreflight checks that a login or managed configuration in the CODEX_HOME,
// the options that pick another provider, and -c or --enable for a launcher setting at
// another value are refused, and that the same settings at the launcher's values, other
// settings, and anything after "--" are not.
func TestCodexPreflight(t *testing.T) {
	tests := []struct {
		name     string
		files    []string // under the CODEX_HOME
		args     []string
		override string // substring of exactly one override; empty for none
		conflict string // substring of exactly one conflict; empty for none
	}{
		{"clean", nil, []string{"exec", "Hi"}, "", ""},
		{"other_files", []string{"config.toml", "installation_id"}, nil, "", ""},
		{"login", []string{"auth.json"}, nil, "a login in HOME/.config/codex/auth.json", ""},
		{"managed_config", []string{"managed_config.toml"}, nil, "managed configuration HOME/.config/codex/managed_config.toml", ""},
		{"same_value", nil, []string{"-c", "analytics.enabled=false", "-c", "otel.metrics_exporter=none", "--config=feedback.enabled = false"}, "", ""},
		{"other_setting", nil, []string{"-c", "model=gpt-5.5", "--enable", "apps"}, "", ""},
		{"other_value", nil, []string{"-c", "analytics.enabled=true"}, "", "-c analytics.enabled=\"true\" on the command line; the launcher sets analytics.enabled=false"},
		{"attached_value", nil, []string{"-cfeedback.enabled=true"}, "", "-c feedback.enabled="},
		{"long_form", nil, []string{"--config=check_for_update_on_startup=true"}, "", "-c check_for_update_on_startup="},
		{"quoted_key", nil, []string{"-c", `"otel"."metrics_exporter"="statsig"`}, "", "-c otel.metrics_exporter="},
		{"table", nil, []string{"-c", "analytics={enabled=true}"}, "", "-c analytics=\"{enabled=true}\" on the command line; the launcher sets analytics.enabled=false"},
		{"after_subcommand", nil, []string{"exec", "-c", "feedback.enabled=true", "Hi"}, "", "-c feedback.enabled="},
		{"after_flag", nil, []string{"--search", "-c", "analytics.enabled=true"}, "", "-c analytics.enabled="},
		{"enable_plugins", nil, []string{"--enable=plugins"}, "", "--enable plugins on the command line; the launcher sets features.plugins=false"},
		{"enable_shell_snapshot", nil, []string{"exec", "--enable", "shell_snapshot"}, "", "--enable shell_snapshot on the command line; the launcher sets features.shell_snapshot=false"},
		{"provider", nil, []string{"-c", "model_provider=openai"}, "-c model_provider on the command line", ""},
		{"provider_field", nil, []string{"-c", "model_providers.gateway.base_url=http://x"}, "-c model_providers.gateway.base_url on the command line", ""},
		{"oss", nil, []string{"--oss", "exec", "Hi"}, "--oss on the command line", ""},
		{"local_provider", nil, []string{"--local-provider", "ollama"}, "--local-provider on the command line", ""},
		{"remote", nil, []string{"--remote=ws://127.0.0.1:1"}, "--remote on the command line", ""},
		{"after_double_dash", nil, []string{"exec", "--", "-c", "analytics.enabled=true", "--oss"}, "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			for _, name := range test.files {
				writeFile(t, filepath.Join(home, ".config", "codex", name), "{}")
			}
			overrides, conflicts := codexPreflight(nil, t.TempDir(), home, test.args)
			for _, group := range []struct {
				name     string
				problems []string
				want     string
			}{{"overrides", overrides, test.override}, {"conflicts", conflicts, test.conflict}} {
				want := strings.ReplaceAll(group.want, "HOME", home)
				switch {
				case want == "" && len(group.problems) != 0:
					t.Errorf("%s %q, want none", group.name, group.problems)
				case want != "" && (len(group.problems) != 1 || !strings.Contains(group.problems[0], want)):
					t.Errorf("%s %q, want exactly one containing %q", group.name, group.problems, want)
				}
			}
		})
	}
}

// TestCodexOverTLS checks that an https gateway gets an https forward proxy and a
// NO_PROXY naming its host before loopback, and that a reverse proxy that is not an
// origin is refused before anything is written.
func TestCodexOverTLS(t *testing.T) {
	home := t.TempDir()
	l := &launcher{home: home, environ: []string{"PATH=/usr/bin"}}
	grant := session.Grant{SessionID: "S", Token: "header.payload.signature", Entrypoints: session.Entrypoints{ReverseProxy: "https://gateway.example.test:8792/", ForwardProxy: "gateway.example.test:8793"}}
	envList, args, err := prepareCodex(l, grant)
	if err != nil {
		t.Fatal(err)
	}
	env := environment(envList)
	proxy := "https://session:" + grant.Token + "@gateway.example.test:8793"
	if env["HTTPS_PROXY"] != proxy || env["NO_PROXY"] != "gateway.example.test,127.0.0.1,localhost" || !slices.Contains(args, `model_providers.gateway.base_url="https://gateway.example.test:8792/backend-api/codex"`) {
		t.Fatalf("environment %v, arguments %q", env, args)
	}
	for _, reverse := range []string{"https://gateway.example.test:8792/v1", `http://gate"way:1`, "ftp://gateway.example.test", "http://user:pass@gateway.example.test:1"} {
		grant.ReverseProxy = reverse
		if _, _, err := prepareCodex(&launcher{home: t.TempDir()}, grant); err == nil {
			t.Errorf("reverse proxy %q accepted", reverse)
		}
	}
}
