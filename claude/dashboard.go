package claude

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Luolc/redcoast/backup"
	"github.com/Luolc/redcoast/session"
)

// commitName matches a full commit hash, the name of a release or inventory directory.
var commitName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DashboardConfig is what the dashboard reads besides the accounts and the egress
// checks. Every field is read, none is written.
type DashboardConfig struct {
	Reader    *session.Reader // the store over a read-only connection
	Store     *session.Store  // only for its last retention run, kept in memory
	DBPath    string          // the store's file, whose -wal file is measured
	Binary    string          // the running executable's path
	Inventory string          // the --inventory link
	Started   time.Time       // when the gateway started
	Tunnels   func() (open, limit int)
	Backup    func() backup.Status // nil when no backups are configured
	Dir       string               // holds the dashboard page, index.html, served at /; empty for none
}

// Dashboard is the read-only view of the gateway: GET /dashboard.json, and the
// page at / when a directory is configured. It reads the
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
	BinaryCommit    string               `json:"binary_commit"`    // empty when the executable is not under a release directory
	InventoryCommit string               `json:"inventory_commit"` // the link's target now; empty when it names no commit
	Started         time.Time            `json:"started"`
	WALBytes        int64                `json:"wal_bytes"`
	Retention       session.RetentionRun `json:"retention"`
	Backup          *backupView          `json:"backup"` // null when no backups are configured
	OpenTunnels     int                  `json:"open_tunnels"`
	TunnelLimit     int                  `json:"tunnel_limit"`
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

// Handler returns the dashboard's routes.
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
	if d.cfg.Dir != "" {
		mux.Handle("GET /{$}", page(d.cfg.Dir))
	}
	return mux
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
		BinaryCommit:    commitOf(d.cfg.Binary, true),
		InventoryCommit: commitOf(d.cfg.Inventory, false),
		Started:         d.cfg.Started.UTC(),
		Retention:       d.cfg.Store.LastRetention(),
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
	return view, nil
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

// commitOf returns the commit that path's resolved location names: its directory's
// name for an executable under releases/<commit>/, its own name for an inventory link
// to inventories/<commit>. It is empty when the location names no commit.
func commitOf(path string, inDirectory bool) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	if inDirectory {
		resolved = filepath.Dir(resolved)
	}
	if name := filepath.Base(resolved); commitName.MatchString(name) {
		return name
	}
	return ""
}
