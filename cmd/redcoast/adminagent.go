package main

import (
	"flag"
	"net/http"
	"slices"
	"time"
)

// agentFlags are the flags of the commands an agent and a login machine use: the Slack
// message a write came from, the account's settings, a request ID, the source kind of a
// task and a wait. given names the flags that were set.
type agentFlags struct {
	sourceKind, waitUntil, requestID, thread, ts string
	email, plan, host, ip, userRef, passRef      string
	deviceID, receiverEmail                      string
	provider, kind                               string
	port                                         int
	rev                                          int64
	given                                        map[string]bool
}

// registerAgentFlags registers the flags of the setup and account subcommands on flags;
// for any other subcommand it registers nothing.
func registerAgentFlags(flags *flag.FlagSet, subcommand string) *agentFlags {
	f := &agentFlags{given: map[string]bool{}}
	if subcommand != "setup" && subcommand != "account" {
		return f
	}
	flags.StringVar(&f.sourceKind, "source-kind", "", "Source of the task a setup report or token is for: dashboard or slack")
	flags.StringVar(&f.waitUntil, "wait-until", "", "Until when a login program waits for a person at a captcha page, RFC 3339")
	flags.StringVar(&f.requestID, "request-id", "", "Request ID of the one setup task to take")
	flags.StringVar(&f.thread, "source-thread", "", "Thread key CHANNEL/TS of the Slack message a write came from")
	flags.StringVar(&f.ts, "source-ts", "", "ts of the Slack message a write came from")
	flags.StringVar(&f.email, "email", "", "Login email of a new account")
	flags.StringVar(&f.deviceID, "device-id", "", "Device ID of a new account, 64 lower-case hex digits")
	flags.StringVar(&f.receiverEmail, "receiver-email", "", "Email address that receives the new account's mail, optional")
	flags.StringVar(&f.provider, "proxy-provider", "", "Provider of the new account's proxy, optional")
	flags.StringVar(&f.kind, "proxy-type", "", "Type of the new account's proxy, optional")
	flags.StringVar(&f.plan, "plan", "", "Plan of a new account")
	flags.StringVar(&f.host, "proxy-host", "", "Proxy host of a new account")
	flags.IntVar(&f.port, "proxy-port", 0, "Proxy port of a new account")
	flags.StringVar(&f.ip, "expected-egress-ip", "", "IP the new account's proxy exits from")
	flags.StringVar(&f.userRef, "proxy-username-ref", "", "Reference of the proxy user name of a new account")
	flags.StringVar(&f.passRef, "proxy-password-ref", "", "Reference of the proxy password of a new account")
	flags.Int64Var(&f.rev, "rev", 0, "Revision of the account the command is made against")
	return f
}

// note records which flags were set; it runs after the flags are parsed.
func (a *agentFlags) note(flags *flag.FlagSet) {
	flags.Visit(func(f *flag.Flag) { a.given[f.Name] = true })
}

// only reports whether no flag of the agent set is given but the allowed ones.
func (a *agentFlags) only(allowed ...string) bool {
	for name := range a.given {
		if !slices.Contains(allowed, name) && !isGeneralFlag(name) {
			return false
		}
	}
	return true
}

// isGeneralFlag says whether name is a flag of the setup commands that agentFlags does not
// own.
func isGeneralFlag(name string) bool {
	return slices.Contains([]string{"admin-socket", "run-id", "state", "step", "reason"}, name)
}

// has reports whether every named flag is given.
func (a *agentFlags) has(names ...string) bool {
	for _, name := range names {
		if !a.given[name] {
			return false
		}
	}
	return true
}

// source is the source of a write in the form the gateway reads. The kind is fixed here,
// not a flag.
func (a *agentFlags) source() map[string]string {
	return map[string]string{"kind": "slack", "thread": a.thread, "ts": a.ts}
}

// revBody is the body of verify, activate and setup request.
func (a *agentFlags) revBody() map[string]any {
	body := map[string]any{"source": a.source()}
	if a.given["rev"] {
		body["rev"] = a.rev
	}
	return body
}

// accountAgentCommand builds `account create|show|verify|activate <alias>`. A misuse,
// such as a missing flag, is refused here without a request.
func accountAgentCommand(a *agentFlags, socket string, rest []string) (adminCommand, error) {
	if len(rest) != 2 || !accountAlias.MatchString(rest[1]) {
		return adminCommand{}, errAdminUsage
	}
	for name := range a.given {
		if isGeneralFlag(name) && name != "admin-socket" {
			return adminCommand{}, errAdminUsage
		}
	}
	alias := rest[1]
	cmd := adminCommand{socket: socket, method: http.MethodPost, timeout: accountCommandTimeout}
	switch rest[0] {
	case "show":
		if len(a.given) != 1 {
			return adminCommand{}, errAdminUsage
		}
		cmd.method, cmd.path, cmd.timeout = http.MethodGet, "/accounts/"+alias, 10*time.Second
	case "create":
		fields := []string{"email", "device-id", "plan", "proxy-host", "proxy-port", "expected-egress-ip", "proxy-username-ref", "proxy-password-ref", "source-thread", "source-ts"}
		if !a.only(append([]string{"receiver-email", "proxy-provider", "proxy-type"}, fields...)...) || !a.has(fields...) {
			return adminCommand{}, errAdminUsage
		}
		cmd.path = "/accounts"
		cmd.body = a.createBody(alias)
		cmd.timeout = 30 * time.Second
	case "set":
		if !a.only("rev", "receiver-email", "proxy-provider", "proxy-type", "source-thread", "source-ts") || !a.has("rev", "source-thread", "source-ts") || !a.setsAField() {
			return adminCommand{}, errAdminUsage
		}
		cmd.method, cmd.path, cmd.body = http.MethodPatch, "/accounts/"+alias, a.setBody()
	case "verify", "activate":
		required := []string{"source-thread", "source-ts"}
		if rest[0] == "activate" {
			required = append(required, "rev")
		}
		if !a.only("rev", "source-thread", "source-ts") || !a.has(required...) {
			return adminCommand{}, errAdminUsage
		}
		cmd.path, cmd.body = "/accounts/"+alias+"/"+rest[0], a.revBody()
	default:
		return adminCommand{}, errAdminUsage
	}
	return cmd, nil
}

// createBody is the body of `account create`: the optional flags are in it only when given.
func (a *agentFlags) createBody(alias string) map[string]any {
	proxy := map[string]any{"host": a.host, "port": a.port, "expected_egress_ip": a.ip, "username_ref": a.userRef, "password_ref": a.passRef}
	body := map[string]any{"alias": alias, "email": a.email, "device_id": a.deviceID, "plan": a.plan, "source": a.source(), "proxy": proxy}
	if a.given["receiver-email"] {
		body["receiver_email"] = a.receiverEmail
	}
	if a.given["proxy-provider"] {
		proxy["provider"] = a.provider
	}
	if a.given["proxy-type"] {
		proxy["type"] = a.kind
	}
	return body
}

// setsAField reports whether a flag of `account set` that names a field is given.
func (a *agentFlags) setsAField() bool {
	return a.given["receiver-email"] || a.given["proxy-provider"] || a.given["proxy-type"]
}

// setBody is the body of `account set`: the fields given, an empty value clearing one.
func (a *agentFlags) setBody() map[string]any {
	body := a.revBody()
	if a.given["receiver-email"] {
		body["receiver_email"] = a.receiverEmail
	}
	proxy := map[string]any{}
	if a.given["proxy-provider"] {
		proxy["provider"] = a.provider
	}
	if a.given["proxy-type"] {
		proxy["type"] = a.kind
	}
	if len(proxy) > 0 {
		body["proxy"] = proxy
	}
	return body
}

// accountCommandTimeout bounds a verify or an activate: the gateway's three checks, each
// bounded together by two minutes, a reload and the answer.
const accountCommandTimeout = 2*time.Minute + 30*time.Second + 30*time.Second
