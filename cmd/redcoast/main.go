// redcoast is the single entrypoint of the inference gateway. "redcoast claude serve"
// runs the Claude gateway: it authenticates session credentials, forwards each request
// through the account its session is bound to and records bounded, redacted
// observations. The forwarding lives in package claude; this program only parses the
// configuration and runs it. The other "redcoast claude" commands manage a running
// gateway or its store.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/redcoast/backup"
	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
	"github.com/cloudflare/tableflip"
)

// accountsLoadTimeout bounds reading the inventory and resolving its references at
// startup, the SDK's client setup included.
const accountsLoadTimeout = 60 * time.Second

// credentialsInputTimeout is how long the test entry waits for the JSON object on stdin.
const credentialsInputTimeout = 5 * time.Second

// errUsage is returned when the command line names no provider and command.
var errUsage = errors.New(`usage:
  redcoast claude serve --config <path>
  redcoast claude plan --session-db <path> ...
  redcoast claude reload|handoff|status|sessions|pause|resume|machine ... --admin-socket <path>
a command given without its arguments prints its own usage`)

// main runs "redcoast claude serve" until SIGINT or SIGTERM, then waits for capture
// writes still in progress. Any startup or shutdown failure exits with status 1. Any
// other command runs once and exits.
func main() {
	command, err := claudeCommand(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if command[0] != "serve" {
		if err := runSubcommand(context.Background(), command); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := loadConfig(command[1:])
	if err != nil {
		log.Fatal(err)
	}
	if cfg.warning != "" {
		log.Print(cfg.warning)
	}
	started := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A handoff starts os.Args[0], the unit's path through the current link, so the new
	// process runs the release current points at then.
	upg, err := tableflip.New(tableflip.Options{UpgradeTimeout: handoffTimeout})
	if err != nil {
		log.Fatal(err)
	}
	resolve, err := newResolver(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	set, err := loadAccounts(ctx, cfg, resolve)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("accounts: %s", strings.Join(set.Candidates(), " "))
	store, err := openStore(ctx, cfg.sessionDB)
	if err != nil {
		log.Fatal(err)
	}
	backups, err := newBackup(ctx, cfg, store, resolve)
	if err != nil {
		log.Fatal(err)
	}
	accounts := claude.NewAccounts(set, store)
	defer accounts.CloseIdleConnections()
	egress := claude.NewEgress(accounts, store)
	// An account whose exit IP differs is paused before the gateway serves.
	if err := egress.Sweep(ctx); err != nil {
		log.Fatal(err)
	}
	admin := claude.NewAdmin(accounts, store, reloader(cfg, accounts, resolve, egress))
	admin.Handoff = upg.Upgrade
	entrypoint, err := claude.NewHandler(cfg.upstream, cfg.captureDir, store, accounts, cfg.limits)
	if err != nil {
		log.Fatal(err)
	}
	dashboard, err := newDashboard(cfg, store, backups, egress, entrypoint, started)
	if err != nil {
		log.Fatal(err)
	}
	sessions := session.NewServer(store, entrypoints(cfg), accounts.Candidates)
	l, err := listenAndNotify(upg, cfg)
	if err != nil {
		log.Fatal(err)
	}
	serving, stopServing := serveUntilHandoff(ctx, upg.Exit(), drainTimeout)
	// A server failure stops the others; Run still waits for tunnels and record writes
	// in progress, and the store is closed after them, before the failure is reported.
	servers := []func(context.Context) error{
		func(ctx context.Context) error { return egress.Run(ctx, cfg.egressInterval) },
		func(ctx context.Context) error {
			return claude.Serve(ctx, l.health, cfg.readOnly(egress.HealthHandler()))
		},
		func(ctx context.Context) error {
			return claude.Serve(ctx, l.dashboard, cfg.readOnly(dashboard.Handler()))
		},
		func(ctx context.Context) error { return claude.Serve(ctx, l.reverse, entrypoint.Reverse) },
		func(ctx context.Context) error { return claude.Serve(ctx, l.forward, entrypoint.Forward) },
		func(ctx context.Context) error { return session.ServeUnixListener(ctx, l.session, sessions.Handler()) },
		func(ctx context.Context) error { return session.ServeUnixListener(ctx, l.admin, admin.Handler()) },
		func(ctx context.Context) error {
			if l.sessionTCP == nil {
				<-ctx.Done()
				return nil
			}
			return sessions.ServeTCPListener(ctx, l.sessionTCP)
		},
		afterOldProcess(upg, func(ctx context.Context) error { return store.RunRetention(ctx, cfg.retention) }),
	}
	if backups != nil {
		servers = append(servers, afterOldProcess(upg, backups.Run))
	}
	err = entrypoint.Run(serving, servers...)
	stopServing()
	// Without a handoff, Stop closes the listeners and removes the socket files; after
	// one they belong to the new process and Stop leaves them.
	upg.Stop()
	<-upg.Exit()
	if err := errors.Join(err, dashboard.Close(), store.Close()); err != nil {
		log.Fatal(err)
	}
}

// entrypoints are the proxy addresses sessions are handed: the listen addresses, or with
// TLS the server name and the listen ports.
func entrypoints(cfg config) session.Entrypoints {
	if cfg.tls == nil {
		return session.Entrypoints{ReverseProxy: "http://" + cfg.listen, ForwardProxy: cfg.forwardListen}
	}
	// The addresses were checked to be IP literals with a port.
	_, reverse, _ := net.SplitHostPort(cfg.listen)
	_, forward, _ := net.SplitHostPort(cfg.forwardListen)
	return session.Entrypoints{ReverseProxy: "https://" + net.JoinHostPort(cfg.serverName, reverse), ForwardProxy: net.JoinHostPort(cfg.serverName, forward)}
}

// readOnly is how /health and the dashboard are served: only to requests that name the
// gateway by one of cfg.hostNames.
func (cfg config) readOnly(handler http.Handler) http.Handler {
	return claude.HostGuard(cfg.hostNames, handler)
}

// openStore opens the session database and refuses a file that fails quick_check: the
// gateway then does not start and waits for a restore from backup, instead of starting
// over on an empty file that would lose plans, thresholds and account pauses.
func openStore(ctx context.Context, path string) (*session.Store, error) {
	store, err := session.Open(ctx, path, session.Config{})
	if err != nil {
		return nil, err
	}
	if err := store.QuickCheck(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

// dashboard is the read-only dashboard with the read-only connection it closes.
type dashboard struct {
	*claude.Dashboard
	reader *session.Reader
}

// Close closes the dashboard's connection.
func (d dashboard) Close() error {
	return d.reader.Close()
}

// newDashboard opens a read-only connection to the store and builds the dashboard over
// it and this process's state.
func newDashboard(cfg config, store *session.Store, backups *backup.Backup, egress *claude.Egress, entrypoint *claude.Entrypoint, started time.Time) (dashboard, error) {
	reader, err := session.OpenReadOnly(cfg.sessionDB, session.Config{}, cfg.limits.MaxTunnel)
	if err != nil {
		return dashboard{}, err
	}
	dc := claude.DashboardConfig{Reader: reader, Store: store, DBPath: cfg.sessionDB, Version: version, Inventory: cfg.inventory, Started: started, Tunnels: entrypoint.Tunnels, Dir: cfg.dashboardDir}
	if cfg.dashboardDir != "" {
		if info, err := os.Stat(filepath.Join(cfg.dashboardDir, "index.html")); err != nil || !info.Mode().IsRegular() {
			log.Printf("dashboard: %s has no index.html; / answers 404, /dashboard.json is served", cfg.dashboardDir)
		}
	}
	if backups != nil {
		dc.Backup = backups.Status
	}
	return dashboard{Dashboard: claude.NewDashboard(egress, dc), reader: reader}, nil
}

// newBackup returns the hourly backup of store to cfg.backup with the writer's key from
// resolve, or nil when no backup is configured. The local snapshot goes to a directory
// next to the database.
func newBackup(ctx context.Context, cfg config, store *session.Store, resolve claude.Resolver) (*backup.Backup, error) {
	if cfg.backup == nil {
		return nil, nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	keyID, err := resolve(resolveCtx, cfg.backup.AccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("backup access key id: %v", err)
	}
	secret, err := resolve(resolveCtx, cfg.backup.SecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("backup secret access key: %v", err)
	}
	return backup.New(store, backup.Config{
		Endpoint:    bucketURL(cfg.backup.Endpoint, cfg.backup.Bucket),
		Region:      cfg.backup.Region,
		Prefix:      cfg.backup.Prefix,
		Credentials: backup.Credentials{AccessKeyID: keyID, SecretAccessKey: secret},
		Dir:         filepath.Join(filepath.Dir(cfg.sessionDB), "backup"),
	}), nil
}

// reloader returns the reload the management interface runs. It resolves the token
// reference again, so a rotated service account token is picked up; the stdin test entry keeps
// the one object it read at startup.
func reloader(cfg config, accounts *claude.Accounts, startup claude.Resolver, egress *claude.Egress) func(context.Context) (claude.Reload, error) {
	return func(ctx context.Context) (claude.Reload, error) {
		reloadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
		defer cancel()
		resolve := startup
		if !cfg.credentialsStdin {
			var err error
			if resolve, err = newResolver(reloadCtx, cfg); err != nil {
				return claude.Reload{}, err
			}
		}
		return accounts.Reload(reloadCtx, cfg.inventory, resolve, egress.Check)
	}
}

// claudeCommand returns the arguments after "redcoast claude", the first of them
// "serve", "plan" or a management subcommand.
func claudeCommand(arguments []string) ([]string, error) {
	if len(arguments) < 2 || arguments[0] != "claude" || arguments[1] != "serve" && arguments[1] != "plan" && !adminCommands[arguments[1]] {
		return nil, errUsage
	}
	return arguments[1:], nil
}

// runSubcommand runs "plan" or one of the management subcommands.
func runSubcommand(ctx context.Context, arguments []string) error {
	if arguments[0] == "plan" {
		cmd, err := parsePlanCommand(arguments[1:])
		if err != nil {
			return err
		}
		return runPlan(ctx, cmd)
	}
	cmd, err := parseAdminCommand(arguments)
	if err != nil {
		return err
	}
	return runAdmin(ctx, cmd, os.Stdout)
}

// newResolver builds the resolver the configuration asks for: the stdin test entry, or
// env:// and file:// references with op:// ones through the 1Password SDK when a
// service account token is configured.
func newResolver(ctx context.Context, cfg config) (claude.Resolver, error) {
	loadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	if cfg.credentialsStdin {
		inputCtx, cancelInput := context.WithTimeout(loadCtx, credentialsInputTimeout)
		defer cancelInput()
		return stdinResolver(inputCtx, os.Stdin)
	}
	resolve := credential.New(nil)
	if cfg.tokenRef == "" {
		return resolve, nil
	}
	token, err := resolve(loadCtx, cfg.tokenRef)
	if err != nil {
		return nil, fmt.Errorf("1Password service account token: %v", err)
	}
	onePassword, err := onePasswordResolver(loadCtx, token)
	if err != nil {
		return nil, err
	}
	return credential.New(onePassword), nil
}

// loadAccounts loads the inventory with resolve. Any failure is reported as a whole;
// the gateway then does not start.
func loadAccounts(ctx context.Context, cfg config, resolve claude.Resolver) (*claude.AccountSet, error) {
	loadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	return claude.LoadAccounts(loadCtx, cfg.inventory, resolve, cfg.accountConcurrency)
}
