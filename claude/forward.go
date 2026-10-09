package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luolc/redcoast/session"
)

// connectTarget is the only CONNECT authority accepted: a host name or IPv4 literal
// and a port. It keeps request lines and records free of anything else.
var connectTarget = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}:[0-9]{1,5}$`)

// connectRecord is the saved capture record of one forward-proxy request. Result is
// one of ok, refused (not a CONNECT, an invalid target, or an account-route target
// accountTarget rules out), auth_failed, wrong_source, no_account,
// quota_exhausted, store_failed, limited, egress_failed (the exit could not be reached, answered
// nothing valid, or refused the tunnel; ExitStatus tells which; on the direct route,
// the target could not be reached), idle_timeout or
// max_duration (the limits ended an open tunnel).
type connectRecord struct {
	Kind       string    `json:"kind"`
	ID         string    `json:"request_id"`
	Account    string    `json:"account_alias,omitempty"`
	Method     string    `json:"method"`
	Target     string    `json:"target"`
	Route      string    `json:"route,omitempty"` // direct or account, once the target is valid
	Started    time.Time `json:"started_utc"`
	Duration   int64     `json:"duration_ns"`
	Result     string    `json:"result"`
	ExitStatus int       `json:"exit_status,omitempty"`
	BytesUp    int64     `json:"bytes_up"`
	BytesDown  int64     `json:"bytes_down"`
}

// forwardProxy is the side-traffic entrypoint. It authenticates the session credential
// in Proxy-Authorization, has the session store evaluate the binding and tunnels every
// CONNECT through the bound account's exit with that account's proxy credentials,
// except a target listed in the inventory's direct-hosts.yaml, which it connects to
// from the gateway's own address. Neither route reaches a non-public address. A failed
// connection on either route is not retried on the other. A tunnel stays on the
// account it was opened with. Other methods are refused.
type forwardProxy struct {
	store    *session.Store
	accounts *Accounts
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	resolve  func(ctx context.Context, network, host string) ([]netip.Addr, error) // a direct target's addresses
	sink     func(connectRecord) error                                             // nil records nothing beyond the traffic table
	slots    chan struct{}
	limits   Limits
	refusals refusalRows
	prefix   string
	sequence atomic.Uint64
	active   sync.WaitGroup // handlers, including hijacked tunnels the server no longer tracks
}

// newForwardProxy returns the forward proxy over store and accounts. sink receives one
// capture record per request.
func newForwardProxy(store *session.Store, accounts *Accounts, sink func(connectRecord) error) *forwardProxy {
	return &forwardProxy{
		store:    store,
		accounts: accounts,
		dial:     (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		resolve:  net.DefaultResolver.LookupNetIP,
		sink:     sink,
		slots:    make(chan struct{}, DefaultLimits.Tunnels),
		limits:   DefaultLimits,
		prefix:   rand.Text(),
	}
}

// setLimits replaces the default limits; it runs before the proxy serves.
func (f *forwardProxy) setLimits(limits Limits) {
	f.limits, f.slots = limits, make(chan struct{}, limits.Tunnels)
}

// ServeHTTP answers one forward-proxy request and records it when the request ends:
// a capture record through the sink and a traffic row.
func (f *forwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.active.Add(1)
	defer f.active.Done()
	record := connectRecord{Kind: "connect", ID: fmt.Sprintf("%s-%d", f.prefix, f.sequence.Add(1)), Method: safeMethod(r.Method), Started: time.Now().UTC()}
	row := session.Traffic{At: record.Started, Kind: "connect"}
	defer func() {
		record.Duration = time.Since(record.Started).Nanoseconds()
		if f.sink != nil && f.sink(record) != nil {
			log.Printf("capture persistence failed request_id=%s", record.ID)
		}
		row.Duration, row.Result, row.Status, row.BytesUp, row.BytesDown = time.Duration(record.Duration), record.Result, record.ExitStatus, record.BytesUp, record.BytesDown
		f.recordTraffic(row, session.SourceOf(r.RemoteAddr))
	}()
	if r.Method != http.MethodConnect || !connectTarget.MatchString(r.Host) {
		record.Result = "refused"
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT is forwarded", http.StatusMethodNotAllowed)
		} else {
			http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		}
		return
	}
	record.Target = r.Host
	row.Host, row.Port = splitTarget(r.Host)
	row.Route = "account"
	if f.accounts.set.Load().direct(r.Host) {
		row.Route = "direct"
	}
	record.Route = row.Route
	if row.Route == "account" && !accountTarget(r.Host) {
		record.Result = "refused"
		http.Error(w, "CONNECT target not allowed", http.StatusForbidden)
		return
	}
	a, binding, result := f.authenticate(r)
	if result != "" {
		record.Result, row.SessionID = result, binding.SessionID
		switch result {
		case "auth_failed":
			w.Header().Set("Proxy-Authenticate", `Basic realm="gateway session"`)
			http.Error(w, "session credential required", http.StatusProxyAuthRequired)
		case "wrong_source":
			w.Header().Set("Proxy-Authenticate", `Basic realm="gateway session"`)
			http.Error(w, "session used from another address", http.StatusProxyAuthRequired)
		case "no_account":
			http.Error(w, "redcoast: no account available", http.StatusServiceUnavailable)
		case "quota_exhausted":
			http.Error(w, "redcoast: local gateway quota exhausted", http.StatusTooManyRequests)
		default:
			http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	record.Account, row.SessionID, row.Alias = a.alias, binding.SessionID, binding.Alias
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		record.Result = "limited"
		http.Error(w, "forward proxy busy", http.StatusServiceUnavailable)
		return
	}
	exit, reader := f.open(w, r, a, &record)
	if exit == nil {
		record.Result = "egress_failed"
		return
	}
	defer closeQuietly(exit)
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		record.Result = "egress_failed"
		http.Error(w, "tunnel unavailable", http.StatusInternalServerError)
		return
	}
	defer closeQuietly(client)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		record.Result = "egress_failed"
		return
	}
	var cut error
	record.BytesUp, record.BytesDown, cut = splice(r.Context(), f.limits, client, buffered.Reader, exit, reader)
	record.Result = "ok"
	if cut != nil {
		record.Result = cut.Error()
	}
}

// recordTraffic writes row under its own deadline, unless it is a refusal from source
// over the per-address limit. A failure is logged; the response is not affected.
func (f *forwardProxy) recordTraffic(row session.Traffic, source string) {
	if refusedBeforeBinding[row.Result] && !f.refusals.allow(source, time.Now()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), captureWriteTimeout)
	defer cancel()
	if err := f.store.RecordTraffic(ctx, row); err != nil {
		log.Print("traffic record failed")
	}
}

// open connects to the far side of r's tunnel on record.Route: the target itself on
// the direct route, a CONNECT through a's exit on the account route. It returns the
// connection and the reader positioned at the tunnel's first byte, or nil after
// answering w with 502.
func (f *forwardProxy) open(w http.ResponseWriter, r *http.Request, a *account, record *connectRecord) (net.Conn, io.Reader) {
	if record.Route == "direct" {
		conn, err := f.dialDirect(r.Context(), r.Host)
		if err != nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return nil, nil
		}
		return conn, conn
	}
	conn, err := f.dial(r.Context(), "tcp", a.exit.Host)
	if err != nil {
		http.Error(w, "exit unavailable", http.StatusBadGateway)
		return nil, nil
	}
	reader, status := openTunnel(conn, r.Host, a.exitAuthorization())
	record.ExitStatus = status
	if status != http.StatusOK {
		closeQuietly(conn)
		http.Error(w, "exit refused the tunnel", http.StatusBadGateway)
		return nil, nil
	}
	return conn, reader
}

// authenticate reads the session credential from Proxy-Authorization and evaluates the
// binding. It returns the bound account and binding, or the result to record:
// auth_failed, no_account, quota_exhausted or store_failed. With no_account and
// quota_exhausted the binding still carries the recognised session's ID.
func (f *forwardProxy) authenticate(r *http.Request) (*account, session.Binding, string) {
	token, ok := proxyCredential(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return nil, session.Binding{}, "auth_failed"
	}
	binding, a, err := f.accounts.bind(r.Context(), token, session.SourceOf(r.RemoteAddr))
	var exhausted *session.QuotaExhausted
	switch {
	case errors.Is(err, session.ErrUnknownSession):
		return nil, session.Binding{}, "auth_failed"
	case errors.Is(err, session.ErrWrongSource):
		return nil, session.Binding{}, "wrong_source"
	case errors.Is(err, session.ErrNoAccount):
		return nil, binding, "no_account"
	case errors.As(err, &exhausted):
		// Side traffic spends no quota, but it follows the inference binding, and
		// there is none to follow.
		return nil, binding, "quota_exhausted"
	case errors.Is(err, errBindingQueue):
		return nil, session.Binding{}, "limited"
	case err != nil:
		return nil, session.Binding{}, "store_failed"
	}
	if a == nil {
		return nil, binding, "no_account"
	}
	return a, binding, ""
}

// proxyCredential extracts the session credential from a Proxy-Authorization value.
// HTTPS_PROXY's userinfo arrives as Basic user:password; the credential is the
// password, or the whole userinfo when it has no user part.
func proxyCredential(value string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Basic") {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", false
	}
	userinfo := string(decoded)
	if _, password, found := strings.Cut(userinfo, ":"); found {
		return password, password != ""
	}
	return userinfo, userinfo != ""
}

// splitTarget splits a validated CONNECT authority into host and port.
func splitTarget(target string) (string, int) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return target, 0
	}
	// connectTarget accepted the authority, so the port is numeric.
	number, _ := strconv.Atoi(port)
	return host, number
}

// openTunnel asks the exit to CONNECT to target with authorization and returns the
// reader positioned after the exit's response head, with the exit's status, or 0 when
// no valid response arrived within ten seconds.
func openTunnel(exit net.Conn, target, authorization string) (*bufio.Reader, int) {
	if err := exit.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, 0
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: " + authorization + "\r\n\r\n"
	if _, err := io.WriteString(exit, request); err != nil {
		return nil, 0
	}
	reader := bufio.NewReader(exit)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, 0
	}
	// A CONNECT response has no body; the next bytes belong to the tunnel.
	if err := exit.SetDeadline(time.Time{}); err != nil {
		return nil, 0
	}
	return reader, response.StatusCode
}

// splice copies bytes both ways until either side ends, ctx ends, no byte has moved
// either way for limits.Idle or limits.MaxTunnel has passed, then returns the bytes
// sent up (client to exit) and down, and errIdleTimeout or errMaxDuration when a limit
// ended the tunnel. The readers carry bytes each side's buffer already holds.
func splice(ctx context.Context, limits Limits, client net.Conn, fromClient io.Reader, exit net.Conn, fromExit io.Reader) (int64, int64, error) {
	// The server's deadlines may still be on the hijacked connection; the watchdog
	// bounds the tunnel instead.
	if client.SetDeadline(time.Time{}) != nil || exit.SetDeadline(time.Time{}) != nil {
		return 0, 0, nil
	}
	// Closing both connections unblocks both copies.
	closeBoth := func() {
		closeQuietly(client)
		closeQuietly(exit)
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	dog := startWatchdog(limits.Idle, limits.MaxTunnel, closeBoth)
	var up, down int64
	var wait sync.WaitGroup
	wait.Go(func() {
		up, _ = io.Copy(touchWriter{exit, dog}, fromClient)
		closeQuietly(exit)
	})
	down, _ = io.Copy(touchWriter{client, dog}, fromExit)
	closeQuietly(client)
	wait.Wait()
	return up, down, dog.stop()
}

// wait blocks until every request, including hijacked tunnels, has ended or ctx ends.
func (f *forwardProxy) wait(ctx context.Context) error {
	finished := make(chan struct{})
	go func() {
		f.active.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("forward tunnels still open")
	}
}

// safeMethod keeps the standard method names and hides anything else a client sent.
func safeMethod(method string) string {
	switch method {
	case http.MethodConnect, http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return method
	}
	return hidden
}

// closeQuietly closes c; a second close or a peer that already left is not an error
// worth reporting here.
func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}
