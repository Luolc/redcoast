package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// script writes an executable shell script with body and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client")
	writeFile(t, path, "#!/bin/sh\n"+body)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClientVersion checks that only the version number of the --version output is
// kept, and that a client that fails, prints no version or does not finish in time
// gives no version, at once, even when a descendant keeps its output open.
func TestClientVersion(t *testing.T) {
	versionTimeout = 500 * time.Millisecond
	environ := []string{"PATH=/usr/bin:/bin"}
	for _, test := range []struct{ name, body, want string }{
		{"claude", `echo "2.1.0 (Claude Code)"`, "2.1.0"},
		{"codex", `echo "codex-cli 0.46.0"`, "0.46.0"},
		{"pre_release", `echo "v1.2.0-beta.1"; echo "3.0.0"`, "1.2.0-beta.1"},
		{"no_version", `echo "unknown option --version"`, ""},
		{"version_on_stderr", `echo "2.1.0" >&2`, ""},
		{"fails", `echo "2.1.0"; exit 1`, ""},
		{"hangs", `exec sleep 60`, ""},
		{"descendant_holds_output", `echo "2.1.0"; sleep 60 &`, "2.1.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			if got := clientVersion(context.Background(), script(t, test.body), environ); got != test.want {
				t.Fatalf("clientVersion = %q, want %q", got, test.want)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("clientVersion took %v", elapsed)
			}
		})
	}
}

// TestLaunchMeta checks that the launch metadata names the client program and its
// version, that each program is one the gateway accepts, and that a working directory
// that would push the metadata past the gateway's limit is left out.
func TestLaunchMeta(t *testing.T) {
	for name, c := range clients {
		if !slices.Contains(session.ClientPrograms, c.program) {
			t.Fatalf("client %s names program %q, which the gateway does not accept", name, c.program)
		}
	}
	executable := script(t, `echo "codex-cli 0.46.0"`)
	for _, test := range []struct {
		name    string
		cwd     string
		wantCwd bool
	}{
		{"short_cwd", "/home/someone/project", true},
		{"cwd_over_the_limit", "/" + strings.Repeat("d", session.MaxLaunchMeta), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := &launcher{client: clients["codex"], executable: executable, cwd: test.cwd, environ: []string{"PATH=/usr/bin:/bin", "HERDR_PANE_ID=p1"}}
			data := l.launchMeta(context.Background())
			if len(data) > session.MaxLaunchMeta {
				t.Fatalf("launch metadata of %d bytes exceeds the limit", len(data))
			}
			var meta map[string]any
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
			if meta["client"] != "codex-cli" || meta["client_version"] != "0.46.0" || meta["herdr_pane_id"] != "p1" || meta["launcher_pid"] == nil {
				t.Fatalf("launch metadata = %s", data)
			}
			if _, has := meta["cwd"]; has != test.wantCwd {
				t.Fatalf("launch metadata = %s; cwd present %v, want %v", data, has, test.wantCwd)
			}
		})
	}
}
