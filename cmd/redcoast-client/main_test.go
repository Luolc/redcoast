package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Luolc/redcoast/internal/testtls"
	"github.com/Luolc/redcoast/session"
)

// writeFile writes content at path, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPreflight checks that a clean configuration passes and that each overriding
// variable, settings key and settings env entry is reported with where it was found,
// without reading or printing any value.
func TestPreflight(t *testing.T) {
	type setup func(t *testing.T, home, cwd string) (environ, args []string)
	clean := func(t *testing.T, home, cwd string) ([]string, []string) {
		t.Helper()
		writeFile(t, filepath.Join(home, ".claude.json"), `{"hasCompletedOnboarding": true, "other": 1}`)
		return []string{"PATH=/usr/bin", "HOME=" + home}, nil
	}
	tests := []struct {
		name  string
		setup setup
		want  string // substring of exactly one problem; empty for none
	}{
		{"clean", clean, ""},
		{"onboarding_missing", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude.json"), `{"hasCompletedOnboarding": false}`)
			return env, args
		}, "hasCompletedOnboarding is not true in " + "HOME/.claude.json"},
		{"state_file_absent", func(t *testing.T, home, cwd string) ([]string, []string) {
			return []string{"HOME=" + home}, nil
		}, "hasCompletedOnboarding is not true"},
		{"config_dir_moves_state_file", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			dir := filepath.Join(home, "elsewhere")
			writeFile(t, filepath.Join(dir, ".claude.json"), `{"hasCompletedOnboarding": true}`)
			writeFile(t, filepath.Join(dir, "settings.json"), `{"apiKeyHelper": "/bin/true"}`)
			return append(env, "CLAUDE_CONFIG_DIR="+dir), args
		}, "apiKeyHelper in user settings HOME/elsewhere/settings.json"},
		{"api_key_in_environment", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			return append(env, "ANTHROPIC_API_KEY=sk-ant-synthetic"), args
		}, "ANTHROPIC_API_KEY in the environment"},
		{"auth_token_in_environment", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			return append(env, "ANTHROPIC_AUTH_TOKEN="), args
		}, "ANTHROPIC_AUTH_TOKEN in the environment"},
		{"bedrock_in_environment", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			return append(env, "CLAUDE_CODE_USE_BEDROCK=1"), args
		}, "CLAUDE_CODE_USE_BEDROCK in the environment"},
		{"helper_in_user_settings", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"apiKeyHelper": "/bin/true"}`)
			return env, args
		}, "apiKeyHelper in user settings HOME/.claude/settings.json"},
		{"force_login_in_project_settings", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"forceLoginMethod": "claudeai"}`)
			return env, args
		}, "forceLoginMethod in project settings CWD/.claude/settings.json"},
		{"base_url_in_local_settings_env", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(cwd, ".claude", "settings.local.json"), `{"env": {"ANTHROPIC_BASE_URL": "http://example.test"}}`)
			return env, args
		}, "env.ANTHROPIC_BASE_URL in local settings CWD/.claude/settings.local.json"},
		{"api_key_in_settings_env", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"ANTHROPIC_API_KEY": "x"}}`)
			return env, args
		}, "env.ANTHROPIC_API_KEY in user settings"},
		{"lowercase_proxy_in_settings_env", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"https_proxy": "http://127.0.0.1:8791"}}`)
			return env, args
		}, "env.https_proxy in user settings"},
		{"all_proxy_in_settings_env", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"env": {"ALL_PROXY": "socks5://x"}}`)
			return env, args
		}, "env.ALL_PROXY in project settings"},
		{"local_settings_at_git_root_from_subdirectory", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			root := filepath.Dir(cwd) // cwd is a subdirectory of the repository (see the loop below)
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, ".claude", "settings.local.json"), `{"env": {"ANTHROPIC_AUTH_TOKEN": "synthetic"}}`)
			return env, args
		}, "env.ANTHROPIC_AUTH_TOKEN in local settings ROOT/.claude/settings.local.json"},
		{"harmless_local_settings_at_git_root", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			root := filepath.Dir(cwd)
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, ".claude", "settings.local.json"), `{"env": {"EDITOR": "vi"}}`)
			return env, args
		}, ""},
		{"local_settings_at_main_checkout_of_worktree", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			root := filepath.Dir(cwd)
			main := filepath.Join(home, "main-checkout")
			writeFile(t, filepath.Join(main, ".git", "worktrees", "wt", "gitdir"), "")
			writeFile(t, filepath.Join(root, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")
			writeFile(t, filepath.Join(main, ".claude", "settings.local.json"), `{"apiKeyHelper": "/bin/true"}`)
			return env, args
		}, "apiKeyHelper in local settings HOME/main-checkout/.claude/settings.local.json"},
		{"settings_flag_file", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, _ := clean(t, home, cwd)
			writeFile(t, filepath.Join(cwd, "extra.json"), `{"apiKeyHelper": "/bin/true"}`)
			return env, []string{"-p", "--settings", filepath.Join(cwd, "extra.json"), "Hi"}
		}, "apiKeyHelper in --settings CWD/extra.json"},
		{"settings_flag_inline", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, _ := clean(t, home, cwd)
			return env, []string{`--settings={"env":{"HTTPS_PROXY":"http://x"}}`}
		}, "env.HTTPS_PROXY in --settings"},
		{"unreadable_settings", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `not json`)
			return env, args
		}, "user settings HOME/.claude/settings.json is not a JSON object"},
		{"harmless_settings", func(t *testing.T, home, cwd string) ([]string, []string) {
			env, args := clean(t, home, cwd)
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"EDITOR": "vi"}, "model": "opus"}`)
			return env, args
		}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			root := t.TempDir()
			cwd := filepath.Join(root, "sub")
			if err := os.Mkdir(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			environ, args := test.setup(t, home, cwd)
			problems := preflight(environ, cwd, home, args)
			want := strings.NewReplacer("HOME", home, "CWD", cwd, "ROOT", root).Replace(test.want)
			switch {
			case want == "" && len(problems) != 0:
				t.Fatalf("clean configuration refused: %v", problems)
			case want != "" && (len(problems) != 1 || !strings.Contains(problems[0], want)):
				t.Fatalf("problems %v, want exactly one containing %q", problems, want)
			}
		})
	}
}

// TestChildEnvironmentOverTLS checks that a gateway whose reverse proxy is https gets
// an https forward proxy too, under the same server name.
func TestChildEnvironmentOverTLS(t *testing.T) {
	grant := session.Grant{SessionID: "S", Token: "sk-ant-gws-synthetic", Entrypoints: session.Entrypoints{ReverseProxy: "https://gateway.example.test:8789", ForwardProxy: "gateway.example.test:8791"}}
	env := environment(childEnvironment(nil, grant))
	proxy := "https://session:" + grant.Token + "@gateway.example.test:8791"
	if env["ANTHROPIC_BASE_URL"] != grant.ReverseProxy || env["HTTPS_PROXY"] != proxy || env["https_proxy"] != proxy || env["NO_PROXY"] != "gateway.example.test" {
		t.Fatalf("environment %v", env)
	}
}

// TestChildEnvironment checks that every proxy variable the parent had is gone, the
// gateway's variables are set from the grant, and unrelated variables survive.
func TestChildEnvironment(t *testing.T) {
	grant := session.Grant{SessionID: "S", Token: "sk-ant-gws-synthetic", Entrypoints: session.Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}}
	parent := []string{"PATH=/usr/bin", "ALL_PROXY=socks5://x", "all_proxy=socks5://x", "HTTP_PROXY=http://x", "http_proxy=http://x", "https_proxy=http://x", "NO_PROXY=old", "no_proxy=old", "DISABLE_AUTOUPDATER=0", "ANTHROPIC_BASE_URL=http://old", "EDITOR=vi"}
	env := environment(childEnvironment(parent, grant))
	for _, key := range []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		if _, present := env[key]; present {
			t.Fatalf("%s still set for claude", key)
		}
	}
	want := map[string]string{
		"PATH": "/usr/bin", "EDITOR": "vi",
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:8789", "CLAUDE_CODE_OAUTH_TOKEN": grant.Token,
		"HTTPS_PROXY": "http://session:" + grant.Token + "@127.0.0.1:8791", "https_proxy": "http://session:" + grant.Token + "@127.0.0.1:8791",
		"NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1", "DISABLE_AUTOUPDATER": "1",
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("%s = %q, want %q", key, env[key], value)
		}
	}
	if len(env) != len(want) {
		t.Fatalf("claude's environment has %d variables, want %d: %v", len(env), len(want), env)
	}
}

// fakeClaude is the stand-in for claude: it starts three sleeping grandchildren, one
// in its own process group and two detached into new sessions (as claude's Bash tool
// does), the last one under a command name containing ") " (FAKE_SLEEP_ODD), which a
// careless /proc parser misreads; writes its environment and the PIDs to the files
// named by FAKE_OUT; then exits 0, sleeps, ignores SIGTERM and sleeps, or leaves an
// orphan in a session of its own (its parent, a subshell, exits at once) and sleeps.
const fakeClaude = `#!/bin/sh
sleep 60 &
echo $! > "$FAKE_OUT.grandchild"
setsid sleep 60 &
echo $! > "$FAKE_OUT.detached"
setsid "$FAKE_SLEEP_ODD" 60 &
echo $! > "$FAKE_OUT.oddname"
env > "$FAKE_OUT.env"
echo $$ > "$FAKE_OUT.pid"
case "$1" in
  exit0) exit 0 ;;
  sleep) exec sleep 60 ;;
  ignore) trap '' TERM; exec sleep 60 ;;
  orphan) (setsid sleep 60 & echo $! > "$FAKE_OUT.orphan"); exec sleep 60 ;;
esac
exit 9
`

// launch starts the in-process session interface and a launcher whose claude is
// fakeClaude. It returns the store (to check the session), the launcher, the signal
// channel and the FAKE_OUT prefix.
func launch(t *testing.T, socketPresent bool, args ...string) (*session.Store, *launcher, chan os.Signal, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.Open(t.Context(), filepath.Join(dir, "gw.sqlite"), session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Account "a" needs a plan to be a candidate.
	if err := store.SetPlanSchedule(t.Context(), session.PlanPeriod{Alias: "a", Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	// The socket lives in a directory of its own under /tmp: a unix socket path is
	// limited to 108 bytes, and t.TempDir() grows with TMPDIR and the subtest's name.
	socketDir, err := os.MkdirTemp("/tmp", "gw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s.sock")
	if socketPresent {
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan error, 1)
		go func() {
			served <- session.NewServer(store, session.Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return []string{"a"} }).ServeUnix(ctx, socket)
		}()
		t.Cleanup(func() { cancel(); <-served })
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(socket); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("socket did not appear")
			}
		}
	}
	home := filepath.Join(dir, "home")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"hasCompletedOnboarding": true}`)
	claude := filepath.Join(dir, "claude")
	writeFile(t, claude, fakeClaude)
	if err := os.Chmod(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	oddSleep := filepath.Join(dir, "sleep) worker")
	if err := os.Symlink("/bin/sleep", oddSleep); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 2)
	// Real files, as in production: a descendant that inherits a pipe would otherwise
	// keep Wait from returning.
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdout.Close(); _ = stderr.Close() })
	l := &launcher{
		stdout: stdout, stderr: stderr, signals: signals, cwd: dir, home: home, args: args,
		environ: []string{"PATH=/usr/bin:/bin", "HOME=" + home, "REDCOAST_CLIENT_SOCKET=" + socket, "REDCOAST_CLIENT_CLAUDE=" + claude, "REDCOAST_CLIENT_MACHINE=test", "FAKE_OUT=" + out, "FAKE_SLEEP_ODD=" + oddSleep, "HTTP_PROXY=http://stale"},
	}
	return store, l, signals, out
}

// stderrOf returns what the launcher wrote to its stderr file.
func stderrOf(t *testing.T, l *launcher) string {
	t.Helper()
	data, err := os.ReadFile(l.stderr.(*os.File).Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// alive reports whether a process with pid exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitFile returns the content of path once it exists.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) != 0 {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", path)
		}
	}
}

// TestExits is E3.5 in process: after a normal exit, after claude is killed, and after
// the launcher receives SIGTERM, the session is revoked and claude's status is passed
// through; while claude runs (the control arm) the credential is still accepted. With
// no gateway socket the launcher fails without starting claude.
func TestExits(t *testing.T) {
	termGrace = 2 * time.Second
	for _, test := range []struct {
		name   string
		arg    string
		act    func(pid int, signals chan os.Signal)
		status int
	}{
		{"normal_exit", "exit0", nil, 0},
		{"claude_killed", "sleep", func(pid int, _ chan os.Signal) { _ = syscall.Kill(pid, syscall.SIGKILL) }, 137},
		{"launcher_terminated", "sleep", func(_ int, signals chan os.Signal) { signals <- syscall.SIGTERM }, 143},
		{"launcher_terminated_claude_ignores", "ignore", func(_ int, signals chan os.Signal) { signals <- syscall.SIGTERM }, 137},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, l, signals, out := launch(t, true, test.arg)
			started := make(chan int, 1)
			l.launched = func(pid int) { started <- pid }
			result := make(chan int, 1)
			go func() { result <- l.run(context.Background()) }()
			pid := <-started
			env := environment(strings.Split(strings.TrimSpace(waitFile(t, out+".env")), "\n"))
			token := env["CLAUDE_CODE_OAUTH_TOKEN"]
			if !strings.HasPrefix(token, session.TokenPrefix) || env["HTTPS_PROXY"] != "http://session:"+token+"@127.0.0.1:8791" || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8789" {
				t.Fatalf("claude's environment lacks the session: %v", env)
			}
			if _, present := env["HTTP_PROXY"]; present {
				t.Fatal("HTTP_PROXY reached claude")
			}
			grandchild, err := strconv.Atoi(strings.TrimSpace(waitFile(t, out+".grandchild")))
			if err != nil {
				t.Fatal(err)
			}
			detached, err := strconv.Atoi(strings.TrimSpace(waitFile(t, out+".detached")))
			if err != nil {
				t.Fatal(err)
			}
			oddName, err := strconv.Atoi(strings.TrimSpace(waitFile(t, out+".oddname")))
			if err != nil {
				t.Fatal(err)
			}
			if test.act != nil {
				// Control arm: the credential works and the grandchild lives while claude runs.
				if _, err := store.Bind(context.Background(), token, []string{"a"}); err != nil {
					t.Fatalf("credential refused while claude runs: %v", err)
				}
				if !alive(grandchild) || !alive(detached) || !alive(oddName) {
					t.Fatal("control arm: grandchildren not alive while claude runs")
				}
				test.act(pid, signals)
			}
			select {
			case status := <-result:
				if status != test.status {
					t.Fatalf("exit status %d, want %d; stderr: %s", status, test.status, stderrOf(t, l))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("launcher did not return after claude ended")
			}
			if _, err := store.Bind(context.Background(), token, []string{"a"}); !errors.Is(err, session.ErrUnknownSession) {
				t.Fatalf("credential still accepted after exit: %v", err)
			}
			if msg := stderrOf(t, l); msg != "" {
				t.Fatalf("launcher wrote to stderr: %s", msg)
			}
			if alive(pid) {
				t.Fatalf("claude process %d is still alive", pid)
			}
			for _, descendant := range []int{grandchild, detached, oddName} {
				for deadline := time.Now().Add(3 * time.Second); alive(descendant); time.Sleep(20 * time.Millisecond) {
					if time.Now().After(deadline) {
						_ = syscall.Kill(descendant, syscall.SIGKILL)
						t.Fatalf("descendant %d outlived the launcher", descendant)
					}
				}
			}
		})
	}
	t.Run("gateway_unreachable", func(t *testing.T) {
		_, l, _, out := launch(t, false, "exit0")
		if status := l.run(context.Background()); status != exitLauncherFailure {
			t.Fatalf("exit status %d, want %d", status, exitLauncherFailure)
		}
		if _, err := os.Stat(out + ".env"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("claude was started without a session")
		}
		if msg := stderrOf(t, l); !strings.Contains(msg, "no session from the gateway") {
			t.Fatalf("stderr %q does not name the cause", msg)
		}
	})
	t.Run("preflight_refusal_does_not_start_claude", func(t *testing.T) {
		_, l, _, out := launch(t, true, "exit0")
		l.environ = append(l.environ, "ANTHROPIC_API_KEY=synthetic")
		if status := l.run(context.Background()); status != exitLauncherFailure {
			t.Fatalf("exit status %d, want %d", status, exitLauncherFailure)
		}
		if _, err := os.Stat(out + ".env"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("claude was started despite the overriding variable")
		}
		if msg := stderrOf(t, l); !strings.Contains(msg, "ANTHROPIC_API_KEY in the environment") || strings.Contains(msg, "synthetic") {
			t.Fatalf("stderr %q: must name the variable and not its value", msg)
		}
	})
}

// TestOrphanReapedWhileClaudeRuns checks that a process adopted from claude's tree is
// reaped as soon as it dies, not when claude exits: a zombie keeps its process group
// alive, and a program that kills a group and waits for it to vanish (a test stopping
// Chrome) would wait in vain.
func TestOrphanReapedWhileClaudeRuns(t *testing.T) {
	termGrace = 2 * time.Second
	_, l, signals, out := launch(t, true, "orphan")
	result := make(chan int, 1)
	go func() { result <- l.run(context.Background()) }()
	orphan, err := strconv.Atoi(strings.TrimSpace(waitFile(t, out+".orphan")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(orphan, syscall.SIGKILL) })
	// Precondition: the orphan's parent is the launcher, so only the launcher can reap it.
	for deadline := time.Now().Add(5 * time.Second); parentOf(t, orphan) != os.Getpid(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("orphan %d was not adopted by the launcher", orphan)
		}
	}
	if err := syscall.Kill(orphan, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	reaped := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(-orphan, 0) == syscall.ESRCH {
			reaped = true
			break
		}
	}
	// End claude before asserting, so that a failure leaves nothing running.
	signals <- syscall.SIGTERM
	select {
	case status := <-result:
		if status != 143 {
			t.Fatalf("exit status %d, want 143; stderr: %s", status, stderrOf(t, l))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not return after SIGTERM")
	}
	if !reaped {
		t.Fatalf("process group %d still existed while claude ran; the orphan was not reaped", orphan)
	}
}

// parentOf returns pid's parent PID from /proc, or 0 when pid is gone.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	return ppid
}

// TestArgumentsPassThrough checks that claude receives the launcher's arguments unchanged.
func TestArgumentsPassThrough(t *testing.T) {
	_, l, _, out := launch(t, true, "exit0", "--settings", `{"model":"x"}`, "Hi there")
	writeFile(t, filepath.Join(l.cwd, "claude"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_OUT.args\"\n")
	if status := l.run(context.Background()); status != 0 {
		t.Fatalf("exit status %d; stderr %s", status, stderrOf(t, l))
	}
	got := strings.Split(strings.TrimSuffix(waitFile(t, out+".args"), "\n"), "\n")
	if !slices.Equal(got, []string{"exit0", "--settings", `{"model":"x"}`, "Hi there"}) {
		t.Fatalf("claude got %q", got)
	}
}

// TestRefusedByGateway checks the launcher when the gateway issues no session: with
// every account over hard it exits 3 and prints the gateway's quota message with the
// earliest recovery; with the only account refused by the upstream it prints the
// distinct no-account message; claude is never started in either case. Control arm:
// the same launcher with the account available starts claude.
func TestRefusedByGateway(t *testing.T) {
	// The launcher's store runs on the wall clock, so the reading's reset is derived from
	// it: an hour ahead, on a whole second, which the message prints back.
	reset := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	for _, test := range []struct {
		name  string
		setup func(store *session.Store)
		want  string
	}{
		{"quota_exhausted", func(store *session.Store) {
			if err := store.RecordQuota(t.Context(), "a", []session.Reading{{Window: session.Window7d, Utilization: 0.95, Status: "allowed_warning", ResetAt: reset, Source: "response"}}); err != nil {
				t.Fatal(err)
			}
		}, "redcoast: local gateway quota exhausted, every account is over its hard threshold or rate-limited, earliest recovery " + reset.Format(time.RFC3339) + "\n"},
		{"no_account", func(store *session.Store) {
			if err := store.Pause(t.Context(), "a", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "upstream_401"); err != nil {
				t.Fatal(err)
			}
		}, "redcoast: no account available for a new session (refused token, no plan, or no account configured); not a quota limit\n"},
		{"available_control", func(*session.Store) {}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, l, _, out := launch(t, true, "exit0")
			writeFile(t, filepath.Join(l.cwd, "claude"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_OUT.args\"\n")
			test.setup(store)
			status := l.run(context.Background())
			if test.want == "" {
				if status != 0 {
					t.Fatalf("control arm: exit status %d; stderr %s", status, stderrOf(t, l))
				}
				waitFile(t, out+".args")
				return
			}
			if status != exitLauncherFailure || stderrOf(t, l) != test.want {
				t.Fatalf("exit status %d; stderr %q, want %q", status, stderrOf(t, l), test.want)
			}
			if _, err := os.Stat(out + ".args"); err == nil {
				t.Fatal("claude was started")
			}
		})
	}
}

// TestNetworkMode checks the launcher's cross-machine mode: with REDCOAST_CLIENT_ADDRESS and a
// credential file of mode 0600 it gets a session over TCP as a registered machine and
// runs claude; a credential file readable by others, a missing file, an unregistered
// credential and both modes set at once fail before claude starts, and the stderr
// never contains the credential.
func TestNetworkMode(t *testing.T) {
	const credential = "machine-a-synthetic-credential-value"
	store, l, _, out := launch(t, false, "exit0")
	if err := store.AddMachine(t.Context(), "client-a", hashOf(credential), 0); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- session.NewServer(store, session.Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return []string{"a"} }).ServeTCP(ctx, address)
	}()
	t.Cleanup(func() { cancel(); <-served })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if conn, err := net.Dial("tcp", address); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not come up")
		}
	}
	// The same interface over TLS, with a certificate from a CA the system does not trust.
	tlsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{testtls.New(t).Certificate}}
	tlsServed := make(chan error, 1)
	go func() {
		tlsServed <- session.NewServer(store, session.Entrypoints{}, func() []string { return []string{"a"} }).ServeTCPListener(ctx, tls.NewListener(tlsListener, config))
	}()
	t.Cleanup(func() { cancel(); <-tlsServed })
	credentialFile := filepath.Join(l.cwd, "machine-credential")
	writeFile(t, credentialFile, credential+"\n")
	if err := os.Chmod(credentialFile, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongFile := filepath.Join(l.cwd, "wrong-credential")
	writeFile(t, wrongFile, "not-registered\n")
	if err := os.Chmod(wrongFile, 0o600); err != nil {
		t.Fatal(err)
	}
	openFile := filepath.Join(l.cwd, "open-credential")
	writeFile(t, openFile, credential+"\n")
	if err := os.Chmod(openFile, 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=/usr/bin:/bin", "HOME=" + l.home, "REDCOAST_CLIENT_CLAUDE=" + filepath.Join(l.cwd, "claude"), "FAKE_OUT=" + out}
	for _, test := range []struct {
		name string
		env  []string
		want string // a fragment of stderr; empty means claude runs
	}{
		{"registered", []string{"REDCOAST_CLIENT_ADDRESS=" + address, "REDCOAST_CLIENT_CREDENTIAL_FILE=" + credentialFile}, ""},
		{"unregistered", []string{"REDCOAST_CLIENT_ADDRESS=" + address, "REDCOAST_CLIENT_CREDENTIAL_FILE=" + wrongFile}, "redcoast: the machine credential matches no registered machine"},
		{"untrusted_tls", []string{"REDCOAST_CLIENT_ADDRESS=https://" + tlsListener.Addr().String(), "REDCOAST_CLIENT_CREDENTIAL_FILE=" + credentialFile}, "certificate signed by unknown authority"},
		{"readable_by_others", []string{"REDCOAST_CLIENT_ADDRESS=" + address, "REDCOAST_CLIENT_CREDENTIAL_FILE=" + openFile}, "readable by others"},
		{"missing_file", []string{"REDCOAST_CLIENT_ADDRESS=" + address, "REDCOAST_CLIENT_CREDENTIAL_FILE=" + filepath.Join(l.cwd, "absent")}, "machine credential file"},
		{"no_file", []string{"REDCOAST_CLIENT_ADDRESS=" + address}, "REDCOAST_CLIENT_ADDRESS needs REDCOAST_CLIENT_CREDENTIAL_FILE"},
		{"both_modes", []string{"REDCOAST_CLIENT_SOCKET=" + filepath.Join(l.cwd, "s.sock"), "REDCOAST_CLIENT_ADDRESS=" + address, "REDCOAST_CLIENT_CREDENTIAL_FILE=" + credentialFile}, "REDCOAST_CLIENT_SOCKET excludes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_ = os.Remove(out + ".args")
			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stderr.Close() }()
			run := *l
			run.stderr = stderr
			run.environ = append(append([]string{}, base...), test.env...)
			writeFile(t, filepath.Join(l.cwd, "claude"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_OUT.args\"\n")
			status := run.run(context.Background())
			if strings.Contains(stderrOf(t, &run), credential) {
				t.Fatal("stderr contains the credential")
			}
			if test.want == "" {
				if status != 0 {
					t.Fatalf("exit status %d; stderr %s", status, stderrOf(t, &run))
				}
				waitFile(t, out+".args")
				return
			}
			if status != exitLauncherFailure || !strings.Contains(stderrOf(t, &run), test.want) {
				t.Fatalf("exit status %d; stderr %q, want %q", status, stderrOf(t, &run), test.want)
			}
			if _, err := os.Stat(out + ".args"); err == nil {
				t.Fatal("claude was started")
			}
		})
	}
}

// hashOf is the SHA-256 of a credential in lowercase hex, as the client machine prints
// it for registration.
func hashOf(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

// TestClaudeArgs checks that the arguments after "claude" pass through and that a
// command line without it is refused.
func TestClaudeArgs(t *testing.T) {
	if args, ok := claudeArgs([]string{"claude", "-p", "claude"}); !ok || !slices.Equal(args, []string{"-p", "claude"}) {
		t.Fatalf("got %q %v", args, ok)
	}
	for _, arguments := range [][]string{nil, {"-p"}, {"codex", "claude"}} {
		if _, ok := claudeArgs(arguments); ok {
			t.Errorf("%q accepted", arguments)
		}
	}
}
