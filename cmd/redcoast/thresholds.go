package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/Luolc/redcoast/session"
)

// thresholdsCommand is a parsed `redcoast claude thresholds ...` invocation.
type thresholdsCommand struct {
	sessionDB  string
	soft, hard string
}

// errThresholdsUsage is returned when the thresholds subcommand is misused.
var errThresholdsUsage = errors.New(`usage:
  redcoast claude thresholds --session-db <path> <soft> <hard>
both are fractions in [0, 1] (for example 0.9 0.95), soft not above hard; the gateway
reads them on its next request`)

// parseThresholdsCommand parses the arguments after "thresholds". The values are
// checked by the store, with the rules it reads them by.
func parseThresholdsCommand(arguments []string) (thresholdsCommand, error) {
	flags := flag.NewFlagSet("redcoast claude thresholds", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sessionDB := flags.String("session-db", "", "SQLite file of the gateway")
	flagArguments, rest := splitFlags(flags, arguments)
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *sessionDB == "" || len(rest) != 2 {
		return thresholdsCommand{}, errThresholdsUsage
	}
	return thresholdsCommand{sessionDB: *sessionDB, soft: rest[0], hard: rest[1]}, nil
}

// runThresholds writes both thresholds to the store in one transaction.
func runThresholds(ctx context.Context, cmd thresholdsCommand) error {
	store, err := session.Open(ctx, cmd.sessionDB, session.Config{})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return store.SetThresholds(ctx, cmd.soft, cmd.hard)
}
