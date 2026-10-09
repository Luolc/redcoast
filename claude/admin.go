package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// Admin is the management interface the operator reaches over the management unix
// socket: reload, status, pause and resume, machines and the session list. Only the service user can open the socket;
// there is no authentication beyond that.
type Admin struct {
	accounts *Accounts
	store    *session.Store
	reload   func(context.Context) (Reload, error) // reloads from the configured inventory and resolver
	// Handoff starts the new process on the current binary and returns once it is
	// ready, or why it is not; nil refuses every handoff.
	Handoff func() error
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

// serveStatus answers with the store's status for the current accounts.
func (a *Admin) serveStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.store.Status(r.Context(), a.accounts.Candidates())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, status)
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
