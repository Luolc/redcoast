package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entrypoints are the two proxy addresses handed to every new session. The forward
// proxy speaks TLS exactly when the reverse proxy's origin is https.
type Entrypoints struct {
	ReverseProxy string `json:"reverse_proxy"` // origin for ANTHROPIC_BASE_URL
	ForwardProxy string `json:"forward_proxy"` // host:port for HTTPS_PROXY
}

// Server is the HTTP interface launchers use to issue and revoke sessions. Over the
// unix socket (ServeUnix) the caller is trusted by file permission; over TCP (ServeTCP)
// every request carries a registered machine's credential.
type Server struct {
	store       *Store
	entrypoints Entrypoints
	candidates  func() []string // the accounts the inventory allows, as Bind takes them
	issuing     func()          // called when an issue request starts; set only by tests
}

// NewServer returns the session interface over store. candidates lists the accounts a
// session could bind to; a session is refused when none of them is available.
func NewServer(store *Store, entrypoints Entrypoints, candidates func() []string) *Server {
	return &Server{store: store, entrypoints: entrypoints, candidates: candidates}
}

// Refusal is the body of a POST /sessions the gateway refuses, and the error the client
// returns for it. Code is quota_exhausted (every account is over its hard threshold or
// rate-limited; ResetAt is the earliest recovery when known), no_account (an account is
// out for another reason, such as a refused token or a missing plan), machine_limit
// (the machine is at its session limit) or unknown_machine (the machine credential
// matches no registered machine). Message starts with "redcoast:" so that it is
// not mistaken for an upstream error.
type Refusal struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	ResetAt time.Time `json:"reset_at,omitzero"`
}

func (r *Refusal) Error() string { return r.Message }

// refusal translates an issue failure into the response to send: the status and the
// body, or nil when the failure is not one of the business refusals.
func refusal(err error) (int, *Refusal) {
	var exhausted *QuotaExhausted
	var limit *MachineLimit
	switch {
	case errors.As(err, &limit):
		return http.StatusTooManyRequests, &Refusal{Code: "machine_limit", Message: "redcoast: " + limit.Error()}
	case errors.Is(err, ErrUnknownMachine):
		return http.StatusUnauthorized, &Refusal{Code: "unknown_machine", Message: "redcoast: the machine credential matches no registered machine"}
	case errors.As(err, &exhausted):
		r := &Refusal{Code: "quota_exhausted", Message: "redcoast: local gateway quota exhausted, every account is over its hard threshold or rate-limited", ResetAt: exhausted.ResetAt}
		if !exhausted.ResetAt.IsZero() {
			r.Message += ", earliest recovery " + exhausted.ResetAt.UTC().Format(time.RFC3339)
		}
		return http.StatusTooManyRequests, r
	case errors.Is(err, ErrNoAccount):
		return http.StatusServiceUnavailable, &Refusal{Code: "no_account", Message: "redcoast: no account available for a new session (refused token, no plan, or no account configured); not a quota limit"}
	}
	return 0, nil
}

// issueRequest is the body of POST /sessions.
type issueRequest struct {
	ClientMachine string          `json:"client_machine"`
	LaunchMeta    json.RawMessage `json:"launch_meta,omitempty"`
}

// Grant is the body of a successful POST /sessions: the session and its credential,
// which appears here only, plus the entrypoints the client must use.
type Grant struct {
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	Entrypoints
}

// maxRequestBody bounds the body of POST /sessions.
const maxRequestBody = 8192

// Handler returns the routes: POST /sessions issues a session; DELETE /sessions/current
// revokes the session whose credential is in the Authorization header.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", srv.issue)
	mux.HandleFunc("DELETE /sessions/current", srv.revoke)
	return mux
}

// sourceOf returns the source a session issued to r is bound to: LocalSource for a
// unix socket or loopback peer, the peer's IP otherwise.
func sourceOf(r *http.Request) string {
	return SourceOf(r.RemoteAddr)
}

// SourceOf classifies a peer address the way sessions are bound: LocalSource for a
// unix socket peer or a loopback IP, the IP otherwise. An address that is not an IP
// and port is treated as local only when it is a unix peer's ("" or "@").
func SourceOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return LocalSource
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() {
		return LocalSource
	}
	return ip.String()
}

// issue answers POST /sessions. Over TCP the request carries a machine credential and
// the session belongs to that machine; over the unix socket the body names the machine.
func (srv *Server) issue(w http.ResponseWriter, r *http.Request) {
	if srv.issuing != nil {
		srv.issuing()
	}
	var request issueRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	credential, networked := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := decoder.Decode(&request); err != nil || (!networked && request.ClientMachine == "") {
		http.Error(w, "body must be a JSON object with client_machine", http.StatusBadRequest)
		return
	}
	// Over the network the machine is authenticated first, so that an unknown
	// credential learns nothing about the accounts' state. Then refuse before issuing
	// when no account could serve the session, by Bind's rules. The check and the
	// first Bind are separate transactions: an account can become unavailable in
	// between, and the first request then gets the same refusal.
	var err error
	if networked {
		_, err = srv.store.AuthenticateMachine(r.Context(), credential)
	}
	if err == nil {
		err = srv.store.Available(r.Context(), srv.candidates())
	}
	var issued Issued
	if err == nil {
		if networked {
			issued, err = srv.store.IssueForMachine(r.Context(), credential, sourceOf(r), request.LaunchMeta)
		} else {
			issued, err = srv.store.Issue(r.Context(), request.ClientMachine, sourceOf(r), request.LaunchMeta)
		}
	}
	if status, body := refusal(err); body != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	switch {
	case errors.Is(err, ErrLaunchMeta):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "session store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(Grant{SessionID: issued.ID, Token: issued.Token, Entrypoints: srv.entrypoints}); err != nil {
		// The client did not receive the credential; the session stays issued but
		// unused and the idle expiry ends it.
		return
	}
}

// revoke answers DELETE /sessions/current.
func (srv *Server) revoke(w http.ResponseWriter, r *http.Request) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		http.Error(w, "session credential required", http.StatusUnauthorized)
		return
	}
	err := srv.store.RevokeFrom(r.Context(), token, sourceOf(r), "client")
	switch {
	case errors.Is(err, ErrUnknownSession):
		http.Error(w, "unknown session", http.StatusUnauthorized)
	case errors.Is(err, ErrWrongSource):
		http.Error(w, "session used from another address", http.StatusUnauthorized)
	case err != nil:
		http.Error(w, "session store unavailable", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// shutdownGrace is how long ServeUnix waits for requests in progress after ctx is
// canceled before it closes their connections.
const shutdownGrace = 5 * time.Second

// ServeUnix serves the interface on the unix socket at path; see ServeUnixSocket.
func (srv *Server) ServeUnix(ctx context.Context, path string) error {
	return ServeUnixSocket(ctx, path, srv.Handler())
}

// ServeTCP serves the interface on the TCP address, one specific IP and port, until
// ctx is canceled. Every request must carry a machine credential in Authorization;
// one that does not is refused before the handler, so the unix socket's trust by file
// permission never applies here. The listener is the only difference from ServeUnix.
func (srv *Server) ServeTCP(ctx context.Context, address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on session address: %v", err)
	}
	return srv.ServeTCPListener(ctx, listener)
}

// ServeTCPListener is ServeTCP on a listener the caller opened.
func (srv *Server) ServeTCPListener(ctx context.Context, listener net.Listener) error {
	handler := srv.Handler()
	return serveListener(ctx, listener, "session address", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sessions" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(Refusal{Code: "unknown_machine", Message: "redcoast: a machine credential is required to ask for a session over the network"})
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

// ServeUnixSocket serves handler on the unix socket at path, readable and writable by
// the owner only, until ctx is canceled. Every request's context is derived from ctx,
// so handlers in progress stop with the server; connections still open after the
// grace period are closed. Nothing is served after it returns. The socket file is
// removed on return. A file already at path makes it fail rather than replace it.
func ServeUnixSocket(ctx context.Context, path string, handler http.Handler) error {
	listener, err := ListenUnixSocket(path)
	if err != nil {
		return err
	}
	return ServeUnixListener(ctx, listener, handler)
}

// ListenUnixSocket listens on the unix socket at path, readable and writable by the
// owner only. A file already at path makes it fail rather than replace it.
func ListenUnixSocket(path string) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on socket %s: %v", filepath.Base(path), err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict socket %s: %v", filepath.Base(path), err)
	}
	return listener, nil
}

// ServeUnixListener is ServeUnixSocket on a listener the caller opened; whether the
// socket file is removed on return is up to the listener.
func ServeUnixListener(ctx context.Context, listener net.Listener, handler http.Handler) error {
	return serveListener(ctx, listener, "socket "+filepath.Base(listener.Addr().String()), handler)
}

// serveListener serves handler on listener until ctx is canceled, then shuts down as
// ServeUnixSocket describes. name labels the listener in errors.
func serveListener(ctx context.Context, listener net.Listener, name string, handler http.Handler) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var err error
	select {
	case err := <-done:
		return fmt.Errorf("%s: %v", name, err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	err = server.Shutdown(shutdownCtx)
	// Shutdown stops listening and waits for idle connections; a connection still
	// mid-request after the grace period is closed here so that no request completes
	// after the server is gone.
	if closeErr := server.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	<-done
	if err != nil {
		return fmt.Errorf("stop %s: %v", name, err)
	}
	return nil
}
