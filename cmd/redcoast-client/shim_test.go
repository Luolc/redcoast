package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFullShimWithTheBuild runs the full shim in front of a real build
// of redcoast-client, configured with an unreachable gateway: from a deleted working
// directory redcoast-client stops at os.Getwd, from an existing one it gets as far as
// asking the gateway. Neither starts claude.
func TestFullShimWithTheBuild(t *testing.T) {
	root := t.TempDir()
	bin, home, cwd := filepath.Join(root, "bin"), filepath.Join(root, "home"), filepath.Join(root, "work")
	shim, err := os.ReadFile("redcoast-client-shim-full")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(bin, "claude"), string(shim))
	marker := filepath.Join(root, "claude-started")
	writeFile(t, filepath.Join(root, "real", "claude"), "#!/bin/sh\ntouch "+marker+"\n")
	for _, path := range []string{filepath.Join(bin, "claude"), filepath.Join(root, "real", "claude")} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "redcoast-client"), ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(home, ".claude.json"), `{"hasCompletedOnboarding": true}`)
	writeFile(t, filepath.Join(home, ".config", "redcoast-client", "machine-credential"), "synthetic\n")
	writeFile(t, filepath.Join(home, ".config", "redcoast-client", "shim.env"), "address=127.0.0.1:1\n"+
		"credential_file=$HOME/.config/redcoast-client/machine-credential\n"+
		"claude="+filepath.Join(root, "real", "claude")+"\nprefix=\n")

	tests := []struct {
		name   string
		script string // run by sh with $1 the working directory, then the shim
		want   string
	}{
		{"deleted_directory", `mkdir "$1" && cd "$1" && rmdir "$1" && exec "$2"`, "redcoast-client: getwd"},
		{"existing_directory", `mkdir "$1" && cd "$1" && exec "$2"`, "redcoast-client: no session from the gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(cwd, tt.name)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", tt.script, "sh", dir, filepath.Join(bin, "claude"))
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); err == nil || code != 3 {
				t.Fatalf("exit %d (%v), want 3:\n%s", code, err, out)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, out)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("the real claude started (%v)", err)
			}
		})
	}
}
