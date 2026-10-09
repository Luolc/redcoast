// redcoast-client starts claude through the gateway: it checks that nothing on this
// machine would override the session credential, asks the gateway for a session, starts
// claude with the gateway's environment variables and nothing else changed, forwards
// termination signals, and revokes the session when claude exits.
//
// Usage is "redcoast-client claude [claude's arguments]": every argument after "claude"
// is passed through unchanged. The launcher is configured by environment variables. On
// the gateway's machine, REDCOAST_CLIENT_SOCKET (the gateway's session socket). On
// another machine, REDCOAST_CLIENT_ADDRESS (the gateway's session address: host:port,
// or https://host:port when the gateway serves TLS with a certificate the system
// trusts) and REDCOAST_CLIENT_CREDENTIAL_FILE (the file holding this machine's
// credential, mode 0600); the two modes exclude each other. Also
// REDCOAST_CLIENT_CLAUDE (the claude executable, default: claude in PATH) and
// REDCOAST_CLIENT_MACHINE (the client machine name reported over the socket, default:
// the host name; over the network the gateway knows the machine by its credential).
// Exit status is claude's; a failure before claude starts, a wrong command line
// included, exits with 3.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/redcoast/session"
	"golang.org/x/sys/unix"
)

// exitLauncherFailure is the exit status for failures before claude starts; claude's
// own statuses are passed through unchanged.
const exitLauncherFailure = 3

// termGrace is how long claude gets after a forwarded SIGTERM before its process group
// is killed. Tests shorten it.
var termGrace = 15 * time.Second

// descendantGrace is how long claude's remaining descendants get after claude itself
// has exited before they are killed.
const descendantGrace = 2 * time.Second

// reapWait bounds the wait for killed descendants to disappear.
const reapWait = 2 * time.Second

// revokeTimeout bounds the revocation after claude exits.
const revokeTimeout = 5 * time.Second

// launcher is one run's configuration.
type launcher struct {
	socket     string
	address    string // the gateway's session address, in the network mode
	credential string // this machine's credential, in the network mode
	claude     string // resolved path of the claude executable
	machine    string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	signals    <-chan os.Signal // SIGINT, SIGTERM and SIGHUP to forward
	environ    []string
	cwd        string
	home       string
	args       []string
	launched   func(int) // called with claude's PID once it runs; tests use it
}

// usage is printed when the command line does not start with "claude".
const usage = "usage: redcoast-client claude [claude's arguments]"

// claudeArgs returns the arguments to pass to claude, the ones after "claude".
func claudeArgs(arguments []string) ([]string, bool) {
	if len(arguments) == 0 || arguments[0] != "claude" {
		return nil, false
	}
	return arguments[1:], true
}

func main() {
	args, ok := claudeArgs(os.Args[1:])
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(exitLauncherFailure)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	// claude's process group becomes the terminal's foreground group, which leaves the
	// launcher in the background: it still writes its messages to the terminal and
	// must not be stopped for that.
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTTIN)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "redcoast-client: %v\n", err)
		os.Exit(exitLauncherFailure)
	}
	home, _ := os.UserHomeDir()
	l := &launcher{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, signals: signals, environ: os.Environ(), cwd: cwd, home: home, args: args}
	os.Exit(l.run(context.Background()))
}

// report writes a message to the launcher's stderr. A failed write has nowhere else
// to go.
func (l *launcher) report(format string, args ...any) {
	_, _ = fmt.Fprintf(l.stderr, format, args...)
}

// run performs the whole launch and returns the exit status.
func (l *launcher) run(ctx context.Context) int {
	if err := l.configure(); err != nil {
		l.report("redcoast-client: %v\n", err)
		return exitLauncherFailure
	}
	if problems := preflight(l.environ, l.cwd, l.home, l.args); len(problems) != 0 {
		l.report("redcoast-client: refusing to start claude; the session credential would be overridden by:\n")
		for _, problem := range problems {
			l.report("  - %s\n", problem)
		}
		return exitLauncherFailure
	}
	client := &session.Client{Socket: l.socket, Address: l.address, Credential: l.credential}
	grant, err := client.Issue(ctx, l.machine, l.launchMeta())
	var refused *session.Refusal
	switch {
	case errors.As(err, &refused):
		// The gateway's own message, which names it, is the whole line.
		l.report("%s\n", refused.Message)
		return exitLauncherFailure
	case err != nil:
		l.report("redcoast-client: no session from the gateway: %v\n", err)
		return exitLauncherFailure
	}
	status := l.start(ctx, grant)
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	if err := client.Revoke(revokeCtx, grant.Token); err != nil {
		// The gateway ends the session itself after the idle expiry.
		l.report("redcoast-client: session %s not revoked: %v\n", grant.SessionID, err)
	}
	return status
}

// configure reads the launcher's own variables and resolves the claude executable.
func (l *launcher) configure() error {
	env := environment(l.environ)
	l.socket, l.address = env["REDCOAST_CLIENT_SOCKET"], env["REDCOAST_CLIENT_ADDRESS"]
	credentialFile := env["REDCOAST_CLIENT_CREDENTIAL_FILE"]
	switch {
	case l.socket == "" && l.address == "":
		return errors.New("neither REDCOAST_CLIENT_SOCKET nor REDCOAST_CLIENT_ADDRESS is set")
	case l.socket != "" && (l.address != "" || credentialFile != ""):
		return errors.New("REDCOAST_CLIENT_SOCKET excludes REDCOAST_CLIENT_ADDRESS and REDCOAST_CLIENT_CREDENTIAL_FILE")
	case l.address != "" && credentialFile == "":
		return errors.New("REDCOAST_CLIENT_ADDRESS needs REDCOAST_CLIENT_CREDENTIAL_FILE")
	case l.address != "":
		credential, err := readCredential(credentialFile)
		if err != nil {
			return err
		}
		l.credential = credential
	}
	l.machine = env["REDCOAST_CLIENT_MACHINE"]
	if l.machine == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("REDCOAST_CLIENT_MACHINE is not set and the host name is unknown: %v", err)
		}
		l.machine = host
	}
	claude := env["REDCOAST_CLIENT_CLAUDE"]
	if claude == "" {
		claude = "claude"
	}
	path, err := exec.LookPath(claude)
	if err != nil {
		return fmt.Errorf("claude executable: %v", err)
	}
	if self, err := os.Executable(); err == nil && sameFile(self, path) {
		return errors.New("REDCOAST_CLIENT_CLAUDE resolves to redcoast-client itself")
	}
	l.claude = path
	return nil
}

// readCredential reads this machine's credential: one line in a file only its owner can
// read. The value goes into the session request and nowhere else; errors name the file,
// never its content.
func readCredential(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("machine credential file: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("machine credential file %s is readable by others; make it mode 0600", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("machine credential file: %v", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" || len(credential) > 256 || strings.ContainsAny(credential, " \t\r\n") {
		return "", fmt.Errorf("machine credential file %s must hold one non-empty line of at most 256 characters", path)
	}
	return credential, nil
}

// sameFile reports whether two paths name the same file.
func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// launchMeta is what the gateway stores about this launch, for matching traffic to a
// transcript later. It contains no credential.
func (l *launcher) launchMeta() json.RawMessage {
	env := environment(l.environ)
	meta := map[string]any{"launcher_pid": os.Getpid(), "cwd": l.cwd}
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID"} {
		if value, ok := env[key]; ok {
			meta[strings.ToLower(key)] = value
		}
	}
	// Encoding a map of strings and an int cannot fail.
	data, _ := json.Marshal(meta)
	return data
}

// start runs claude with the session's environment until it exits and returns its
// exit status. claude runs in its own process group, which is the terminal's
// foreground group when stdin is a terminal, so that the terminal's signals reach
// claude and not the launcher. The launcher adopts orphaned descendants (on Linux),
// so claude's whole tree stays visible, and reaps them as they exit. Signals the launcher receives go to the whole
// tree; after a SIGTERM the tree is killed when termGrace passes. When claude has
// exited, whatever it left behind gets descendantGrace after a SIGTERM and is then
// killed, so nothing claude started outlives the launcher.
func (l *launcher) start(ctx context.Context, grant session.Grant) int {
	if err := adoptOrphans(); err != nil {
		l.report("redcoast-client: cannot adopt claude's orphans: %v\n", err)
		return exitLauncherFailure
	}
	cmd := exec.CommandContext(ctx, l.claude, l.args...)
	cmd.Env = childEnvironment(l.environ, grant)
	cmd.Dir = l.cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = l.stdin, l.stdout, l.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if tty, ok := l.stdin.(*os.File); ok && isTerminal(tty) {
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = int(tty.Fd())
	}
	// An adopted orphan that dies must be reaped at once: as a zombie it keeps its
	// process group alive for whoever killed it and waits for the group to go.
	children := make(chan os.Signal, 1)
	signal.Notify(children, syscall.SIGCHLD)
	defer signal.Stop(children)
	if err := cmd.Start(); err != nil {
		l.report("redcoast-client: cannot start claude: %v\n", err)
		return exitLauncherFailure
	}
	tree := processTree{group: cmd.Process.Pid} // Setpgid without Pgid makes the child's PID its group ID
	if l.launched != nil {
		l.launched(cmd.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var killAt <-chan time.Time
	for {
		select {
		case err := <-done:
			tree.reap()
			return exitStatus(err)
		case sig := <-l.signals:
			tree.signal(sig.(syscall.Signal))
			if sig == syscall.SIGTERM && killAt == nil {
				killAt = time.After(termGrace)
			}
		case <-killAt:
			tree.signal(syscall.SIGKILL)
			killAt = nil
		case <-children:
			reapOrphans(cmd.Process.Pid)
		}
	}
}

// processTree is claude's process group plus every descendant of the launcher that
// left the group (claude's Bash tool detaches its commands into new sessions).
type processTree struct {
	group int
}

// signal delivers sig to the group and to every descendant outside it. Delivery to
// a process that is already gone fails, which needs no handling.
func (t processTree) signal(sig syscall.Signal) {
	_ = syscall.Kill(-t.group, sig)
	for _, p := range descendants() {
		if p.group != t.group {
			_ = syscall.Kill(p.pid, sig)
		}
	}
}

// alive reports whether anything in the tree still exists, reaping exited children
// first so that zombies do not count.
func (t processTree) alive() bool {
	reapChildren()
	return syscall.Kill(-t.group, 0) == nil || len(descendants()) != 0
}

// reap ends whatever is left in the tree after claude exited: SIGTERM,
// descendantGrace, then SIGKILL, and waits briefly for the killed to disappear.
func (t processTree) reap() {
	if !t.alive() {
		return
	}
	t.signal(syscall.SIGTERM)
	if t.settled(descendantGrace) {
		return
	}
	t.signal(syscall.SIGKILL)
	t.settled(reapWait)
}

// settled polls until the tree is empty or d passes, and reports which.
func (t processTree) settled(d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !t.alive() {
			return true
		}
	}
	return !t.alive()
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlReadTermios)
	return err == nil
}

// exitStatus maps Wait's result to a shell exit status: the child's code, or 128 plus
// the signal number when it was killed by a signal.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return exitLauncherFailure
}

// gatewayVariables are the variables the launcher sets for claude. Any of them in a
// settings file's env map would override the launcher's value.
var gatewayVariables = []string{
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"HTTPS_PROXY",
	"NO_PROXY",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL",
	"DISABLE_AUTOUPDATER",
}

// clearedProxyVariables are removed from claude's environment: a proxy variable that
// outranks or duplicates HTTPS_PROXY would send traffic past the gateway's forward
// proxy. The lower-case forms of HTTPS_PROXY and NO_PROXY are set to the same values
// for programs that read only those.
var clearedProxyVariables = []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

// childEnvironment returns claude's environment: the parent's, minus the proxy
// variables, plus the gateway's variables for this session.
func childEnvironment(parent []string, grant session.Grant) []string {
	drop := make(map[string]bool)
	for _, key := range clearedProxyVariables {
		drop[key] = true
	}
	for _, key := range gatewayVariables {
		drop[key] = true
	}
	var env []string
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if !drop[key] {
			env = append(env, entry)
		}
	}
	reverse, err := url.Parse(grant.ReverseProxy)
	noProxy := ""
	if err == nil {
		noProxy = reverse.Hostname()
	}
	// The forward proxy speaks TLS exactly when the reverse proxy does.
	scheme := "http"
	if err == nil && reverse.Scheme == "https" {
		scheme = "https"
	}
	proxy := scheme + "://session:" + grant.Token + "@" + grant.ForwardProxy
	return append(env,
		"ANTHROPIC_BASE_URL="+grant.ReverseProxy,
		"CLAUDE_CODE_OAUTH_TOKEN="+grant.Token,
		"HTTPS_PROXY="+proxy,
		"https_proxy="+proxy,
		"NO_PROXY="+noProxy,
		"no_proxy="+noProxy,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1",
		"DISABLE_AUTOUPDATER=1",
	)
}

// environment turns KEY=value entries into a map.
func environment(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	return env
}

// configDir is claude's configuration directory: CLAUDE_CONFIG_DIR, or ~/.claude.
func configDir(env map[string]string, home string) string {
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}
