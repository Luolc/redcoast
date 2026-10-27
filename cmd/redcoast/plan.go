package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// planCommand is a parsed `redcoast claude plan ...` invocation: a plan's quota unit
// or an account's plan to set from a time on, or an account's plan to end at a time.
// A zero time is the moment of the write.
type planCommand struct {
	sessionDB   string
	adminSocket string // the running gateway's, to refresh the accounts whose quota unit went up
	unit        *session.PlanUnit
	period      *session.PlanPeriod
	end         *planEnd
}

// planEnd is an account's plan to end at a time.
type planEnd struct {
	alias string
	at    time.Time
}

// errPlanUsage is returned when the plan subcommand is misused.
var errPlanUsage = errors.New(`usage:
  redcoast claude|codex plan --session-db <path> [--admin-socket <path>] unit <plan> <quota-unit> <monthly-usd> [<from>]
  redcoast claude|codex plan --session-db <path> [--admin-socket <path>] schedule <alias> <plan> [<from>] [--note <text>]
  redcoast claude|codex plan --session-db <path> end <alias> [<at>]
times are RFC 3339 (for example 2026-10-29T00:00:00Z), past or future; an omitted time
is now. unit and schedule set the plan's unit or the account's plan from that time on,
open-ended, in one transaction: the period in effect then ends there, later periods
are deleted, earlier ones stay. A write that would leave an account without a plan
with a quota unit at a time it had one is refused, and so is a schedule onto a plan
that lacks a quota unit at some time from <from> on; nothing is written then.
end takes the account off its plan from <at> on: the only way to leave it without one.
With --admin-socket, every account whose quota unit is higher now than before the
change has its quota read again by the gateway (as with refresh)`)

// parsePlanCommand parses the arguments after "plan".
func parsePlanCommand(arguments []string) (planCommand, error) {
	flags := flag.NewFlagSet("redcoast claude plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sessionDB := flags.String("session-db", "", "SQLite file of the gateway")
	note := flags.String("note", "", "Note saved with an account plan period")
	adminSocket := flags.String("admin-socket", "", "Unix socket of the running gateway's management interface")
	flagArguments, rest := splitFlags(flags, arguments)
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *sessionDB == "" || len(rest) == 0 {
		return planCommand{}, errPlanUsage
	}
	cmd := planCommand{sessionDB: *sessionDB, adminSocket: *adminSocket}
	var err error
	switch rest[0] {
	case "unit":
		cmd.unit, err = parseUnit(rest[1:])
	case "schedule":
		cmd.period, err = parseSchedule(rest[1:], *note)
	case "end":
		cmd.end, err = parseEnd(rest[1:])
	default:
		err = errPlanUsage
	}
	if err != nil {
		return planCommand{}, err
	}
	return cmd, nil
}

// splitFlags separates interleaved flags from positional arguments: every "-name value"
// pair (or "-name=value", or a boolean flag of flags alone) is a flag, the rest are
// positional.
func splitFlags(flags *flag.FlagSet, arguments []string) (flagArguments, rest []string) {
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch {
		case !strings.HasPrefix(argument, "-") || argument == "-":
			rest = append(rest, argument)
		case strings.Contains(argument, "=") || i+1 == len(arguments) || isBoolFlag(flags, argument):
			flagArguments = append(flagArguments, argument)
		default:
			flagArguments = append(flagArguments, argument, arguments[i+1])
			i++
		}
	}
	return flagArguments, rest
}

// isBoolFlag reports whether argument names a boolean flag of flags, which takes no
// value.
func isBoolFlag(flags *flag.FlagSet, argument string) bool {
	f := flags.Lookup(strings.TrimLeft(argument, "-"))
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// parseUnit parses "<plan> <quota-unit> <monthly-usd> [<from>]".
func parseUnit(rest []string) (*session.PlanUnit, error) {
	if len(rest) < 3 || len(rest) > 4 {
		return nil, errPlanUsage
	}
	unit := session.PlanUnit{Plan: rest[0]}
	var err error
	if unit.QuotaUnit, err = parsePositive(rest[1]); err != nil {
		return nil, err
	}
	if unit.MonthlyUSD, err = parsePositive(rest[2]); err != nil {
		return nil, err
	}
	if unit.From, err = optionalTime(rest[3:]); err != nil {
		return nil, err
	}
	return &unit, nil
}

// parseSchedule parses "<alias> <plan> [<from>]".
func parseSchedule(rest []string, note string) (*session.PlanPeriod, error) {
	if len(rest) == 4 {
		return nil, fmt.Errorf("%w\nschedule takes no <to>: to take an account off its plan at a time, use end <alias> [<at>]", errPlanUsage)
	}
	if len(rest) < 2 || len(rest) > 3 {
		return nil, errPlanUsage
	}
	period := session.PlanPeriod{Alias: rest[0], Plan: rest[1], Source: "manual", Note: note}
	if !accountAlias.MatchString(period.Alias) {
		return nil, errors.New("invalid account alias")
	}
	var err error
	if period.From, err = optionalTime(rest[2:]); err != nil {
		return nil, err
	}
	return &period, nil
}

// parseEnd parses "<alias> [<at>]".
func parseEnd(rest []string) (*planEnd, error) {
	if len(rest) < 1 || len(rest) > 2 {
		return nil, errPlanUsage
	}
	if !accountAlias.MatchString(rest[0]) {
		return nil, errors.New("invalid account alias")
	}
	at, err := optionalTime(rest[1:])
	if err != nil {
		return nil, err
	}
	return &planEnd{alias: rest[0], at: at}, nil
}

// parsePositive parses a positive decimal number.
func parsePositive(text string) (float64, error) {
	var value float64
	if _, err := fmt.Sscanf(text, "%g", &value); err != nil || value <= 0 {
		return 0, fmt.Errorf("%q is not a positive number", text)
	}
	return value, nil
}

// optionalTime parses the one RFC 3339 time in texts, or returns the zero time when
// texts is empty.
func optionalTime(texts []string) (time.Time, error) {
	if len(texts) == 0 {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, texts[0])
	if err != nil {
		return t, fmt.Errorf("%q is not an RFC 3339 time", texts[0])
	}
	return t.UTC(), nil
}

// runPlan writes the parsed change to the store. The accounts whose quota unit at this
// moment is higher after the write than before it are refreshed over the admin socket
// when one is given; an account the gateway does not refresh, because of a pause other
// than quota (409) or because the gateway cannot refresh (501, Codex), gets a line, not
// an error. Without the socket one line says how to refresh them. A unit that stays
// the same or goes down sends nothing, and neither does a change that starts later.
func runPlan(ctx context.Context, cmd planCommand, out io.Writer) error {
	raised, err := writePlan(ctx, cmd)
	if err != nil || len(raised) == 0 {
		return err
	}
	if cmd.adminSocket == "" {
		_, err := fmt.Fprintf(out, "quota unit raised for %s; read the quota again with: redcoast claude refresh <alias> --admin-socket <path>\n", strings.Join(raised, " "))
		return err
	}
	var errs []error
	for _, alias := range raised {
		err := runAdmin(ctx, refreshCommand(cmd.adminSocket, alias), out)
		var refused *refusal
		switch {
		case errors.As(err, &refused) && (refused.status == http.StatusConflict || refused.status == http.StatusNotImplemented):
			if _, err := fmt.Fprintf(out, "%s not refreshed: %s\n", alias, refused.message); err != nil {
				return err
			}
		case err != nil:
			errs = append(errs, fmt.Errorf("plan written; %s: %v", alias, err))
		}
	}
	return errors.Join(errs...)
}

// writePlan makes the change and returns, sorted, the accounts whose quota unit now is
// higher after it than before it. A change without a time takes effect at that same
// now.
func writePlan(ctx context.Context, cmd planCommand) ([]string, error) {
	store, err := session.Open(ctx, cmd.sessionDB, session.Config{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	now := time.Now()
	before, err := store.QuotaUnits(ctx, now)
	if err != nil {
		return nil, err
	}
	switch {
	case cmd.unit != nil:
		unit := *cmd.unit
		unit.From = cmp.Or(unit.From, now)
		err = store.SetPlanUnit(ctx, unit)
	case cmd.period != nil:
		period := *cmd.period
		period.From = cmp.Or(period.From, now)
		err = store.SetPlanSchedule(ctx, period)
	default:
		err = store.EndPlan(ctx, cmd.end.alias, cmp.Or(cmd.end.at, now))
	}
	if err != nil {
		return nil, err
	}
	after, err := store.QuotaUnits(ctx, now)
	if err != nil {
		return nil, err
	}
	var raised []string
	for alias, unit := range after {
		if unit > before[alias] {
			raised = append(raised, alias)
		}
	}
	slices.Sort(raised)
	return raised, nil
}
