package claude

import (
	"compress/gzip"
	"context"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Luolc/redcoast/backup"
	"github.com/Luolc/redcoast/session"
)

// commitName matches a full commit hash, the name of an inventory directory.
var commitName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DashboardConfig is what the dashboard reads besides the accounts and the egress
// checks. Every field is read, none is written; only QuotaRefresh starts anything.
type DashboardConfig struct {
	Provider  string          // this process's provider, claude or codex
	Links     []DashboardLink // the other providers' dashboards; nil for none
	Reader    *session.Reader // the store over a read-only connection
	Store     *session.Store  // only for its last retention run, kept in memory
	DBPath    string          // the store's file, whose -wal file is measured
	Version   string          // the running executable's release
	Inventory string          // the --inventory link
	Started   time.Time       // when the gateway started
	Tunnels   func() (open, limit int)
	Backup    func() backup.Status // nil when no backups are configured
	Dir       string               // holds the dashboard page, index.html, served at /; empty for none
	// QuotaRefresh is the refresh POST /quota/refresh starts; nil when the provider
	// cannot refresh, and the endpoint answers 501.
	QuotaRefresh *QuotaRefresh
	// Write is the write path of the accounts table; nil when the provider keeps its
	// accounts elsewhere, and the write routes answer 501.
	Write *AccountWriter
}

// DashboardLink is another provider's dashboard, which the page offers as a link: the
// page navigates there and reads nothing of it. URL is an http or https URL without a
// user name or password, or ":<port>" for the page's own scheme and host at that port,
// so that the link follows whatever name or tunnel reached this dashboard.
type DashboardLink struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
}

// Dashboard is the view of the gateway: GET /dashboard.json, the page at / when a
// directory is configured, and POST /quota/refresh, its one action. It reads the
// store through a read-only connection and the in-memory state of this process, and
// shows no credential, credential hash or full session ID.
type Dashboard struct {
	egress *Egress
	cfg    DashboardConfig

	// Test hooks: snapshot is the computation of the snapshot, every the rest between two
	// background computations.
	snapshot func(context.Context, []string) (session.Snapshot, error)
	every    func() <-chan time.Time
	snaps    snapshots
}

// snapshotInterval is how long the background computation of the snapshot rests after
// each run. Computing it reads the store's last 7 days and takes seconds of a core on a
// large store; the page reloads every 30 s and the monitors poll every minute, and all of
// them read the one snapshot instead of each computing its own. Nothing else the dashboard
// shows is computed ahead.
const snapshotInterval = 30 * time.Second

// snapshots holds the latest snapshot. compute is a slot of one, held by whoever computes,
// so that one computation runs at a time and a request that needs one waits for the one
// under way.
type snapshots struct {
	compute chan struct{}
	mu      sync.Mutex
	latest  *computedSnapshot // nil until a computation succeeds
}

// computedSnapshot is a snapshot with the set whose candidates it was computed for.
type computedSnapshot struct {
	set  *AccountSet
	snap session.Snapshot
}

func (s *snapshots) load() *computedSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// NewDashboard returns the dashboard over the egress checks' accounts.
func NewDashboard(egress *Egress, cfg DashboardConfig) *Dashboard {
	d := &Dashboard{egress: egress, cfg: cfg, snaps: snapshots{compute: make(chan struct{}, 1)},
		every: func() <-chan time.Time { return time.After(snapshotInterval) }}
	if cfg.Reader != nil {
		d.snapshot = cfg.Reader.Snapshot
	}
	return d
}

// RunSnapshots computes the snapshot for the current account set, unless a request
// already has, and again snapshotInterval after each computation ends, until ctx is
// canceled; a computation under way stops with it. A failed computation is logged and
// leaves the snapshot before it to be shown, whose at tells its age. Without a Reader it
// only waits for ctx.
func (d *Dashboard) RunSnapshots(ctx context.Context) error {
	if d.snapshot == nil {
		<-ctx.Done()
		return nil
	}
	for again := false; ; again = true {
		if _, err := d.refresh(ctx, d.egress.accounts.set.Load(), again); err != nil && ctx.Err() == nil {
			log.Printf("dashboard: snapshot: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-d.every():
		}
	}
}

// refresh computes the snapshot for set's candidates and keeps it when it succeeds. It
// waits for the computation under way first; without again, it then computes only when
// the latest snapshot is not for set.
func (d *Dashboard) refresh(ctx context.Context, set *AccountSet, again bool) (session.Snapshot, error) {
	s := &d.snaps
	select {
	case s.compute <- struct{}{}:
	case <-ctx.Done():
		return session.Snapshot{}, ctx.Err()
	}
	defer func() { <-s.compute }()
	if latest := s.load(); !again && latest != nil && latest.set == set {
		return latest.snap, nil
	}
	snap, err := d.snapshot(ctx, set.Candidates())
	if err != nil {
		return session.Snapshot{}, err
	}
	s.mu.Lock()
	s.latest = &computedSnapshot{set: set, snap: snap}
	s.mu.Unlock()
	return snap, nil
}

// latestSnapshot returns the latest snapshot without computing, when it is for set. When
// it is not, before the first computation or after a reload swapped in another set, the
// request waits for a snapshot for set, so that an account a write added, removed or
// changed shows at once: the one under way, or one it computes itself. If that fails, it
// returns the latest snapshot for another set, and with none the error.
func (d *Dashboard) latestSnapshot(ctx context.Context, set *AccountSet) (session.Snapshot, error) {
	if latest := d.snaps.load(); latest != nil && latest.set == set {
		return latest.snap, nil
	}
	snap, err := d.refresh(ctx, set, false)
	if err != nil {
		if latest := d.snaps.load(); latest != nil {
			return latest.snap, nil
		}
		return session.Snapshot{}, err
	}
	return snap, nil
}

// dashboardView is the JSON answer. Its Accounts hides the embedded snapshot's, which
// it extends with this process's state.
type dashboardView struct {
	Health   health             `json:"health"`
	Gateway  gatewayView        `json:"gateway"`
	Accounts []dashboardAccount `json:"accounts"`
	// Unserved are the accounts of the accounts table that the gateway does not serve
	// (testing, paused, suspended, retired, or not on the gateway path); always a list.
	Unserved []accountConfig `json:"unserved_accounts"`
	// AccountsEditable is whether this process keeps its accounts where they can be
	// edited; Write says whether this request's source may write; ProxyRefOptions are the
	// proxy logins accounts use, the only ones a write can choose.
	AccountsEditable bool             `json:"accounts_editable"`
	Write            writeView        `json:"write"`
	ProxyRefOptions  []proxyRefOption `json:"proxy_ref_options"`
	session.Snapshot
}

// writeView says whether the account writes are on for the request: Reason is empty
// when they are, and CanCreate when a new account can be created, which needs a token
// reference template.
type writeView struct {
	Enabled   bool   `json:"enabled"`
	Reason    string `json:"reason,omitempty"`
	CanCreate bool   `json:"can_create"`
}

// gatewayView is the overview. Retention, backup, egress and in-flight figures are this
// process's memory: they cover the time since it started.
type gatewayView struct {
	Provider        string               `json:"provider"`         // claude or codex
	Dashboards      []DashboardLink      `json:"dashboards"`       // the other providers' dashboards, each URL absolute
	Version         string               `json:"version"`          // the release this binary was built from
	InventoryCommit string               `json:"inventory_commit"` // the link's target now; empty when it names no commit
	Started         time.Time            `json:"started"`
	WALBytes        int64                `json:"wal_bytes"`
	Retention       session.RetentionRun `json:"retention"`
	Backup          *backupView          `json:"backup"` // null when no backups are configured
	OpenTunnels     int                  `json:"open_tunnels"`
	TunnelLimit     int                  `json:"tunnel_limit"`
	QuotaRefresh    *QuotaRefreshState   `json:"quota_refresh"` // null when the provider cannot refresh
	Setup           *setupGateway        `json:"setup"`         // null while setup is off, and at a provider without it
}

// backupView is the backup's last runs. The error text stays in the log: it comes from
// the storage service and is not the dashboard's to show.
type backupView struct {
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastFailure time.Time `json:"last_failure,omitzero"`
}

// dashboardAccount is one account with what this process knows of it.
type dashboardAccount struct {
	session.AccountView
	Email       string       `json:"email"`
	ExpectedIP  string       `json:"expected_egress_ip"`
	Egress      EgressResult `json:"egress_check,omitzero"`
	InFlight    int          `json:"in_flight"`
	Concurrency int          `json:"concurrency"`
	// Config is the account's row of the accounts table; absent for a provider without one.
	Config *accountConfig `json:"config,omitempty"`
	// Setup is the account's setup state, as in Config; absent while setup is off.
	Setup *setupBlock `json:"setup,omitempty"`
}

// accountConfig is one row of the accounts table as the dashboard shows it. The
// references are the strings themselves; no credential value is read to build it.
type accountConfig struct {
	Alias         string               `json:"alias"`
	Email         string               `json:"email"`
	ReceiverEmail string               `json:"receiver_email"`
	Access        string               `json:"access"`
	Status        string               `json:"status"`
	TokenRef      string               `json:"token_ref"`
	Proxy         proxyConfig          `json:"proxy"`
	DeviceID      string               `json:"device_id"`
	AccountUUID   string               `json:"account_uuid"`
	Verify        session.Verification `json:"verify"`
	Rev           int64                `json:"rev"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
	// Setup is the account's setup state; absent while setup is off.
	Setup *setupBlock `json:"setup,omitempty"`
}

// proxyConfig is the exit of an accountConfig.
type proxyConfig struct {
	Provider    string `json:"provider"`
	Type        string `json:"type"`
	Direct      bool   `json:"direct"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	ExpectedIP  string `json:"expected_egress_ip"`
	UsernameRef string `json:"username_ref"`
	PasswordRef string `json:"password_ref"`
}

// configOf returns row as the dashboard shows it.
func configOf(row session.AccountRow) accountConfig {
	return accountConfig{
		Alias: row.Alias, Email: row.Email, ReceiverEmail: row.ReceiverEmail, Access: row.Access, Status: row.Status, TokenRef: row.TokenRef,
		Proxy:    proxyConfig{Provider: row.ProxyProvider, Type: row.ProxyType, Direct: row.Direct, Host: row.ProxyHost, Port: row.ProxyPort, ExpectedIP: row.ExpectedIP, UsernameRef: row.UsernameRef, PasswordRef: row.PasswordRef},
		DeviceID: row.DeviceID, AccountUUID: row.AccountUUID, Verify: row.Verify, Rev: row.Rev, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// Handler returns the dashboard's routes, compressed with gzip for a request that
// accepts it.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard.json", func(w http.ResponseWriter, r *http.Request) {
		view, err := d.view(r)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("POST /quota/refresh", d.serveQuotaRefresh)
	d.writeRoutes(mux)
	if d.cfg.Dir != "" {
		mux.Handle("GET /{$}", page(d.cfg.Dir))
	}
	return compressed(mux)
}

// QuotaRefreshHeader is the header, with the value refresh, that a POST /quota/refresh
// must carry. A page of another origin cannot add it without a CORS preflight, which
// the dashboard does not answer.
const QuotaRefreshHeader = "X-Redcoast-Action"

// serveQuotaRefresh starts a refresh of every account in the background and answers at
// once: 202 {"started": true}, or {"started": false} when it joins the round pending or
// running; 429 with Retry-After while the wait after the last round lasts; 501 when the
// provider cannot refresh. The request must carry exactly one Origin, the page's own
// (HostGuard lets a request without one through), and QuotaRefreshHeader; otherwise it
// gets 403 and starts nothing.
func (d *Dashboard) serveQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r.Header["Origin"], r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request"})
		return
	}
	if values := r.Header.Values(QuotaRefreshHeader); len(values) != 1 || values[0] != "refresh" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": QuotaRefreshHeader + ": refresh is required"})
		return
	}
	if d.cfg.QuotaRefresh == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this gateway cannot refresh"})
		return
	}
	started, wait := d.cfg.QuotaRefresh.Request()
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "a refresh ran recently"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": started})
}

// RunQuotaRefresh runs the rounds POST /quota/refresh starts until ctx is canceled;
// without a QuotaRefresh it only waits for ctx.
func (d *Dashboard) RunQuotaRefresh(ctx context.Context) error {
	if d.cfg.QuotaRefresh == nil {
		<-ctx.Done()
		return nil
	}
	return d.cfg.QuotaRefresh.Run(ctx)
}

// view reads everything the dashboard shows.
func (d *Dashboard) view(r *http.Request) (dashboardView, error) {
	set := d.egress.accounts.set.Load()
	snap, err := d.latestSnapshot(r.Context(), set)
	if err != nil {
		return dashboardView{}, err
	}
	rows, _, err := d.cfg.Reader.Accounts(r.Context())
	if err != nil {
		return dashboardView{}, err
	}
	configs := make(map[string]accountConfig, len(rows))
	for _, row := range rows {
		configs[row.Alias] = configOf(row)
	}
	view := dashboardView{Snapshot: snap, Accounts: []dashboardAccount{}, Unserved: []accountConfig{}}
	statuses := make([]session.AccountStatus, len(snap.Accounts))
	for i, a := range snap.Accounts {
		statuses[i] = a.AccountStatus
		account := dashboardAccount{AccountView: a}
		if config, ok := configs[a.Alias]; ok {
			account.Config = &config
			account.Email = config.Email
			delete(configs, a.Alias)
		}
		if live := set.lookup(a.Alias); live != nil {
			account.Email, account.ExpectedIP = live.email, live.expectedIP.String()
			if account.Config != nil {
				// A reload keeps the object of an account whose served settings are the same, so
				// the email shown is the table's.
				account.Email = account.Config.Email
			}
			account.Egress = d.egress.lastCheck(live)
			account.InFlight, account.Concurrency = len(live.slots), cap(live.slots)
		}
		view.Accounts = append(view.Accounts, account)
	}
	for _, row := range rows {
		if config, ok := configs[row.Alias]; ok {
			view.Unserved = append(view.Unserved, config)
		}
	}
	view.ProxyRefOptions = proxyRefOptions(rows)
	setupView, err := d.setupView(r.Context(), &view)
	if err != nil {
		return dashboardView{}, err
	}
	view.AccountsEditable, view.Write = d.cfg.Write.view(r)
	view.Health = d.egress.judge(statuses, nil)
	view.Gateway = gatewayView{
		Provider:        d.cfg.Provider,
		Dashboards:      make([]DashboardLink, 0, len(d.cfg.Links)),
		Version:         d.cfg.Version,
		InventoryCommit: commitOf(d.cfg.Inventory),
		Started:         d.cfg.Started.UTC(),
		Retention:       d.cfg.Store.LastRetention(),
		Setup:           setupView,
	}
	for _, link := range d.cfg.Links {
		view.Gateway.Dashboards = append(view.Gateway.Dashboards, DashboardLink{Provider: link.Provider, URL: linkURL(link.URL, r)})
	}
	if info, err := os.Stat(d.cfg.DBPath + "-wal"); err == nil {
		view.Gateway.WALBytes = info.Size()
	}
	if d.cfg.Backup != nil {
		status := d.cfg.Backup()
		view.Gateway.Backup = &backupView{LastSuccess: status.LastSuccess, LastFailure: status.LastFailure}
	}
	if d.cfg.Tunnels != nil {
		view.Gateway.OpenTunnels, view.Gateway.TunnelLimit = d.cfg.Tunnels()
	}
	if d.cfg.QuotaRefresh != nil {
		state := d.cfg.QuotaRefresh.State()
		view.Gateway.QuotaRefresh = &state
	}
	return view, nil
}

// setupView puts each account's setup state into view and returns gateway.setup; both
// are absent while setup is off.
func (d *Dashboard) setupView(ctx context.Context, view *dashboardView) (*setupGateway, error) {
	setup := d.cfg.Write.Setup()
	if setup == nil {
		return nil, nil
	}
	states, err := d.cfg.Reader.SetupRows(ctx)
	if err != nil {
		return nil, err
	}
	var shown []*accountConfig
	for i := range view.Unserved {
		shown = append(shown, &view.Unserved[i])
	}
	for i := range view.Accounts {
		if c := view.Accounts[i].Config; c != nil {
			shown = append(shown, c)
		}
		view.Accounts[i].Setup = setup.block(view.Accounts[i].Alias, states[view.Accounts[i].Alias])
	}
	return setup.decorate(states, shown), nil
}

// linkURL returns link as it is, or for ":<port>" the root of r's scheme and Host
// name at that port. The Host has passed the dashboard's host check before r reaches
// here.
func linkURL(link string, r *http.Request) string {
	port, ok := strings.CutPrefix(link, ":")
	if !ok {
		return link
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(strings.Trim(host, "[]"), port), Path: "/"}).String()
}

// page serves dir/index.html for / and nothing else. The deploy account writes the
// directory, so the file is opened through os.Root: a symbolic link that leads out of
// dir, such as one to the session store the service user can read, is refused. A
// missing directory or page answers 404, which main logs at startup.
func page(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root, err := os.OpenRoot(dir)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = root.Close() }()
		file, err := root.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "index.html", info.ModTime(), file)
	})
}

// compressed compresses next's 200 answers with gzip for a request that accepts it. A
// Range header is dropped first, so the page is never answered with a part of the
// compressed body: such a request gets the whole page, still allowed by RFC 9110.
// Other statuses, 304 and 404 among them, pass as they are.
func compressed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header) {
			next.ServeHTTP(w, r)
			return
		}
		r = r.Clone(r.Context())
		r.Header.Del("Range")
		gw := &gzipWriter{ResponseWriter: w}
		next.ServeHTTP(gw, r)
		if gw.gz != nil {
			_ = gw.gz.Close()
		}
	})
}

// acceptsGzip reports whether Accept-Encoding gives gzip, or else *, a weight above 0.
func acceptsGzip(header http.Header) bool {
	gzipQ, anyQ := -1.0, -1.0
	for _, value := range header.Values("Accept-Encoding") {
		for entry := range strings.SplitSeq(value, ",") {
			coding, params, _ := strings.Cut(entry, ";")
			q := 1.0
			for param := range strings.SplitSeq(params, ";") {
				name, weight, ok := strings.Cut(param, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
					continue
				}
				// A weight that does not parse counts as 0.
				q, _ = strconv.ParseFloat(strings.TrimSpace(weight), 64)
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gzipQ = q
			case "*":
				anyQ = q
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return anyQ > 0
}

// gzipWriter compresses the body when the answer's status is 200.
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer // nil until a 200 answer starts
	wroteHeader bool
}

func (g *gzipWriter) WriteHeader(status int) {
	if !g.wroteHeader && status == http.StatusOK {
		header := g.Header()
		header.Del("Content-Length")
		header.Del("Accept-Ranges")
		header.Set("Content-Encoding", "gzip")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.wroteHeader = true
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

// commitOf returns the commit that an inventory link to inventories/<commit> names. It
// is empty when the link's target names no commit.
func commitOf(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	if name := filepath.Base(resolved); commitName.MatchString(name) {
		return name
	}
	return ""
}

// setupUnavailable is the answer of the setup route while setup is off, given before
// anything else is read.
func setupUnavailable(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, http.StatusNotImplemented, map[string]string{"error": errorSetupUnavailable})
}

// writeRoutes registers the account write routes. Without a write path they answer 501
// before anything else is read; with one, each runs the checks of AccountWriter.route.
func (d *Dashboard) writeRoutes(mux *http.ServeMux) {
	w := d.cfg.Write
	if w == nil {
		unsupported := func(rw http.ResponseWriter, _ *http.Request) {
			writeJSON(rw, http.StatusNotImplemented, map[string]string{"error": "this gateway does not edit accounts"})
		}
		mux.HandleFunc("POST /accounts", unsupported)
		mux.HandleFunc("PATCH /accounts/{alias}", unsupported)
		mux.HandleFunc("POST /accounts/{alias}/verify", unsupported)
		mux.HandleFunc("POST /accounts/{alias}/activate", unsupported)
		mux.HandleFunc("POST /accounts/{alias}/setup", setupUnavailable)
		mux.HandleFunc("GET /accounts/{alias}/record", unsupported)
		return
	}
	// The record is read where writes are configured, and by the same guards as
	// /dashboard.json, since it changes nothing.
	if len(w.cfg.AllowFrom) == 0 {
		mux.HandleFunc("GET /accounts/{alias}/record", func(rw http.ResponseWriter, _ *http.Request) {
			adminRefuse(rw, writeError{status: http.StatusNotImplemented, code: codeWriteUnavailable})
		})
	} else {
		mux.HandleFunc("GET /accounts/{alias}/record", func(rw http.ResponseWriter, r *http.Request) { serveRecord(rw, r, w) })
	}
	if w.setup == nil {
		mux.HandleFunc("POST /accounts/{alias}/setup", setupUnavailable)
	} else {
		mux.HandleFunc("POST /accounts/{alias}/setup", route(w, actionSetup, w.setupRequest))
	}
	mux.HandleFunc("POST /accounts", route(w, actionCreate, w.create))
	mux.HandleFunc("PATCH /accounts/{alias}", route(w, actionUpdate, w.update))
	mux.HandleFunc("POST /accounts/{alias}/verify", route(w, actionVerify, w.verify))
	mux.HandleFunc("POST /accounts/{alias}/activate", route(w, actionActivate, func(r *http.Request, alias string, body *revBody, entry session.AuditEntry) (any, error) {
		return w.activate(r, alias, body, entry, false)
	}))
}
