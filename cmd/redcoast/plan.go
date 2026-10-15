package main

import (
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

// planCommand is a parsed `redcoast claude plan ...` invocation: one plan unit period or
// one account plan period to write.
type planCommand struct {
	sessionDB   string
	adminSocket string // the running gateway's, to refresh the accounts whose quota unit went up
	unit        *session.PlanUnit
	period      *session.PlanPeriod
}

// errPlanUsage is returned when the plan subcommand is misused.
var errPlanUsage = errors.New(`usage:
  redcoast claude plan --session-db <path> [--admin-socket <path>] unit <plan> <quota-unit> <monthly-usd> <from> [<to>]
  redcoast claude plan --session-db <path> [--admin-socket <path>] schedule <alias> <plan> <from> [<to>] [--note <text>]
times are RFC 3339 (for example 2026-10-29T00:00:00Z); an omitted <to> is open-ended;
with --admin-socket, every account whose quota unit is higher now than before the
change has its quota read again by the gateway (as with refresh)`)

// parsePlanCommand parses the arguments after "plan".
func parsePlanCommand(arguments []string) (planCommand, error) {
	flags := flag.NewFlagSet("redcoast claude plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sessionDB := flags.String("session-db", "", "SQLite file of the gateway")
	note := flags.String("note", "", "Note saved with an account plan period")
	adminSocket := flags.String("admin-socket", "", "Unix socket of the running gateway's management interface")
	flagArguments, rest := splitFlags(arguments)
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
	default:
		err = errPlanUsage
	}
	if err != nil {
		return planCommand{}, err
	}
	return cmd, nil
}

// splitFlags separates interleaved flags from positional arguments: every "-name value"
// pair (or "-name=value") is a flag, the rest are positional.
func splitFlags(arguments []string) (flagArguments, rest []string) {
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch {
		case !strings.HasPrefix(argument, "-") || argument == "-":
			rest = append(rest, argument)
		case strings.Contains(argument, "=") || i+1 == len(arguments):
			flagArguments = append(flagArguments, argument)
		default:
			flagArguments = append(flagArguments, argument, arguments[i+1])
			i++
		}
	}
	return flagArguments, rest
}

// parseUnit parses "<plan> <quota-unit> <monthly-usd> <from> [<to>]".
func parseUnit(rest []string) (*session.PlanUnit, error) {
	if len(rest) < 4 || len(rest) > 5 {
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
	if unit.From, unit.To, err = parsePeriod(rest[3:]); err != nil {
		return nil, err
	}
	return &unit, nil
}

// parseSchedule parses "<alias> <plan> <from> [<to>]".
func parseSchedule(rest []string, note string) (*session.PlanPeriod, error) {
	if len(rest) < 3 || len(rest) > 4 {
		return nil, errPlanUsage
	}
	period := session.PlanPeriod{Alias: rest[0], Plan: rest[1], Source: "manual", Note: note}
	if !accountAlias.MatchString(period.Alias) {
		return nil, errors.New("invalid account alias")
	}
	var err error
	if period.From, period.To, err = parsePeriod(rest[2:]); err != nil {
		return nil, err
	}
	return &period, nil
}

// parsePositive parses a positive decimal number.
func parsePositive(text string) (float64, error) {
	var value float64
	if _, err := fmt.Sscanf(text, "%g", &value); err != nil || value <= 0 {
		return 0, fmt.Errorf("%q is not a positive number", text)
	}
	return value, nil
}

// parsePeriod parses one or two RFC 3339 times; a missing second one is open-ended.
func parsePeriod(texts []string) (from, to time.Time, err error) {
	if from, err = time.Parse(time.RFC3339, texts[0]); err != nil {
		return from, to, fmt.Errorf("%q is not an RFC 3339 time", texts[0])
	}
	if len(texts) == 2 {
		if to, err = time.Parse(time.RFC3339, texts[1]); err != nil {
			return from, to, fmt.Errorf("%q is not an RFC 3339 time", texts[1])
		}
		if !to.After(from) {
			return from, to, errors.New("the period's end must be after its start")
		}
	}
	return from.UTC(), to.UTC(), nil
}

// runPlan writes the parsed period to the store. The accounts whose quota unit at this
// moment is higher after the write than before it are refreshed over the admin socket
// when one is given; an account the gateway does not refresh because of a pause other
// than quota gets a line, not an error. Without the socket one line says how to refresh
// them. A unit that stays
// the same or goes down sends nothing, and neither does a period that starts later.
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
		case errors.As(err, &refused) && refused.status == http.StatusConflict:
			if _, err := fmt.Fprintf(out, "%s not refreshed: %s\n", alias, refused.message); err != nil {
				return err
			}
		case err != nil:
			errs = append(errs, fmt.Errorf("plan written; %s: %v", alias, err))
		}
	}
	return errors.Join(errs...)
}

// writePlan writes the period and returns, sorted, the accounts whose quota unit now is
// higher after the write than before it.
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
	if cmd.unit != nil {
		err = store.SetPlanUnit(ctx, *cmd.unit)
	} else {
		err = store.SetPlanSchedule(ctx, *cmd.period)
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
