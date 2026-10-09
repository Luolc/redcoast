package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// requestState is what the layers of one request share through its context: the
// binding the router found and whether the gateway answered in place of the upstream.
type requestState struct {
	sessionID           string
	alias               string
	upstreamStatus      int   // the upstream's status when a response arrived; 0 otherwise
	upstreamUnavailable bool  // set by respondUpstreamUnavailable
	cut                 error // errIdleTimeout or errMaxDuration when the limits ended the request
}

// stateKey is the context key of the request's *requestState.
type stateKey struct{}

// withState returns ctx carrying a new request state, or ctx itself with the state it
// already carries.
func withState(ctx context.Context) (context.Context, *requestState) {
	if state, ok := ctx.Value(stateKey{}).(*requestState); ok {
		return ctx, state
	}
	state := &requestState{}
	return context.WithValue(ctx, stateKey{}, state), state
}

// accountKey is the context key under which the router stores the request's *account
// for Rewrite and the account transport.
type accountKey struct{}

// accountFrom returns the account the router bound the request to, or nil.
func accountFrom(ctx context.Context) *account {
	a, _ := ctx.Value(accountKey{}).(*account)
	return a
}

// accountTransport sends each outgoing request through the transport of the account
// the router bound it to.
type accountTransport struct{}

// RoundTrip dispatches to the bound account's transport.
func (accountTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	a := accountFrom(r.Context())
	if a == nil {
		return nil, errors.New("request has no account")
	}
	return a.transport.RoundTrip(r)
}

// The shapes of the Claude Code IDs the traffic table keeps: the session ID is the
// transcript's UUID, the agent ID is the lowercase hex name of the sub-agent's
// transcript. A header value of another shape is neither stored nor forwarded, so a
// credential placed in one of these headers goes nowhere.
var (
	claudeSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	claudeAgentID   = regexp.MustCompile(`^[0-9a-f]{8,32}$`)
)

// claudeIDHeaders maps the two headers to their accepted shapes.
var claudeIDHeaders = map[string]*regexp.Regexp{"X-Claude-Code-Session-Id": claudeSessionID, "X-Claude-Code-Agent-Id": claudeAgentID}

// indefinitePause is the pause an operator or the egress check writes without an end.
// A person clears it with Resume.
var indefinitePause = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// credentialPause is how long an account rests after the upstream refused its token.
// The pause ends on its own so that a refusal the gateway misread cannot take an
// account out for good; a token that is really revoked is refused again on the next
// request and the pause starts over.
const credentialPause = time.Hour

// allowedRequests are the only method and escaped path pairs the reverse proxy
// forwards; anything else is answered 403 without reaching the upstream. Claude Code's
// LLM gateway protocol names /v1/messages and the optional count_tokens as the
// endpoints behind ANTHROPIC_BASE_URL, plus a HEAD /api/hello at startup that may be
// refused; the gateway's own Claude Code runs sent nothing else to the base URL. The
// startup check carries no credential, so listing it only keeps its answer the 401 of
// a missing credential.
var allowedRequests = map[string]bool{
	"POST /v1/messages":              true,
	"POST /v1/messages/count_tokens": true,
	"HEAD /api/hello":                true,
}

// inferenceRequests are the allowed requests that use the account's token for a model
// request. Only their 401s can pause an account.
var inferenceRequests = map[string]bool{
	"POST /v1/messages":              true,
	"POST /v1/messages/count_tokens": true,
}

// oauthBeta is the anthropic-beta value that carries the OAuth capability. Without it
// the upstream answers a subscription token with 401, so a 401 to a request that lacks
// it says nothing about the token.
const oauthBeta = "oauth-2025-04-20"

// maxRequestLine bounds the method and path kept in the row of a refused request.
const maxRequestLine = 256

// requestLine is the method and escaped path of r, the key of allowedRequests.
func requestLine(r *http.Request) string {
	return r.Method + " " + r.URL.EscapedPath()
}

// router is the reverse-proxy entrypoint: it authenticates the session credential,
// has the session store evaluate the binding, forwards through the bound account and
// writes one traffic row per request.
type router struct {
	store    *session.Store
	accounts *Accounts
	upstream *url.URL
	port     int
	proxy    *httputil.ReverseProxy
	limits   Limits
	refusals refusalRows
	now      func() time.Time // the clock Retry-After and pauses are measured on; tests inject the store's
}

// newRouter returns the entrypoint in front of upstream. transport carries the
// outgoing requests; it must end in accountTransport.
func newRouter(store *session.Store, accounts *Accounts, upstream *url.URL, transport http.RoundTripper) *router {
	rt := &router{store: store, accounts: accounts, upstream: upstream, port: 443, limits: DefaultLimits, now: time.Now}
	if port := upstream.Port(); port != "" {
		// parseOrigin accepted the URL, so the port is numeric.
		rt.port, _ = strconv.Atoi(port)
	} else if upstream.Scheme == "http" {
		rt.port = 80
	}
	rt.proxy = newReverseProxy(upstream, transport, rt.inspectUpstream)
	return rt
}

// ServeHTTP authenticates and forwards one inference request, then records it.
func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, state := withState(r.Context())
	r = r.WithContext(ctx)
	start := time.Now()
	row := session.Traffic{At: start.UTC(), Kind: "inference", Host: rt.upstream.Hostname(), Port: rt.port,
		ClaudeSession: claudeID(r.Header, "X-Claude-Code-Session-Id"), ClaudeAgent: claudeID(r.Header, "X-Claude-Code-Agent-Id")}
	up := &countingReader{ReadCloser: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = up
	}
	down := &countingWriter{ResponseWriter: w}
	line := requestLine(r)
	defer func() {
		// ReverseProxy panics with http.ErrAbortHandler when the upstream body fails
		// after the response started. Record first, then re-panic so net/http still
		// aborts the client connection.
		aborted := recover()
		row.BytesUp, row.BytesDown, row.Status = up.count, down.count, state.upstreamStatus
		row.Duration = time.Since(start)
		if row.Result == "" {
			row.Result = outcome(state, r.Context().Err() != nil || aborted != nil)
		}
		if !refusedBeforeBinding[row.Result] || rt.refusals.allow(session.SourceOf(r.RemoteAddr), rt.now()) {
			rt.record(row)
		}
		if aborted != nil {
			panic(aborted)
		}
	}()
	if !allowedRequests[line] {
		row.Result, row.Request = "forbidden", truncate(line, maxRequestLine)
		http.Error(down, "redcoast: request not allowed", http.StatusForbidden)
		return
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		row.Result = "auth_failed"
		http.Error(down, "session credential required", http.StatusUnauthorized)
		return
	}
	binding, a, err := rt.accounts.bind(ctx, token, session.SourceOf(r.RemoteAddr))
	// A recognised session is recorded even when no account can serve it.
	state.sessionID, row.SessionID = binding.SessionID, binding.SessionID
	var exhausted *session.QuotaExhausted
	switch {
	case errors.Is(err, session.ErrUnknownSession):
		row.Result = "auth_failed"
		http.Error(down, "unknown session", http.StatusUnauthorized)
		return
	case errors.Is(err, session.ErrWrongSource):
		// The session exists and is not touched; only the address is wrong.
		row.Result = "wrong_source"
		http.Error(down, "session used from another address", http.StatusUnauthorized)
		return
	case errors.Is(err, session.ErrNoAccount):
		row.Result = "no_account"
		http.Error(down, "redcoast: no account available", http.StatusServiceUnavailable)
		return
	case errors.As(err, &exhausted):
		row.Result = "quota_exhausted"
		localRateLimit(down, exhausted.ResetAt, rt.now())
		return
	case errors.Is(err, errBindingQueue):
		row.Result = "limited"
		http.Error(down, "gateway busy", http.StatusServiceUnavailable)
		return
	case err != nil:
		row.Result = "store_failed"
		http.Error(down, "session store unavailable", http.StatusServiceUnavailable)
		return
	}
	state.alias, row.Alias = binding.Alias, binding.Alias
	if a == nil {
		row.Result = "no_account"
		http.Error(down, "bound account unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		row.Result = "limited"
		http.Error(down, "gateway busy", http.StatusServiceUnavailable)
		return
	}
	serveWithLimits(rt.proxy, rt.limits, state, up, down, r.WithContext(context.WithValue(ctx, accountKey{}, a)))
}

// localRateLimit answers a request no account can serve because every account is at or
// over the hard threshold. The body is in the API's error shape so that the client
// shows it, and the message names the gateway so that it cannot be mistaken for an
// upstream limit. Retry-After is the time to the earliest reset when one is known. No
// anthropic-ratelimit-unified-* headers are forged.
func localRateLimit(w http.ResponseWriter, resetAt, now time.Time) {
	message := "redcoast: local gateway quota exhausted, every account is over its hard threshold"
	if !resetAt.IsZero() {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(max(resetAt.Sub(now), time.Second)/time.Second), 10))
		message += ", earliest reset " + resetAt.UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "rate_limit_error", "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// outcome classifies a forwarded request: idle_timeout or max_duration when the limits
// ended it, upstream_failed when the gateway answered for an unreachable upstream,
// incomplete when the client left or the upstream body broke off, ok otherwise.
func outcome(state *requestState, interrupted bool) string {
	switch {
	case state.cut != nil:
		return state.cut.Error()
	case state.upstreamUnavailable:
		return "upstream_failed"
	case interrupted:
		return "incomplete"
	}
	return "ok"
}

// inspectUpstream runs on every upstream response before it is written back. It keeps
// the upstream's status for the traffic row, saves the account's quota readings from
// the response headers and, when the upstream refuses the account's token, pauses the
// account for credentialPause so that the session's next request binds elsewhere. The
// response passes through unchanged and is not replayed.
func (rt *router) inspectUpstream(res *http.Response) error {
	ctx := res.Request.Context()
	if state, ok := ctx.Value(stateKey{}).(*requestState); ok {
		state.upstreamStatus = res.StatusCode
	}
	a := accountFrom(ctx)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	source := "response"
	if res.StatusCode == http.StatusTooManyRequests {
		source = "429"
	}
	readings := quotaReadings(res.Header, source)
	if err := rt.store.RecordQuota(ctx, a.alias, readings); err != nil {
		log.Printf("quota record failed alias=%s", a.alias)
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		if !tokenRefused(res) {
			break
		}
		if err := rt.store.Pause(ctx, a.alias, rt.now().Add(credentialPause), "upstream_401"); err != nil {
			log.Printf("account pause failed alias=%s", a.alias)
		}
	case http.StatusTooManyRequests:
		// The upstream refused the account: the session's next request binds elsewhere
		// and this one is not replayed. The pause ends at the earliest reset the
		// response reports, or after a fixed interval when it reports none.
		if err := rt.store.Pause(ctx, a.alias, retryAfter(readings, rt.now()), session.QuotaPauseReason); err != nil {
			log.Printf("account pause failed alias=%s", a.alias)
		}
	}
	return nil
}

// maxErrorBody bounds the part of a 401 body read to classify it.
const maxErrorBody = 64 << 10

// tokenRefused reports whether a 401 means the upstream refused the account's token: the
// request was an inference request that carried the OAuth capability, and the body is
// the API's error shape with type authentication_error, which the API documents for a
// malformed, revoked or expired credential. A client cannot produce such a 401 by
// leaving out the capability or by calling another endpoint. The body is read up to
// maxErrorBody and put back in front of the rest, so the client still gets all of it.
func tokenRefused(res *http.Response) bool {
	out := res.Request
	if !inferenceRequests[requestLine(out)] || !hasOAuthBeta(out.Header) {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	res.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), res.Body), res.Body}
	if err != nil {
		return false
	}
	body := io.Reader(bytes.NewReader(head))
	switch res.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		if body, err = gzip.NewReader(body); err != nil {
			return false
		}
	default:
		return false
	}
	var parsed struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return json.NewDecoder(body).Decode(&parsed) == nil && parsed.Type == "error" && parsed.Error.Type == "authentication_error"
}

// hasOAuthBeta reports whether one of the comma-separated anthropic-beta values is oauthBeta.
func hasOAuthBeta(header http.Header) bool {
	for _, value := range header.Values("Anthropic-Beta") {
		for item := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(item) == oauthBeta {
				return true
			}
		}
	}
	return false
}

// truncate returns s cut to at most n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// rateLimitPause is how long an account rests after a 429 that reports no reset time.
const rateLimitPause = 5 * time.Minute

// retryAfter is the earliest reset among readings, or now plus rateLimitPause.
func retryAfter(readings []session.Reading, now time.Time) time.Time {
	until := time.Time{}
	for _, r := range readings {
		if !r.ResetAt.IsZero() && r.ResetAt.After(now) && (until.IsZero() || r.ResetAt.Before(until)) {
			until = r.ResetAt
		}
	}
	if until.IsZero() {
		return now.Add(rateLimitPause)
	}
	return until
}

// record writes one traffic row under its own deadline, so a canceled request is still
// recorded. A failure is logged; the response is not affected.
func (rt *router) record(row session.Traffic) {
	ctx, cancel := context.WithTimeout(context.Background(), captureWriteTimeout)
	defer cancel()
	if err := rt.store.RecordTraffic(ctx, row); err != nil {
		log.Print("traffic record failed")
	}
}

// claudeID returns the value of the named Claude Code ID header when it has the
// header's shape. Otherwise it removes the header, so the value is neither recorded
// nor forwarded, and returns "".
func claudeID(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) == 1 && claudeIDHeaders[name].MatchString(values[0]) {
		return values[0]
	}
	header.Del(name)
	return ""
}

// countingReader counts the bytes read from a request body and reports them to the
// request's watchdog once it has one.
type countingReader struct {
	io.ReadCloser
	count int64
	dog   *watchdog
}

// Read reads from the body and adds to the count.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.count += int64(n)
	if n > 0 && c.dog != nil {
		c.dog.touch()
	}
	return n, err
}

// countingWriter counts the body bytes written to the client and reports them to the
// request's watchdog once it has one.
type countingWriter struct {
	http.ResponseWriter
	count int64
	dog   *watchdog
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Write counts the bytes the wrapped writer accepted.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.count += int64(n)
	if n > 0 && c.dog != nil {
		c.dog.touch()
	}
	return n, err
}
