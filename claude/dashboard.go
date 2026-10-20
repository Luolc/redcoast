package claude

import (
	"compress/gzip"
	"context"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
}

// NewDashboard returns the dashboard over the egress checks' accounts.
func NewDashboard(egress *Egress, cfg DashboardConfig) *Dashboard {
	return &Dashboard{egress: egress, cfg: cfg}
}

// dashboardView is the JSON answer. Its Accounts hides the embedded snapshot's, which
// it extends with this process's state.
type dashboardView struct {
	Health   health             `json:"health"`
	Gateway  gatewayView        `json:"gateway"`
	Accounts []dashboardAccount `json:"accounts"`
	session.Snapshot
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
	snap, err := d.cfg.Reader.Snapshot(r.Context(), set.Candidates())
	if err != nil {
		return dashboardView{}, err
	}
	view := dashboardView{Snapshot: snap, Accounts: []dashboardAccount{}}
	statuses := make([]session.AccountStatus, len(snap.Accounts))
	for i, a := range snap.Accounts {
		statuses[i] = a.AccountStatus
		account := dashboardAccount{AccountView: a}
		if live := set.lookup(a.Alias); live != nil {
			account.Email, account.ExpectedIP = live.email, live.expectedIP.String()
			account.Egress = d.egress.lastCheck(live)
			account.InFlight, account.Concurrency = len(live.slots), cap(live.slots)
		}
		view.Accounts = append(view.Accounts, account)
	}
	view.Health = d.egress.judge(statuses, nil)
	view.Gateway = gatewayView{
		Provider:        d.cfg.Provider,
		Dashboards:      make([]DashboardLink, 0, len(d.cfg.Links)),
		Version:         d.cfg.Version,
		InventoryCommit: commitOf(d.cfg.Inventory),
		Started:         d.cfg.Started.UTC(),
		Retention:       d.cfg.Store.LastRetention(),
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
