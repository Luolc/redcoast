package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// planCommand is a parsed `redcoast claude plan ...` invocation: one plan unit period or
// one account plan period to write.
type planCommand struct {
	sessionDB string
	unit      *session.PlanUnit
	period    *session.PlanPeriod
}

// errPlanUsage is returned when the plan subcommand is misused.
var errPlanUsage = errors.New(`usage:
  redcoast claude plan --session-db <path> unit <plan> <quota-unit> <monthly-usd> <from> [<to>]
  redcoast claude plan --session-db <path> schedule <alias> <plan> <from> [<to>] [--note <text>]
times are RFC 3339 (for example 2026-10-29T00:00:00Z); an omitted <to> is open-ended`)

// parsePlanCommand parses the arguments after "plan".
func parsePlanCommand(arguments []string) (planCommand, error) {
	flags := flag.NewFlagSet("redcoast claude plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sessionDB := flags.String("session-db", "", "SQLite file of the gateway")
	note := flags.String("note", "", "Note saved with an account plan period")
	flagArguments, rest := splitFlags(arguments)
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *sessionDB == "" || len(rest) == 0 {
		return planCommand{}, errPlanUsage
	}
	cmd := planCommand{sessionDB: *sessionDB}
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

// runPlan writes the parsed period to the store.
func runPlan(ctx context.Context, cmd planCommand) error {
	store, err := session.Open(ctx, cmd.sessionDB, session.Config{})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if cmd.unit != nil {
		return store.SetPlanUnit(ctx, *cmd.unit)
	}
	return store.SetPlanSchedule(ctx, *cmd.period)
}
