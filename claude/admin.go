package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// Admin is the management interface the operator reaches over the management unix
// socket: reload, status, pause, resume, quota refresh, login import, machines and the session list. Only the service user can open the socket;
// there is no authentication beyond that.
type Admin struct {
	accounts *Accounts
	store    *session.Store
	reload   func(context.Context) (Reload, error) // reloads from the configured inventory and resolver
	// Handoff starts the new process on the current binary and returns once it is
	// ready, or why it is not; nil refuses every handoff.
	Handoff func() error
	// Refresh reads an account's quota again from the upstream; nil refuses every refresh.
	Refresh func(ctx context.Context, alias string) (Refreshed, error)
	// Import replaces an account's login and renews it; nil refuses every import.
	Import func(ctx context.Context, alias string, login []byte) error
}

// NewAdmin returns the management interface over accounts and store; reload runs one
// reload with the gateway's configuration.
func NewAdmin(accounts *Accounts, store *session.Store, reload func(context.Context) (Reload, error)) *Admin {
	return &Admin{accounts: accounts, store: store, reload: reload}
}

// pauseRequest is the body of POST /accounts/{alias}/pause. A missing until pauses
// the account until a person resumes it.
type pauseRequest struct {
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason"`
}

// maxAdminBody bounds the body of a management request.
const maxAdminBody = 4096

// Handler returns the routes. Every answer is JSON; a failure is {"error": "..."}.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reload", a.serveReload)
	mux.HandleFunc("POST /handoff", a.serveHandoff)
	mux.HandleFunc("GET /status", a.serveStatus)
	mux.HandleFunc("POST /accounts/{alias}/pause", a.servePause)
	mux.HandleFunc("POST /accounts/{alias}/resume", a.serveResume)
	mux.HandleFunc("POST /accounts/{alias}/refresh", a.serveRefresh)
	mux.HandleFunc("POST /refresh", a.serveRefreshAll)
	mux.HandleFunc("POST /accounts/{alias}/import", a.serveImport)
	mux.HandleFunc("GET /sessions", a.serveSessions)
	mux.HandleFunc("GET /machines", a.serveMachines)
	mux.HandleFunc("POST /machines", a.serveAddMachine)
	mux.HandleFunc("DELETE /machines/{name}", a.serveRevokeMachine)
	mux.HandleFunc("POST /machines/{name}/limit", a.serveMachineLimit)
	return mux
}

// machineRequest is the body of POST /machines: the name, the SHA-256 of the
// credential the client machine generated and keeps, and the session limit (0 for the
// default). The credential itself never reaches this interface.
type machineRequest struct {
	Name           string `json:"name"`
	CredentialHash string `json:"credential_hash"`
	MaxSessions    int    `json:"max_sessions"`
}

// defaultSessionWindow is how far back GET /sessions looks when since is omitted.
const defaultSessionWindow = 24 * time.Hour

// serveSessions lists the sessions started in [since, until) (RFC 3339; until defaults
// to now, since to a day before until), narrowed by cwd_prefix and machine when given,
// and only the ended ones unless state is "all".
func (a *Admin) serveSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := session.Query{Until: time.Now(), CwdPrefix: query.Get("cwd_prefix"), Machine: query.Get("machine")}
	for _, bound := range []struct {
		name string
		at   *time.Time
	}{{"until", &q.Until}, {"since", &q.Since}} {
		if value := query.Get(bound.name); value != "" {
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": bound.name + " must be an RFC 3339 time"})
				return
			}
			*bound.at = at
		}
	}
	if q.Since.IsZero() {
		q.Since = q.Until.Add(-defaultSessionWindow)
	}
	switch query.Get("state") {
	case "", "ended":
		q.EndedOnly = true
	case "all":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state must be ended or all"})
		return
	}
	if !q.Since.Before(q.Until) || q.CwdPrefix != "" && !strings.HasPrefix(q.CwdPrefix, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be before until and cwd_prefix an absolute path"})
		return
	}
	list, err := a.store.Sessions(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"since": q.Since.UTC(), "until": q.Until.UTC(), "ended_only": q.EndedOnly, "limit": session.MaxRecords, "sessions": list.Sessions, "truncated": list.Truncated})
}

// serveMachines lists the registered machines.
func (a *Admin) serveMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := a.store.Machines(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, machines)
}

// serveAddMachine registers a machine.
func (a *Admin) serveAddMachine(w http.ResponseWriter, r *http.Request) {
	var request machineRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a JSON object with name, credential_hash and max_sessions"})
		return
	}
	err := a.store.AddMachine(r.Context(), request.Name, request.CredentialHash, request.MaxSessions)
	switch {
	case errors.Is(err, session.ErrMachineShape):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, session.ErrMachineExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"name": request.Name})
	}
}

// serveRevokeMachine revokes a machine and ends its sessions.
func (a *Admin) serveRevokeMachine(w http.ResponseWriter, r *http.Request) {
	ended, err := a.store.RevokeMachine(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, session.ErrNoSuchMachine):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "sessions_ended": ended})
	}
}

// serveMachineLimit changes a machine's session limit.
func (a *Admin) serveMachineLimit(w http.ResponseWriter, r *http.Request) {
	var request struct {
		MaxSessions int `json:"max_sessions"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a JSON object with max_sessions"})
		return
	}
	err := a.store.SetMachineLimit(r.Context(), r.PathValue("name"), request.MaxSessions)
	switch {
	case errors.Is(err, session.ErrNoSuchMachine):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil && strings.Contains(err.Error(), "at least"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "max_sessions": request.MaxSessions})
	}
}

// serveReload runs one reload and answers with its report, or 422 with the error: the
// inventory, a reference or the migration refused it and nothing changed.
func (a *Admin) serveReload(w http.ResponseWriter, r *http.Request) {
	report, err := a.reload(r.Context())
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// serveHandoff hands the listeners to a new process. On success this process stops
// accepting after the answer and drains; on failure it keeps serving as before.
func (a *Admin) serveHandoff(w http.ResponseWriter, _ *http.Request) {
	if a.Handoff == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this gateway cannot hand off"})
		return
	}
	if err := a.Handoff(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"handoff": "ready"})
}

// serveStatus answers with the store's status for the current accounts, and the ones
// the egress check set aside with its reason, as the health endpoint names them.
func (a *Admin) serveStatus(w http.ResponseWriter, r *http.Request) {
	set := a.accounts.set.Load()
	status, err := a.store.Status(r.Context(), set.Candidates())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	var setAside []string
	for _, account := range set.sorted() {
		if reason := account.setAside.Load(); reason != nil {
			setAside = append(setAside, account.alias+": "+*reason)
		}
	}
	writeJSON(w, http.StatusOK, struct {
		session.Status
		SetAside []string `json:"set_aside,omitempty"`
	}{status, setAside})
}

// servePause pauses an account of the current set; sessions bound to it rebind on
// their next request.
func (a *Admin) servePause(w http.ResponseWriter, r *http.Request) {
	alias, ok := a.knownAlias(w, r)
	if !ok {
		return
	}
	var request pauseRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a JSON object with reason and an optional until"})
		return
	}
	until := request.Until
	if until.IsZero() {
		until = indefinitePause
	}
	if err := a.store.Pause(r.Context(), alias, until, request.Reason); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alias": alias, "paused_until": until.UTC(), "reason": request.Reason})
}

// serveResume clears an account's pause.
func (a *Admin) serveResume(w http.ResponseWriter, r *http.Request) {
	alias, ok := a.knownAlias(w, r)
	if !ok {
		return
	}
	if err := a.store.Resume(r.Context(), alias); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"alias": alias})
}

// serveRefresh reads an account's quota again and answers with the outcome; 409 when
// the account is paused for a reason other than quota, 502 when the upstream was not
// reached.
func (a *Admin) serveRefresh(w http.ResponseWriter, r *http.Request) {
	alias, ok := a.knownAlias(w, r)
	if !ok {
		return
	}
	if a.Refresh == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this gateway cannot refresh"})
		return
	}
	refreshed, err := a.Refresh(r.Context(), alias)
	var paused pausedError
	switch {
	case errors.As(err, &paused):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, errNoSuchAccount):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, refreshed)
	}
}

// serveRefreshAll refreshes every account of the current set, one after another, and
// answers with one result per account in alias order: the outcome of its refresh, or
// {"alias", "skipped"} for an account paused for a reason other than quota (nothing is
// sent, as with the 409 of a single refresh), or {"alias", "error"}. A failed account
// does not stop the ones after it.
func (a *Admin) serveRefreshAll(w http.ResponseWriter, r *http.Request) {
	if a.Refresh == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this gateway cannot refresh"})
		return
	}
	results := []any{}
	for _, alias := range a.accounts.Candidates() {
		refreshed, err := a.Refresh(r.Context(), alias)
		var paused pausedError
		switch {
		case errors.As(err, &paused):
			results = append(results, map[string]string{"alias": alias, "skipped": err.Error()})
		case err != nil:
			results = append(results, map[string]string{"alias": alias, "error": err.Error()})
		default:
			results = append(results, refreshed)
		}
	}
	writeJSON(w, http.StatusOK, results)
}

// maxImportBody bounds the body of an import, a login of a few kilobytes.
const maxImportBody = 64 << 10

// serveImport replaces an account's login with the request body and renews it: 400
// when the provider refuses the login, 409 when the renewal is refused (the account
// stays paused for a new import), 502 when the renewal failed otherwise (the login is
// kept and the renewal retried).
func (a *Admin) serveImport(w http.ResponseWriter, r *http.Request) {
	alias, ok := a.knownAlias(w, r)
	if !ok {
		return
	}
	if a.Import == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": errNoImport.Error()})
		return
	}
	login, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the login must be at most 64 KiB"})
		return
	}
	err = a.Import(r.Context(), alias, login)
	var refused ImportError
	switch {
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrRenewalRefused):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "imported, but the renewal was refused: " + err.Error()})
	case errors.Is(err, errNoSuchAccount):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "imported, but the renewal failed and is retried: " + err.Error()})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"alias": alias, "imported": "renewed"})
	}
}

// knownAlias reads the alias path value and checks it is in the current set.
func (a *Admin) knownAlias(w http.ResponseWriter, r *http.Request) (string, bool) {
	alias := r.PathValue("alias")
	if !accountAlias.MatchString(alias) || a.accounts.set.Load().lookup(alias) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such account in the current set"})
		return "", false
	}
	return alias, true
}

// writeJSON writes body as JSON with status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
