package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Luolc/redcoast/claude"
)

// adminCommands are the subcommands served over the management socket.
var adminCommands = map[string]bool{"reload": true, "handoff": true, "status": true, "sessions": true, "pause": true, "resume": true, "refresh": true, "machine": true, "import": true, "setup": true, "account": true}

// adminCommand is a parsed management invocation: the request to send to the socket.
type adminCommand struct {
	socket  string
	method  string
	path    string
	body    any           // nil sends no body
	input   io.Reader     // sent as the body instead, as it is; nil sends body
	timeout time.Duration // how long the command waits for the gateway's answer
	// inputLimit is the most bytes input may hold, maxImportInput when 0; trimLine drops
	// one line break from its end; inputName and contentType describe it.
	inputLimit  int
	trimLine    bool
	inputName   string
	contentType string
	// write writes a success answer to out; nil writes it indented.
	write func(answer []byte, out io.Writer) error
}

// errAdminUsage is returned when a management subcommand is misused.
var errAdminUsage = errors.New(`usage:
  redcoast claude|codex reload --admin-socket <path>
  redcoast claude|codex handoff --admin-socket <path>
  redcoast claude|codex status --admin-socket <path>
  redcoast claude|codex sessions --admin-socket <path> [--since <RFC 3339>] [--until <RFC 3339>]
      [--cwd-prefix <path>] [--machine <name>] [--state ended|all]
  redcoast claude|codex pause <alias> --admin-socket <path> [--reason <text>] [--until <RFC 3339>]
  redcoast claude|codex resume <alias> --admin-socket <path>
  redcoast claude|codex refresh <alias> --admin-socket <path>
  redcoast claude|codex refresh --all --admin-socket <path>
  redcoast codex import <alias> --admin-socket <path> < auth.json
  redcoast claude setup next [--request-id <id>] --admin-socket <path>
  redcoast claude setup report <alias> <request-id> --run-id <id> --source-kind <dashboard|slack> --state <state> --step <code> [--reason <code>] [--wait-until <RFC 3339>] --admin-socket <path>
  redcoast claude setup token <alias> <request-id> --run-id <id> --source-kind <dashboard|slack> --admin-socket <path> < token
  redcoast claude setup request <alias> --rev <n> --source-thread <CHANNEL/TS> --source-ts <TS> --admin-socket <path>
  redcoast claude account create <alias> --email <email> --device-id <hex> --plan <plan> --proxy-host <host> --proxy-port <port>
      --expected-egress-ip <ip> --proxy-username-ref <ref> --proxy-password-ref <ref>
      [--receiver-email <email>] [--proxy-provider <name>] [--proxy-type <name>]
      --source-thread <CHANNEL/TS> --source-ts <TS> --admin-socket <path>
  redcoast claude account set <alias> --rev <n> [--receiver-email <email>] [--proxy-provider <name>] [--proxy-type <name>]
      --source-thread <CHANNEL/TS> --source-ts <TS> --admin-socket <path>
  redcoast claude account show <alias> --admin-socket <path>
  redcoast claude account verify <alias> [--rev <n>] --source-thread <CHANNEL/TS> --source-ts <TS> --admin-socket <path>
  redcoast claude account activate <alias> --rev <n> --source-thread <CHANNEL/TS> --source-ts <TS> --admin-socket <path>
  redcoast claude|codex machine add <name> <sha256-of-credential> --admin-socket <path> [--max-sessions <n>]
  redcoast claude|codex machine revoke <name> --admin-socket <path>
  redcoast claude|codex machine limit <name> <n> --admin-socket <path>
  redcoast claude|codex machine list --admin-socket <path>
reload reads the inventory and 1Password again and replaces the accounts; nothing
changes when it fails; handoff starts the binary at the path the gateway was started
from (the current release) on the gateway's sockets, and the old process drains once
the new one is ready and keeps serving when it is not; pause without --until lasts
until resume; refresh sends the account one small request through its exit to read its
quota again, and ends its pause after an upstream 429 when the reading is under the
hard threshold; refresh --all does that for every account, one after another, prints
one line per account and fails when any of them failed (an account skipped for a pause
other than quota is not a failure); import reads a Codex login in auth.json's format
from stdin, replaces the account's login with it when it is the account's, and renews
it at once, so the copy it came from stops working; the setup commands are for a login machine: next prints
the next setup task the dashboard asked for or {"task": null} (with --request-id, that one task, which must have been
requested from Slack), report records one step of the task it runs, and token reads the setup token from
stdin (at most 4 KiB, one line) and hands it over, never printing it; request queues a login-and-token task for an
account from a Slack message; the account commands create, show, verify and activate an account as the dashboard does
(activate only a testing one), the writing ones only while the gateway's admin_write is on and each with the Slack
message it came from; sessions lists the sessions started in [since,
until), by default the last day, newest first and at most 500, only the ended ones unless --state all; machine add takes the SHA-256 the client machine printed, never the credential itself`)

// handoffCommandTimeout bounds a handoff command: the new process's startup, bounded
// by handoffTimeout, and the answer.
const handoffCommandTimeout = handoffTimeout + 30*time.Second

// reloadTimeout bounds a reload command: the inventory, the references and the wait
// for the binding sections in progress.
const reloadTimeout = 90 * time.Second

// refreshCommandTimeout bounds a refresh command: the gateway's own bound on the
// refresh and the answer.
const refreshCommandTimeout = claude.RefreshTimeout + 10*time.Second

// refreshAllCommandTimeout bounds a refresh --all command. The gateway refreshes one
// account after another, each bounded by claude.RefreshTimeout; this covers twenty
// accounts that all run to that bound.
const refreshAllCommandTimeout = 20*claude.RefreshTimeout + 10*time.Second

// importCommandTimeout bounds an import command: the login's renewal and the answer.
const importCommandTimeout = 45 * time.Second

// maxImportInput bounds the login an import reads; the gateway takes at most 64 KiB.
const maxImportInput = 64 << 10

// parseAdminCommand parses "<subcommand> [args]".
func parseAdminCommand(arguments []string) (adminCommand, error) {
	flags := flag.NewFlagSet("redcoast claude "+arguments[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("admin-socket", "", "Unix socket of the management interface")
	reason := flags.String("reason", "manual", "Reason saved with a pause")
	until := flags.String("until", "", "End of a pause, RFC 3339; omitted pauses until resume")
	maxSessions := flags.Int("max-sessions", 0, "Session limit of a machine being added; 0 takes the default")
	all := flags.Bool("all", false, "Refresh every account")
	setup := registerSetupFlags(flags, arguments[0])
	agent := registerAgentFlags(flags, arguments[0])
	listing := sessionFilters(flags, arguments[0])
	flagArguments, rest := splitFlags(flags, arguments[1:])
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *socket == "" || *all && arguments[0] != "refresh" {
		return adminCommand{}, errAdminUsage
	}
	agent.note(flags)
	if arguments[0] == "sessions" {
		return sessionsCommand(*socket, *until, listing, rest)
	}
	if arguments[0] == "setup" || arguments[0] == "account" {
		return parseSetupOrAccount(arguments[0], flags, setup, agent, *socket, rest, *reason, *until != "" || *maxSessions != 0)
	}
	if arguments[0] != "pause" && *until != "" || arguments[0] != "machine" && *maxSessions != 0 {
		return adminCommand{}, errAdminUsage
	}
	if *all {
		if len(rest) != 0 {
			return adminCommand{}, errAdminUsage
		}
		return adminCommand{socket: *socket, method: http.MethodPost, path: "/refresh", timeout: refreshAllCommandTimeout, write: writeRefreshAll}, nil
	}
	cmd := adminCommand{socket: *socket, method: http.MethodPost, timeout: 10 * time.Second}
	switch arguments[0] {
	case "machine":
		return machineCommand(cmd, rest, *maxSessions)
	case "reload", "handoff", "status":
		if len(rest) != 0 {
			return adminCommand{}, errAdminUsage
		}
		cmd.path = "/" + arguments[0]
		switch arguments[0] {
		case "status":
			cmd.method = http.MethodGet
		case "reload":
			cmd.timeout = reloadTimeout
		case "handoff":
			cmd.timeout = handoffCommandTimeout
		}
	case "pause", "resume", "refresh", "import":
		return accountCommand(cmd, arguments[0], rest, *reason, *until)
	default:
		return adminCommand{}, errAdminUsage
	}
	return cmd, nil
}

// parseSetupOrAccount builds a setup or an account command. otherFlags says that a flag of
// another subcommand (--until, --max-sessions) was given; --reason belongs to setup only.
func parseSetupOrAccount(subcommand string, flags *flag.FlagSet, setup *setupFlags, agent *agentFlags, socket string, rest []string, reason string, otherFlags bool) (adminCommand, error) {
	if subcommand == "setup" {
		return parseSetup(flags, setup, agent, socket, rest, reason, otherFlags)
	}
	flags.Visit(func(f *flag.Flag) { otherFlags = otherFlags || f.Name == "reason" })
	if otherFlags {
		return adminCommand{}, errAdminUsage
	}
	return accountAgentCommand(agent, socket, rest)
}

// sessionFilters registers the filters of "sessions" on flags and returns the query
// they fill in; for any other subcommand it registers nothing and returns nil.
func sessionFilters(flags *flag.FlagSet, subcommand string) url.Values {
	if subcommand != "sessions" {
		return nil
	}
	listing := url.Values{}
	for _, name := range []string{"since", "cwd-prefix", "machine", "state"} {
		flags.Func(name, "Session list filter", func(value string) error {
			listing.Set(strings.ReplaceAll(name, "-", "_"), value)
			return nil
		})
	}
	return listing
}

// sessionsCommand builds GET /sessions from the filters; the gateway checks them.
func sessionsCommand(socket, until string, listing url.Values, rest []string) (adminCommand, error) {
	if len(rest) != 0 {
		return adminCommand{}, errAdminUsage
	}
	if until != "" {
		listing.Set("until", until)
	}
	cmd := adminCommand{socket: socket, method: http.MethodGet, path: "/sessions", timeout: 10 * time.Second}
	if len(listing) > 0 {
		cmd.path += "?" + listing.Encode()
	}
	return cmd, nil
}

// machineCommand parses the arguments after "machine".
func machineCommand(cmd adminCommand, rest []string, maxSessions int) (adminCommand, error) {
	if len(rest) == 0 {
		return adminCommand{}, errAdminUsage
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return adminCommand{}, errAdminUsage
		}
		cmd.method, cmd.path = http.MethodGet, "/machines"
	case "add":
		if len(rest) != 3 {
			return adminCommand{}, errAdminUsage
		}
		cmd.path = "/machines"
		cmd.body = map[string]any{"name": rest[1], "credential_hash": rest[2], "max_sessions": maxSessions}
	case "revoke":
		if len(rest) != 2 {
			return adminCommand{}, errAdminUsage
		}
		cmd.method, cmd.path = http.MethodDelete, "/machines/"+rest[1]
	case "limit":
		if len(rest) != 3 {
			return adminCommand{}, errAdminUsage
		}
		limit, err := strconv.Atoi(rest[2])
		if err != nil || limit < 1 {
			return adminCommand{}, fmt.Errorf("%q is not a session limit of at least 1", rest[2])
		}
		cmd.path = "/machines/" + rest[1] + "/limit"
		cmd.body = map[string]any{"max_sessions": limit}
	default:
		return adminCommand{}, errAdminUsage
	}
	return cmd, nil
}

// setupFlags are the flags of the setup commands.
type setupFlags struct {
	runID, state, step, reason string
	reasonGiven                bool
}

// registerSetupFlags registers the flags of the setup commands on flags when the
// subcommand is setup, whose --state would otherwise clash with the session filter of
// the same name; for any other subcommand it registers nothing.
func registerSetupFlags(flags *flag.FlagSet, subcommand string) *setupFlags {
	f := &setupFlags{}
	if subcommand == "setup" {
		flags.StringVar(&f.runID, "run-id", "", "Run ID of a setup task")
		flags.StringVar(&f.state, "state", "", "State of a setup report: running, needs_human or failed")
		flags.StringVar(&f.step, "step", "", "Step of a setup report")
	}
	return f
}

// parseSetup builds a setup command from the parsed flags. otherFlags says that a flag
// of another subcommand (--until, --max-sessions) was given.
func parseSetup(flags *flag.FlagSet, setup *setupFlags, agent *agentFlags, socket string, rest []string, reason string, otherFlags bool) (adminCommand, error) {
	if otherFlags {
		return adminCommand{}, errAdminUsage
	}
	flags.Visit(func(f *flag.Flag) { setup.reasonGiven = setup.reasonGiven || f.Name == "reason" })
	setup.reason = reason
	return setupCommand(socket, rest, *setup, agent)
}

// setupCommandTimeout bounds a setup command: the handover's own work, bounded by the
// gateway, and the answer.
const setupCommandTimeout = 5*time.Minute + 30*time.Second

// maxSetupInput bounds the token a handover reads; the gateway takes at most 4 KiB.
const maxSetupInput = 4 << 10

// setupCommand builds `setup next`, `setup report <alias> <request-id>` or `setup token
// <alias> <request-id>`. The IDs go into the request as the gateway's shapes only, so a
// misuse is refused here without a request.
func setupCommand(socket string, rest []string, f setupFlags, a *agentFlags) (adminCommand, error) {
	cmd := adminCommand{socket: socket, method: http.MethodPost, timeout: 30 * time.Second}
	if len(rest) == 0 {
		return adminCommand{}, errAdminUsage
	}
	plain := f.runID != "" || f.state != "" || f.step != "" || f.reasonGiven
	switch rest[0] {
	case "next":
		if len(rest) != 1 || plain || !a.only("request-id") {
			return adminCommand{}, errAdminUsage
		}
		cmd.path = "/setup/next"
		if a.given["request-id"] {
			cmd.path += "?" + url.Values{"request_id": {a.requestID}}.Encode()
		}
		return cmd, nil
	case "request":
		if len(rest) != 2 || !accountAlias.MatchString(rest[1]) || plain || !a.only("rev", "source-thread", "source-ts") || !a.has("rev", "source-thread", "source-ts") {
			return adminCommand{}, errAdminUsage
		}
		cmd.path, cmd.body = "/accounts/"+rest[1]+"/setup/request", a.revBody()
		return cmd, nil
	case "report", "token":
		if len(rest) != 3 || !accountAlias.MatchString(rest[1]) || f.runID == "" || !a.has("source-kind") {
			return adminCommand{}, errAdminUsage
		}
	default:
		return adminCommand{}, errAdminUsage
	}
	if rest[0] == "token" {
		return setupTokenCommand(cmd, rest[1], rest[2], f, a)
	}
	return setupReportCommand(cmd, rest[1], rest[2], f, a)
}

// setupTokenCommand is `setup token <alias> <request-id>`, which reads the token from stdin.
func setupTokenCommand(cmd adminCommand, alias, requestID string, f setupFlags, a *agentFlags) (adminCommand, error) {
	if f.state != "" || f.step != "" || f.reasonGiven || !a.only("source-kind") {
		return adminCommand{}, errAdminUsage
	}
	query := url.Values{"request_id": {requestID}, "run_id": {f.runID}, "source_kind": {a.sourceKind}}
	cmd.path = "/accounts/" + alias + "/setup/token?" + query.Encode()
	cmd.input, cmd.inputLimit, cmd.trimLine, cmd.timeout = os.Stdin, maxSetupInput, true, setupCommandTimeout
	cmd.inputName, cmd.contentType = "token", "text/plain"
	return cmd, nil
}

// setupReportCommand is `setup report <alias> <request-id>`.
func setupReportCommand(cmd adminCommand, alias, requestID string, f setupFlags, a *agentFlags) (adminCommand, error) {
	if f.state == "" || f.step == "" || !a.only("source-kind", "wait-until") {
		return adminCommand{}, errAdminUsage
	}
	body := map[string]any{"request_id": requestID, "run_id": f.runID, "state": f.state, "step": f.step, "reason": nil, "source_kind": a.sourceKind}
	if f.reasonGiven {
		body["reason"] = f.reason
	}
	if a.given["wait-until"] {
		body["wait_until"] = a.waitUntil
	}
	cmd.path, cmd.body = "/accounts/"+alias+"/setup/report", body
	return cmd, nil
}

// accountCommand builds the pause, resume, refresh or import of the one alias in rest.
func accountCommand(cmd adminCommand, verb string, rest []string, reason, until string) (adminCommand, error) {
	if len(rest) != 1 || !accountAlias.MatchString(rest[0]) {
		return adminCommand{}, errAdminUsage
	}
	switch verb {
	case "refresh":
		return refreshCommand(cmd.socket, rest[0]), nil
	case "import":
		cmd.input, cmd.timeout = os.Stdin, importCommandTimeout
	case "pause":
		body, err := pauseBody(reason, until)
		if err != nil {
			return adminCommand{}, err
		}
		cmd.body = body
	}
	cmd.path = "/accounts/" + rest[0] + "/" + verb
	return cmd, nil
}

// refreshCommand is the refresh of account alias over socket.
func refreshCommand(socket, alias string) adminCommand {
	return adminCommand{socket: socket, method: http.MethodPost, path: "/accounts/" + alias + "/refresh", timeout: refreshCommandTimeout}
}

// writeRefreshAll writes the answer to POST /refresh, one line per account, and fails
// when the refresh of any account failed; a skipped account is not a failure.
func writeRefreshAll(answer []byte, out io.Writer) error {
	var results []json.RawMessage
	if err := json.Unmarshal(answer, &results); err != nil {
		return fmt.Errorf("management socket: %v", err)
	}
	var failed []string
	for _, result := range results {
		if _, err := fmt.Fprintf(out, "%s\n", result); err != nil {
			return err
		}
		var outcome struct {
			Alias string `json:"alias"`
			Error string `json:"error"`
		}
		if json.Unmarshal(result, &outcome) == nil && outcome.Error != "" {
			failed = append(failed, outcome.Alias)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("refresh failed for %s", strings.Join(failed, " "))
	}
	return nil
}

// pauseBody builds the body of a pause: the reason and, when given, the end.
func pauseBody(reason, until string) (map[string]any, error) {
	body := map[string]any{"reason": reason}
	if until != "" {
		at, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return nil, fmt.Errorf("%q is not an RFC 3339 time", until)
		}
		body["until"] = at.UTC()
	}
	return body, nil
}

// refusal is the gateway's failure answer to a management request.
type refusal struct {
	path    string
	status  int
	message string // the gateway's error; empty when it gave none
	// field and check are what the gateway said the refusal concerns, when it did.
	field, check string
	// written is whether a token handover reached the vault, when the gateway said.
	written *bool
}

func (r *refusal) Error() string {
	if r.message == "" {
		return fmt.Sprintf("%s refused with status %d", r.path, r.status)
	}
	text := fmt.Sprintf("%s refused: %s", r.path, r.message)
	if r.field != "" {
		text += " (field: " + r.field + ")"
	}
	if r.check != "" {
		text += " (check: " + r.check + ")"
	}
	if r.written != nil {
		text += fmt.Sprintf(" (written: %t)", *r.written)
	}
	return text
}

// runAdmin sends the command to the gateway's management socket and writes the answer
// to out. A failure answer is returned as an error with the gateway's message.
func runAdmin(ctx context.Context, cmd adminCommand, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, cmd.timeout)
	defer cancel()
	var body io.Reader
	switch {
	case cmd.input != nil:
		limit, name := cmd.inputLimit, cmd.inputName
		if limit == 0 {
			limit, name = maxImportInput, "login"
		}
		// Two bytes more than the limit leave room for the line break a trim drops.
		login, err := io.ReadAll(io.LimitReader(cmd.input, int64(limit)+2))
		if cmd.trimLine {
			login = bytes.TrimSuffix(bytes.TrimSuffix(login, []byte("\n")), []byte("\r"))
		}
		if err != nil || len(login) > limit {
			return fmt.Errorf("stdin: cannot read a %s of at most %d KiB", name, limit>>10)
		}
		body = bytes.NewReader(login)
	case cmd.body != nil:
		encoded, err := json.Marshal(cmd.body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, cmd.method, "http://gateway"+cmd.path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", cmp.Or(cmd.contentType, "application/json"))
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cmd.socket)
	}}}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("management socket: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("management socket: %v", err)
	}
	if res.StatusCode/100 != 2 {
		var failure struct {
			Error   string `json:"error"`
			Field   string `json:"field"`
			Check   string `json:"check"`
			Written *bool  `json:"written"`
		}
		refused := &refusal{path: strings.TrimPrefix(cmd.path, "/"), status: res.StatusCode}
		if json.Unmarshal(answer, &failure) == nil {
			refused.message, refused.field, refused.check, refused.written = failure.Error, failure.Field, failure.Check, failure.Written
		}
		return refused
	}
	if cmd.write != nil {
		return cmd.write(answer, out)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, bytes.TrimSpace(answer), "", "  ") != nil {
		pretty.Write(answer)
	}
	pretty.WriteByte('\n')
	_, err = out.Write(pretty.Bytes())
	return err
}
