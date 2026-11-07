package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
)

// errInventoryUsage is returned when the inventory subcommand is misused.
var errInventoryUsage = errors.New(`usage:
  redcoast claude inventory export --session-db <path> <dir>
export writes one <alias>.yaml per account of the gateway's accounts table into <dir>,
creating it, in the format of the inventory directory the gateway imported at its first
start; a file of the same name is replaced, other files stay. The files hold credential
references and never a value. It reads the SQLite file through a read-only connection,
so it can run beside the gateway`)

// inventoryCommand is a parsed `redcoast claude inventory export` invocation.
type inventoryCommand struct {
	provider  string
	sessionDB string
	dir       string
}

// parseInventoryCommand parses the arguments after "inventory": "export <dir>" and flags.
func parseInventoryCommand(provider string, arguments []string) (inventoryCommand, error) {
	flags := flag.NewFlagSet("redcoast claude inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sessionDB := flags.String("session-db", "", "SQLite file of the gateway")
	flagArguments, rest := splitFlags(flags, arguments)
	if err := flags.Parse(flagArguments); err != nil || flags.NArg() != 0 || *sessionDB == "" || len(rest) != 2 || rest[0] != "export" || rest[1] == "" {
		return inventoryCommand{}, errInventoryUsage
	}
	return inventoryCommand{provider: provider, sessionDB: *sessionDB, dir: rest[1]}, nil
}

// runInventory exports the accounts table, and reports how many files it wrote.
func runInventory(ctx context.Context, cmd inventoryCommand, out io.Writer) error {
	if cmd.provider != "claude" {
		return errors.New("inventory export: only the Claude process keeps its accounts in the store")
	}
	reader, err := session.OpenReadOnly(cmd.sessionDB, session.Config{}, 0)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	rows, initialized, err := reader.Accounts(ctx)
	if err != nil {
		return err
	}
	if !initialized {
		return errors.New("inventory export: the gateway has not imported its inventory yet")
	}
	if err := claude.ExportInventory(cmd.dir, rows); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "exported %d accounts to %s\n", len(rows), cmd.dir)
	return err
}
