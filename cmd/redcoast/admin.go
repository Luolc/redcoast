package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// adminCommands are the subcommands served over the management socket.
var adminCommands = map[string]bool{"reload": true, "handoff": true, "status": true, "sessions": true, "pause": true, "resume": true, "machine": true}

// adminCommand is a parsed management invocation: the request to send to the socket.
type adminCommand struct {
	socket  string
	method  string
	path    string
	body    any           // nil sends no body
	timeout time.Duration // how long the command waits for the gateway's answer
}

// errAdminUsage is returned when a management subcommand is misused.
var errAdminUsage = errors.New(`usage:
  redcoast claude reload --admin-socket <path>
  redcoast claude handoff --admin-socket <path>
  redcoast claude status --admin-socket <path>
  redcoast claude sessions --admin-socket <path> [--since <RFC 3339>] [--until <RFC 3339>]
      [--cwd-prefix <path>] [--machine <name>] [--state ended|all]
  redcoast claude pause <alias> --admin-socket <path> [--reason <text>] [--until <RFC 3339>]
  redcoast claude resume <alias> --admin-socket <path>
  redcoast claude machine add <name> <sha256-of-credential> --admin-socket <path> [--max-sessions <n>]
  redcoast claude machine revoke <name> --admin-socket <path>
  redcoast claude machine limit <name> <n> --admin-socket <path>
  redcoast claude machine list --admin-socket <path>
reload reads the inventory and 1Password again and replaces the accounts; nothing
changes when it fails; handoff starts the binary at the path the gateway was started
from (the current release) on the gateway's sockets, and the old process drains once
the new one is ready and keeps serving when it is not; pause without --until lasts
until resume; sessions lists the sessions started in [since, until), by default the last
day, newest first and at most 500, only the ended ones unless --state all; machine add takes the SHA-256 the client machine printed, never the credential itself`)

// handoffCommandTimeout bounds a handoff command: the new process's startup, bounded
// by handoffTimeout, and the answer.
const handoffCommandTimeout = handoffTimeout + 30*time.Second

// reloadTimeout bounds a reload command: the inventory, the references and the wait
// for the binding sections in progress.
const reloadTimeout = 90 * time.Second

// parseAdminCommand parses "<subcommand> [args]".
func parseAdminCommand(arguments []string) (adminCommand, error) {
	flags := flag.NewFlagSet("redcoast claude "+arguments[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("admin-socket", "", "Unix socket of the management interface")
	reason := flags.String("reason", "manual", "Reason saved with a pause")
	until := flags.String("until", "", "End of a pause, RFC 3339; omitted pauses until resume")
	maxSessions := flags.Int("max-sessions", 0, "Session limit of a machine being added; 0 takes the default")
	listing := sessionFilters(flags, arguments[0])
	flagArguments, rest := splitFlags(arguments[1:])
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *socket == "" {
		return adminCommand{}, errAdminUsage
	}
	if arguments[0] == "sessions" {
		return sessionsCommand(*socket, *until, listing, rest)
	}
	if arguments[0] != "pause" && *until != "" || arguments[0] != "machine" && *maxSessions != 0 {
		return adminCommand{}, errAdminUsage
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
	case "pause", "resume":
		if len(rest) != 1 || !accountAlias.MatchString(rest[0]) {
			return adminCommand{}, errAdminUsage
		}
		cmd.path = "/accounts/" + rest[0] + "/" + arguments[0]
		if arguments[0] == "pause" {
			body, err := pauseBody(*reason, *until)
			if err != nil {
				return adminCommand{}, err
			}
			cmd.body = body
		}
	default:
		return adminCommand{}, errAdminUsage
	}
	return cmd, nil
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

// runAdmin sends the command to the gateway's management socket and writes the answer
// to out. A failure answer is returned as an error with the gateway's message.
func runAdmin(ctx context.Context, cmd adminCommand, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, cmd.timeout)
	defer cancel()
	var body io.Reader
	if cmd.body != nil {
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
		req.Header.Set("Content-Type", "application/json")
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
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &failure) == nil && failure.Error != "" {
			return fmt.Errorf("%s refused: %s", strings.TrimPrefix(cmd.path, "/"), failure.Error)
		}
		return fmt.Errorf("%s refused with status %d", strings.TrimPrefix(cmd.path, "/"), res.StatusCode)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, bytes.TrimSpace(answer), "", "  ") != nil {
		pretty.Write(answer)
	}
	pretty.WriteByte('\n')
	_, err = out.Write(pretty.Bytes())
	return err
}
