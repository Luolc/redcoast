// redcoast-client starts claude, pi or codex through the gateway: it checks that nothing
// on this machine would override the session credential, asks the gateway for a
// session, starts the client with the gateway's settings and nothing else changed,
// forwards termination signals, and revokes the session when the client exits.
//
// Usage is "redcoast-client claude|pi|codex [the client's arguments]": every argument
// after the client's name is passed through unchanged; pi and codex also get the
// launcher's own settings (pi.go, codex.go). The launcher is configured by environment
// variables. claude talks to the gateway's Claude process, pi and codex to its Codex
// process; each process has its own session interface, named by variables with the
// prefix REDCOAST_CLIENT_ for Claude and REDCOAST_CLIENT_CODEX_ for Codex. On the
// gateway's machine, <prefix>SOCKET (the process's session socket). On another
// machine, <prefix>ADDRESS (the process's session address: host:port, or
// https://host:port when the gateway serves TLS with a certificate the system trusts)
// and <prefix>CREDENTIAL_FILE (the file holding this machine's credential, mode 0600);
// the two modes exclude each other. Also REDCOAST_CLIENT_CLAUDE, REDCOAST_CLIENT_PI and
// REDCOAST_CLIENT_CODEX (the client's executable, default: its name in PATH) and
// REDCOAST_CLIENT_MACHINE (the client machine name reported over the socket, default:
// the host name; over the network the gateway knows the machine by its credential).
// Exit status is the client's; a failure before the client starts, a wrong command
// line included, exits with 3.
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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/redcoast/session"
	"golang.org/x/sys/unix"
)

// exitLauncherFailure is the exit status for failures before the client starts; the
// client's own statuses are passed through unchanged.
const exitLauncherFailure = 3

// termGrace is how long the client gets after a forwarded SIGTERM before its process
// group is killed. Tests shorten it.
var termGrace = 15 * time.Second

// descendantGrace is how long the client's remaining descendants get after the client
// itself has exited before they are killed.
const descendantGrace = 2 * time.Second

// reapWait bounds the wait for killed descendants to disappear.
const reapWait = 2 * time.Second

// revokeTimeout bounds the revocation after the client exits.
const revokeTimeout = 5 * time.Second

// launcher is one run's configuration.
type launcher struct {
	socket     string
	address    string // the gateway's session address, in the network mode
	credential string // this machine's credential, in the network mode
	client     *client
	executable string // resolved path of the client's executable
	machine    string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	signals    <-chan os.Signal // SIGINT, SIGTERM and SIGHUP to forward
	environ    []string
	cwd        string
	home       string
	args       []string
	launched   func(int) // called with the client's PID once it runs; tests use it
}

// client is what the launcher does differently for each program it starts.
type client struct {
	name string
	// program names the client in the launch metadata.
	program string
	// sessionPrefix starts the names of the variables that locate the session interface
	// of the gateway process serving this client.
	sessionPrefix string
	// preflight returns everything that would override the session credential, and each
	// setting that conflicts with one the launcher makes.
	preflight func(environ []string, cwd, home string, args []string) (overrides, conflicts []string)
	// prepare returns the client's environment and arguments for the session, after
	// writing whatever files they refer to.
	prepare func(l *launcher, grant session.Grant) (env, args []string, err error)
}

// clients are the programs the launcher starts, by the name on its command line.
var clients = map[string]*client{
	"claude": {name: "claude", program: "claude-code", sessionPrefix: "REDCOAST_CLIENT_", preflight: preflight, prepare: prepareClaude},
	"pi":     {name: "pi", program: "pi", sessionPrefix: "REDCOAST_CLIENT_CODEX_", preflight: piPreflight, prepare: preparePi},
	"codex":  {name: "codex", program: "codex-cli", sessionPrefix: "REDCOAST_CLIENT_CODEX_", preflight: codexPreflight, prepare: prepareCodex},
}

// usage is printed when the command line does not start with a client's name.
const usage = "usage: redcoast-client claude|pi|codex [the client's arguments]"

// commandLine returns the client the command line names and the arguments after its
// name.
func commandLine(arguments []string) (*client, []string, bool) {
	if len(arguments) == 0 || clients[arguments[0]] == nil {
		return nil, nil, false
	}
	return clients[arguments[0]], arguments[1:], true
}

func main() {
	c, args, ok := commandLine(os.Args[1:])
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(exitLauncherFailure)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	// The client's process group becomes the terminal's foreground group, which leaves the
	// launcher in the background: it still writes its messages to the terminal and
	// must not be stopped for that.
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTTIN)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "redcoast-client: %v\n", err)
		os.Exit(exitLauncherFailure)
	}
	home, _ := os.UserHomeDir()
	l := &launcher{client: c, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, signals: signals, environ: os.Environ(), cwd: cwd, home: home, args: args}
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
	if overrides, conflicts := l.client.preflight(l.environ, l.cwd, l.home, l.args); len(overrides)+len(conflicts) != 0 {
		if len(overrides) != 0 {
			l.report("redcoast-client: refusing to start %s; the session credential would be overridden by:\n", l.client.name)
			for _, problem := range overrides {
				l.report("  - %s\n", problem)
			}
		}
		if len(conflicts) != 0 {
			l.report("redcoast-client: refusing to start %s; settings set a launcher toggle to another value:\n", l.client.name)
			for _, problem := range conflicts {
				l.report("  - %s\n", problem)
			}
		}
		return exitLauncherFailure
	}
	client := &session.Client{Socket: l.socket, Address: l.address, Credential: l.credential}
	grant, err := client.Issue(ctx, l.machine, l.launchMeta(ctx))
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

// configure reads the launcher's own variables and resolves the client's executable.
func (l *launcher) configure() error {
	env := environment(l.environ)
	socketVar, addressVar, credentialVar := l.client.sessionPrefix+"SOCKET", l.client.sessionPrefix+"ADDRESS", l.client.sessionPrefix+"CREDENTIAL_FILE"
	l.socket, l.address = env[socketVar], env[addressVar]
	credentialFile := env[credentialVar]
	switch {
	case l.socket == "" && l.address == "":
		return fmt.Errorf("neither %s nor %s is set", socketVar, addressVar)
	case l.socket != "" && (l.address != "" || credentialFile != ""):
		return fmt.Errorf("%s excludes %s and %s", socketVar, addressVar, credentialVar)
	case l.address != "" && credentialFile == "":
		return fmt.Errorf("%s needs %s", addressVar, credentialVar)
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
	executableVar := "REDCOAST_CLIENT_" + strings.ToUpper(l.client.name)
	executable := env[executableVar]
	if executable == "" {
		executable = l.client.name
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return fmt.Errorf("%s executable: %v", l.client.name, err)
	}
	if self, err := os.Executable(); err == nil && sameFile(self, path) {
		return fmt.Errorf("%s resolves to redcoast-client itself", executableVar)
	}
	l.executable = path
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
// transcript later and for counting sessions by client program. It contains no
// credential. A working directory too long for the gateway's limit is left out, so
// that the session is still issued.
func (l *launcher) launchMeta(ctx context.Context) json.RawMessage {
	env := environment(l.environ)
	meta := map[string]any{"launcher_pid": os.Getpid(), "cwd": l.cwd, "client": l.client.program}
	if version := clientVersion(ctx, l.executable, l.environ); version != "" {
		meta["client_version"] = version
	}
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID"} {
		if value, ok := env[key]; ok {
			meta[strings.ToLower(key)] = value
		}
	}
	// Encoding a map of strings and an int cannot fail.
	data, _ := json.Marshal(meta)
	if len(data) > session.MaxLaunchMeta {
		delete(meta, "cwd")
		data, _ = json.Marshal(meta)
	}
	return data
}

// versionTimeout bounds the client's --version run. Tests shorten it.
var versionTimeout = 3 * time.Second

// maxVersionOutput is how much of the --version output is read.
const maxVersionOutput = 4096

// versionPattern finds a version number in the --version output, which each client
// words differently ("2.1.0 (Claude Code)", "codex-cli 0.46.0").
var versionPattern = regexp.MustCompile(session.VersionNumber)

// clientVersion runs the client's executable with --version and returns the first
// version number in its standard output, or "" when it fails, prints none or takes
// longer than versionTimeout: knowing the version is not worth delaying or refusing
// the launch. The run has no session, so it gets only PATH and HOME and a proxy that
// refuses every connection, and it runs in / rather than in a project whose settings
// it might read. It gets its own
// process group, which is killed on the timeout and when a descendant still holds the
// output.
func clientVersion(ctx context.Context, executable string, environ []string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Env = versionEnvironment(environ)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 100 * time.Millisecond
	var out limitedBuffer
	cmd.Stdout = &out
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// The client exited and a descendant in its group holds the output, so the group
		// still exists and its ID is not reused.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = nil
	}
	if err != nil {
		return ""
	}
	return versionPattern.FindString(out.String())
}

// refusingProxy is an address where no proxy listens.
const refusingProxy = "http://127.0.0.1:1"

// versionEnvironment is the environment of the --version run: PATH and HOME from the
// parent, and every proxy variable set to refusingProxy.
func versionEnvironment(parent []string) []string {
	var env []string
	for _, entry := range parent {
		if key, _, _ := strings.Cut(entry, "="); key == "PATH" || key == "HOME" {
			env = append(env, entry)
		}
	}
	for _, key := range clearedProxyVariables {
		if !strings.EqualFold(key, "NO_PROXY") {
			env = append(env, key+"="+refusingProxy)
		}
	}
	return env
}

// limitedBuffer keeps the first maxVersionOutput bytes written to it and discards the
// rest.
type limitedBuffer struct {
	strings.Builder
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxVersionOutput - b.Len(); room > 0 {
		b.Builder.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// start runs the client with the session's environment until it exits and returns its
// exit status. The client runs in its own process group, which is the terminal's
// foreground group when stdin is a terminal, so that the terminal's signals reach
// the client and not the launcher. The launcher adopts orphaned descendants (on Linux),
// so the client's whole tree stays visible, and reaps them as they exit. Signals the launcher receives go to the whole
// tree; after a SIGTERM the tree is killed when termGrace passes. When the client has
// exited, whatever it left behind gets descendantGrace after a SIGTERM and is then
// killed, so nothing the client started outlives the launcher.
func (l *launcher) start(ctx context.Context, grant session.Grant) int {
	name := l.client.name
	env, args, err := l.client.prepare(l, grant)
	if err != nil {
		l.report("redcoast-client: cannot prepare %s: %v\n", name, err)
		return exitLauncherFailure
	}
	if err := adoptOrphans(); err != nil {
		l.report("redcoast-client: cannot adopt %s's orphans: %v\n", name, err)
		return exitLauncherFailure
	}
	cmd := exec.CommandContext(ctx, l.executable, args...)
	cmd.Env = env
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
		l.report("redcoast-client: cannot start %s: %v\n", name, err)
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

// processTree is the client's process group plus every descendant of the launcher that
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

// reap ends whatever is left in the tree after the client exited: SIGTERM,
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

// gatewayVariables are the variables that send claude to the gateway with the session
// credential. Any of them in a settings file's env map would override the launcher's
// value, whatever that value is.
var gatewayVariables = []string{
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"HTTPS_PROXY",
	"NO_PROXY",
}

// toggles are the switches the launcher sets for claude, each to its value. A settings
// file's env map may set one only to the same value.
var toggles = []struct{ key, value string }{
	{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
	{"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1"},
	{"DISABLE_AUTOUPDATER", "1"},
}

// clearedProxyVariables are removed from the client's environment: a proxy variable that
// outranks or duplicates HTTPS_PROXY would send traffic past the gateway's forward
// proxy. The lower-case forms of HTTPS_PROXY and NO_PROXY are set to the same values
// for programs that read only those.
var clearedProxyVariables = []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

// childEnvironment returns claude's environment: the parent's, minus the proxy
// variables, plus the gateway's variables for this session and the toggles.
func childEnvironment(parent []string, grant session.Grant) []string {
	set := []string{"ANTHROPIC_BASE_URL=" + grant.ReverseProxy, "CLAUDE_CODE_OAUTH_TOKEN=" + grant.Token}
	for _, toggle := range toggles {
		set = append(set, toggle.key+"="+toggle.value)
	}
	noProxy := ""
	if reverse, err := url.Parse(grant.ReverseProxy); err == nil {
		noProxy = reverse.Hostname()
	}
	return proxiedEnvironment(parent, grant, noProxy, set)
}

// prepareClaude starts claude with childEnvironment and the arguments unchanged.
func prepareClaude(l *launcher, grant session.Grant) ([]string, []string, error) {
	return childEnvironment(l.environ, grant), l.args, nil
}

// proxiedEnvironment returns the parent's environment minus every proxy variable and
// every variable set assigns, then the forward proxy with the session credential,
// noProxy, and set.
func proxiedEnvironment(parent []string, grant session.Grant, noProxy string, set []string) []string {
	drop := make(map[string]bool)
	for _, key := range clearedProxyVariables {
		drop[key] = true
	}
	for _, entry := range set {
		key, _, _ := strings.Cut(entry, "=")
		drop[key] = true
	}
	var env []string
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if !drop[key] {
			env = append(env, entry)
		}
	}
	// The forward proxy speaks TLS exactly when the reverse proxy does.
	scheme := "http"
	if reverse, err := url.Parse(grant.ReverseProxy); err == nil && reverse.Scheme == "https" {
		scheme = "https"
	}
	proxy := scheme + "://session:" + grant.Token + "@" + grant.ForwardProxy
	env = append(env, "HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	return append(env, set...)
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
