package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
	"github.com/cloudflare/tableflip"
)

// TestMain runs handoffHelper instead of the tests when TestHandoff starts this binary
// as a gateway process; a handoff starts it again the same way.
func TestMain(m *testing.M) {
	if dir := os.Getenv("HANDOFF_HELPER_DIR"); dir != "" {
		handoffHelper(dir)
		return
	}
	os.Exit(m.Run())
}

// handoffHelper is the gateway process reduced to its handoff: main's listeners, the
// reverse entrypoint answering with the process ID and the management socket. With a
// file named fail in dir it exits before it is ready, as a broken release would; with
// one named hang it never gets ready.
func handoffHelper(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "fail")); err == nil {
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(dir, "hang")); err == nil {
		time.Sleep(time.Hour)
	}
	upg, err := tableflip.New(tableflip.Options{UpgradeTimeout: 2 * time.Second})
	if err != nil {
		panic(err)
	}
	cfg := config{listen: "127.0.0.1:0", forwardListen: "127.0.0.2:0", healthListen: "127.0.0.3:0", dashboardListen: "127.0.0.4:0",
		socket: filepath.Join(dir, "session.sock"), adminSocket: filepath.Join(dir, "admin.sock")}
	l, err := listenAndNotify(upg, cfg)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "address"), []byte(l.reverse.Addr().String()), 0o600); err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	serving, stopServing := serveUntilHandoff(ctx, upg.Exit(), 10*time.Second)
	admin := claude.NewAdmin(nil, nil, nil)
	admin.Handoff = upg.Upgrade
	pid := strconv.Itoa(os.Getpid())
	_ = claude.RunServers(serving,
		func(ctx context.Context) error {
			return claude.Serve(ctx, l.reverse, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, pid) }))
		},
		func(ctx context.Context) error { return session.ServeUnixListener(ctx, l.admin, admin.Handler()) })
	stopServing()
	upg.Stop()
	<-upg.Exit()
}

// TestHandoff runs gateway processes of this binary: a handoff to a release that exits
// or never gets ready fails and the old process keeps serving; one that starts moves the
// listeners to the new process and the old one exits. A further handoff from the new
// process shows that it holds the management socket and that the old process left the
// socket file in place when it exited.
func TestHandoff(t *testing.T) {
	dir := t.TempDir()
	first := exec.Command(os.Args[0], "-test.run=^$")
	first.Env = append(os.Environ(), "HANDOFF_HELPER_DIR="+dir)
	first.Stderr = os.Stderr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = first.Wait()
		close(exited)
	}()
	var pids []int
	t.Cleanup(func() {
		for _, pid := range append(pids, first.Process.Pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	var address []byte
	for deadline := time.Now().Add(10 * time.Second); len(address) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first process did not start")
		}
		address, _ = os.ReadFile(filepath.Join(dir, "address"))
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	serving := func() int {
		t.Helper()
		response, err := client.Get("http://" + string(address))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		pid, err := strconv.Atoi(string(body))
		if err != nil {
			t.Fatalf("answer %q", body)
		}
		pids = append(pids, pid)
		return pid
	}
	handoff := func() error {
		cmd, err := parseAdminCommand([]string{"handoff", "--admin-socket", filepath.Join(dir, "admin.sock")})
		if err != nil {
			t.Fatal(err)
		}
		return runAdmin(t.Context(), cmd, io.Discard)
	}
	if pid := serving(); pid != first.Process.Pid {
		t.Fatalf("served by %d, started %d", pid, first.Process.Pid)
	}
	for _, broken := range []string{"fail", "hang"} {
		if err := os.WriteFile(filepath.Join(dir, broken), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := handoff(); err == nil {
			t.Fatalf("%s: the handoff succeeded", broken)
		}
		if pid := serving(); pid != first.Process.Pid {
			t.Fatalf("%s: after the failed handoff served by %d, not the old process %d", broken, pid, first.Process.Pid)
		}
		if err := os.Remove(filepath.Join(dir, broken)); err != nil {
			t.Fatal(err)
		}
	}
	if err := handoff(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the old process did not exit after the handoff")
	}
	second := serving()
	if second == first.Process.Pid {
		t.Fatal("still served by the old process")
	}
	if err := handoff(); err != nil {
		t.Fatalf("handoff from the new process: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); serving() == second; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the second handoff did not move the listeners")
		}
	}
}
