// redcoast is the single entrypoint of the inference gateway. "redcoast claude serve"
// runs the Claude gateway and "redcoast codex serve" the Codex one, each its own
// process: it authenticates session credentials, forwards each request through the
// account its session is bound to and records bounded, redacted observations. The
// forwarding lives in package claude, with package codex as the Codex provider; this
// program only parses the configuration and runs it. The other "redcoast <provider>"
// commands manage a running gateway or its store.
package main

import (
	"cmp"
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
	"github.com/Luolc/redcoast/codex"
	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
	"github.com/cloudflare/tableflip"
)

// renewalSweep is how often a gateway whose provider renews credentials checks every
// account's for one that is due; the Codex CLI renews within five minutes of expiry.
const renewalSweep = time.Minute

// quotaRecheck is how often the gateway refreshes the accounts whose quota reading at or
// over the soft threshold outlived its reset; a reading stays stale for at most this
// long after the reset.
const quotaRecheck = 5 * time.Minute

// accountsLoadTimeout bounds reading the inventory and resolving its references at
// startup, the SDK's client setup included.
const accountsLoadTimeout = 60 * time.Second

// credentialsInputTimeout is how long the test entry waits for the JSON object on stdin.
const credentialsInputTimeout = 5 * time.Second

// errUsage is returned when the command line names no provider and command.
var errUsage = errors.New(`usage:
  redcoast claude|codex serve --config <path>
  redcoast claude|codex plan --session-db <path> ...
  redcoast claude|codex thresholds --session-db <path> <soft> <hard>
  redcoast claude|codex reload|handoff|status|sessions|pause|resume|refresh|machine ... --admin-socket <path>
a command given without its arguments prints its own usage`)

// main runs "redcoast <provider> serve" until SIGINT or SIGTERM, then waits for capture
// writes still in progress. Any startup or shutdown failure exits with status 1. Any
// other command runs once and exits.
func main() {
	provider, command, err := providerCommand(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if command[0] != "serve" {
		if err := runSubcommand(context.Background(), command); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := loadConfig(provider, command[1:])
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
	gateway, form, set, err := loadProvider(ctx, cfg, resolve)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("accounts: %s", strings.Join(set.Candidates(), " "))
	store, err := openStore(ctx, cfg.sessionDB, form)
	if err != nil {
		log.Fatal(err)
	}
	backups, err := newBackup(ctx, cfg, store, resolve)
	if err != nil {
		log.Fatal(err)
	}
	accounts := claude.NewAccounts(set, store)
	defer accounts.CloseIdleConnections()
	egress := claude.NewEgress(accounts, store, cfg.upstream)
	egress.Problems = gateway.Problems
	// An account whose exit IP differs is paused before the gateway serves.
	if err := egress.Sweep(ctx); err != nil {
		log.Fatal(err)
	}
	admin := claude.NewAdmin(accounts, store, reloader(cfg, accounts, resolve, egress))
	admin.Handoff = upg.Upgrade
	entrypoint, err := claude.NewHandler(cfg.upstream, gateway, cfg.captureDir, store, accounts, cfg.limits)
	if err != nil {
		log.Fatal(err)
	}
	offerAccountActions(admin, gateway, entrypoint)
	dashboard, err := newDashboard(cfg, store, backups, egress, entrypoint, started)
	if err != nil {
		log.Fatal(err)
	}
	sessions := newSessionServer(cfg, store, accounts)
	l, err := listenAndNotify(upg, cfg)
	if err != nil {
		log.Fatal(err)
	}
	serving, stopServing := serveUntilHandoff(ctx, upg.Exit(), drainTimeout)
	// A server failure stops the others; Run still waits for tunnels and record writes
	// in progress, and the store is closed after them, before the failure is reported.
	servers := []func(context.Context) error{
		func(ctx context.Context) error { return egress.Run(ctx, cfg.egressInterval) },
		func(ctx context.Context) error { return entrypoint.RunRenewals(ctx, renewalSweep) },
		func(ctx context.Context) error { return entrypoint.RunQuotaRechecks(ctx, quotaRecheck) },
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

// offerAccountActions gives the management interface the quota refresh and the login
// import of entrypoint when gateway's provider has them.
func offerAccountActions(admin *claude.Admin, gateway claude.Provider, entrypoint *claude.Entrypoint) {
	if gateway.Probe != nil {
		admin.Refresh = entrypoint.Refresh
	}
	if gateway.Import != nil && gateway.Renew != nil {
		admin.Import = entrypoint.Import
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

// sessionAccounts is what the session interface reads of the account set: the
// candidates and those the egress check set aside (claude.Accounts).
type sessionAccounts interface {
	Candidates() []string
	SetAside() []string
}

// newSessionServer returns the session interface over store, refusing a session when
// no account of accounts could serve it, set-aside ones left out.
func newSessionServer(cfg config, store *session.Store, accounts sessionAccounts) *session.Server {
	sessions := session.NewServer(store, entrypoints(cfg), accounts.Candidates)
	sessions.SetAside = accounts.SetAside
	return sessions
}

// readOnly is how /health and the dashboard are served: only to requests that name the
// gateway by one of cfg.hostNames.
func (cfg config) readOnly(handler http.Handler) http.Handler {
	return claude.HostGuard(cfg.hostNames, handler)
}

// openStore opens the session database, handing out session credentials in form (nil
// for the store's own), and refuses a file that fails quick_check: the gateway then
// does not start and waits for a restore from backup, instead of starting over on an
// empty file that would lose plans, thresholds and account pauses.
func openStore(ctx context.Context, path string, form session.CredentialForm) (*session.Store, error) {
	store, err := session.Open(ctx, path, session.Config{Form: form})
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
		Dir:         backupDir(cfg.sessionDB),
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

// providerCommand returns the provider, claude or codex, and the arguments after it,
// the first of them "serve", "plan", "thresholds" or a management subcommand.
func providerCommand(arguments []string) (string, []string, error) {
	if len(arguments) < 2 || arguments[0] != "claude" && arguments[0] != "codex" || arguments[1] != "serve" && arguments[1] != "plan" && arguments[1] != "thresholds" && !adminCommands[arguments[1]] {
		return "", nil, errUsage
	}
	return arguments[0], arguments[1:], nil
}

// newProvider returns what the configured provider does and the form of its session
// credentials: Claude hands out the store's own credentials; Codex hands out JWTs
// signed with the key codex.session_key references, read once at startup.
func newProvider(ctx context.Context, cfg config, resolve claude.Resolver) (claude.Provider, session.CredentialForm, error) {
	if cfg.provider != "codex" {
		return claude.Claude, nil, nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	key, err := resolve(resolveCtx, cfg.codex.SessionKey)
	if err != nil {
		return claude.Provider{}, nil, fmt.Errorf("codex.session_key: %v", err)
	}
	form, err := codex.NewSessionForm([]byte(key))
	if err != nil {
		return claude.Provider{}, nil, fmt.Errorf("codex.session_key: %v", err)
	}
	return codex.New(cfg.codex.AccountsDir, cmp.Or(cfg.codex.TokenURL, codex.TokenURL)), form, nil
}

// runSubcommand runs "plan", "thresholds" or one of the management subcommands.
func runSubcommand(ctx context.Context, arguments []string) error {
	switch arguments[0] {
	case "plan":
		cmd, err := parsePlanCommand(arguments[1:])
		if err != nil {
			return err
		}
		return runPlan(ctx, cmd, os.Stdout)
	case "thresholds":
		cmd, err := parseThresholdsCommand(arguments[1:])
		if err != nil {
			return err
		}
		return runThresholds(ctx, cmd)
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

// loadProvider builds the configured provider and the form of its session credentials,
// and loads its inventory.
func loadProvider(ctx context.Context, cfg config, resolve claude.Resolver) (claude.Provider, session.CredentialForm, *claude.AccountSet, error) {
	provider, form, err := newProvider(ctx, cfg, resolve)
	if err != nil {
		return claude.Provider{}, nil, nil, err
	}
	set, err := loadAccounts(ctx, cfg, provider, resolve)
	return provider, form, set, err
}

// loadAccounts loads the inventory for provider with resolve. Any failure is reported
// as a whole; the gateway then does not start.
func loadAccounts(ctx context.Context, cfg config, provider claude.Provider, resolve claude.Resolver) (*claude.AccountSet, error) {
	loadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	return claude.LoadAccounts(loadCtx, cfg.inventory, provider, resolve, cfg.accountConcurrency)
}
