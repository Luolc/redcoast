package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultAccountConcurrency is the default number of inference requests one account
// serves at once, and the size of its connection pool; requests beyond it are refused
// instead of queued.
const DefaultAccountConcurrency = 32

// errUpgradeUnsupported is returned from ModifyResponse when the upstream switches
// protocols, which turns the response into a 502.
var errUpgradeUnsupported = errors.New("protocol upgrade unsupported")

// errGuardRefused wraps the error of a provider's Guard that kept a request from
// leaving.
var errGuardRefused = errors.New("guard refused the request")

// errLimitCut is the cause serveWithLimits cancels a request with when a limit ends it.
var errLimitCut = errors.New("a limit ended the request")

// exitRefused is the error of a CONNECT the account's exit answered with a status other
// than 200.
type exitRefused struct{ status int }

// Error names the exit's status.
func (e exitRefused) Error() string { return fmt.Sprintf("exit answered CONNECT with %d", e.status) }

// newTransport returns the outbound connection pool of concurrency connections. It ignores proxy environment
// variables, so authenticated mode's configureProxy is the only proxy source, and it
// disables compression so bodies and Accept-Encoding pass through unchanged.
func newTransport(concurrency int) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          concurrency,
		MaxIdleConnsPerHost:   concurrency,
		MaxConnsPerHost:       concurrency,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		// The transport's own error for a refused CONNECT is only the status text.
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
			if res.StatusCode != http.StatusOK {
				return exitRefused{res.StatusCode}
			}
			return nil
		},
	}
}

// declaredLength ends a request body at its declared length. After that many bytes
// the transport reads once more to confirm EOF, and net/http may have closed the
// request body by then, when the response header was written; reading it would fail
// and the transport would close the upstream connection in the middle of the response.
type declaredLength struct {
	io.ReadCloser
	remaining int64
}

// Read reads at most the bytes still declared, then returns io.EOF without reading
// the wrapped body.
func (b *declaredLength) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.remaining)])
	b.remaining -= int64(n)
	return n, err
}

// newReverseProxy configures the standard library reverse proxy that does the actual
// forwarding to target through transport, which dispatches by the request's account.
// The client's path loses prefix, which target's path stands for. inspect sees every
// upstream response, including one that is then rejected.
func newReverseProxy(target *url.URL, prefix string, transport http.RoundTripper, inspect func(*http.Response) error) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			if prefix != "" {
				// The allowlist matched the escaped path exactly, so it starts with prefix.
				p.Out.URL.Path = target.Path + strings.TrimPrefix(p.In.URL.Path, prefix)
				p.Out.URL.RawPath = ""
			}
			// Rewrite receives a cleaned query; retain the original query encoding.
			p.Out.URL.RawQuery = p.In.URL.RawQuery
			accountFrom(p.In.Context()).credential.Authorize(p.Out)
			// Transport may retry idempotent requests only when their body is replayable.
			// Every forwarded request gets a non-replayable body, including empty ones.
			if p.Out.Body == nil || p.Out.Body == http.NoBody {
				p.Out.Body = io.NopCloser(http.NoBody)
			} else if p.Out.ContentLength > 0 {
				p.Out.Body = &declaredLength{p.Out.Body, p.Out.ContentLength}
			}
			p.Out.GetBody = nil
		},
		Transport: transport,
		// Flush after every write so each streamed event reaches the client at once.
		FlushInterval: -1,
		// Its messages can include URLs and headers.
		ErrorLog:     log.New(io.Discard, "", 0),
		ErrorHandler: respondUpstreamUnavailable,
		ModifyResponse: func(res *http.Response) error {
			if err := inspect(res); err != nil {
				return err
			}
			return rejectUpgrade(res)
		},
	}
}

// guarded runs a provider's Guard on every request before it leaves; a request the
// guard refuses fails without reaching the upstream.
type guarded struct {
	next  http.RoundTripper
	guard func(*http.Request) error
}

// RoundTrip sends r when the guard lets it pass.
func (g guarded) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := g.guard(r); err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("%w: %w", errGuardRefused, err)
	}
	return g.next.RoundTrip(r)
}

// respondUpstreamUnavailable answers 502 when the upstream cannot be reached, times
// out, or is rejected by rejectUpgrade. It marks the request state so the records
// treat the response as generated by the gateway, keeps why in the state and logs it
// with the account, its exit and the time since the request was forwarded. err is not
// logged: its text can carry the URL.
func respondUpstreamUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	if state, ok := r.Context().Value(stateKey{}).(*requestState); ok {
		state.upstreamUnavailable = true
		alias, exit, proxied := "", "", false
		if a := accountFrom(r.Context()); a != nil {
			alias, exit, proxied = a.alias, a.exitName(), a.transport.Proxy != nil
		}
		state.failure = failureOf(r.Context(), err, &state.phases, proxied)
		log.Printf("upstream unavailable alias=%s exit=%s failure=%s elapsed_ms=%d", alias, exit, state.failure, time.Since(state.forwarded).Milliseconds())
	}
	http.Error(w, "upstream unavailable", http.StatusBadGateway)
}

// upstreamPhases is how far a forwarded request got, as the transport traces it: a TCP
// connection was made (to the exit, or to the upstream when there is no exit), the TLS
// handshake with the upstream began, a connection ready for the request was obtained.
// A dial can go on after the handler returned, so the fields are atomic.
type upstreamPhases struct {
	dialed, handshaking, connected atomic.Bool
}

// trace returns the hooks that record the phases.
func (p *upstreamPhases) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				p.dialed.Store(true)
			}
		},
		TLSHandshakeStart: func() { p.handshaking.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { p.connected.Store(true) },
	}
}

// http2StreamError receives an HTTP/2 stream reset: net/http's own type is internal,
// and errors.As converts it to any struct with the same fields, as net/http documents
// for golang.org/x/net/http2.StreamError.
type http2StreamError struct {
	StreamID uint32
	Code     uint32
	Cause    error
}

// Error names the stream.
func (e http2StreamError) Error() string { return fmt.Sprintf("http2 stream %d reset", e.StreamID) }

// failureOf names why a request that ctx carried got no upstream response, from the
// error the reverse proxy reported and how far the request got; proxied says whether
// the account's connections go through an exit. The names are fixed; only a refused
// CONNECT carries the exit's status in its name.
func failureOf(ctx context.Context, err error, phases *upstreamPhases, proxied bool) string {
	var refused exitRefused
	var op *net.OpError
	var timeout net.Error
	var stream http2StreamError
	switch {
	case errors.Is(err, errUpgradeUnsupported):
		return "upgrade"
	case errors.Is(err, errGuardRefused):
		return "guard"
	case ctx.Err() != nil:
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errLimitCut):
			return "limit"
		case errors.Is(cause, errGatewayStopped):
			return "shutdown"
		}
		return "client_canceled"
	case errors.As(err, &refused):
		return "egress_connect_" + strconv.Itoa(refused.status)
	case errors.As(err, &op) && op.Op == "proxyconnect":
		return "egress_dial"
	case !phases.connected.Load():
		switch {
		case phases.handshaking.Load():
			return "tls"
		case proxied && phases.dialed.Load():
			return "egress_unanswered"
		case errors.As(err, &op) && op.Op == "dial":
			return "dial"
		}
	case errors.As(err, &timeout) && timeout.Timeout():
		return "response_header_timeout"
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &stream):
		return "reset"
	}
	return "other"
}

// rejectUpgrade refuses protocol upgrades such as WebSocket. An upgraded connection is
// a raw byte stream that would bypass the deadlines and capture.
func rejectUpgrade(r *http.Response) error {
	if r.StatusCode == http.StatusSwitchingProtocols {
		return errUpgradeUnsupported
	}
	return nil
}

// serveWithLimits forwards r until it ends, has moved no byte either way (up is the
// request body, down the response) for limits.Idle, or has run for limits.MaxRequest;
// state.cut then says which limit ended it. The server's own read and write timeouts
// are moved to the total limit, or they would end a longer request. A cut cancels the
// upstream side and moves both deadlines to now, because a client that stops reading
// would otherwise keep the response write blocked, and one that stops sending its body
// the body read.
func serveWithLimits(proxy http.Handler, limits Limits, state *requestState, up *countingReader, down *countingWriter, r *http.Request) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	controller := http.NewResponseController(down)
	end := time.Now().Add(limits.MaxRequest)
	if controller.SetReadDeadline(end) != nil || controller.SetWriteDeadline(end) != nil {
		http.Error(down, "deadlines unavailable", http.StatusInternalServerError)
		return
	}
	dog := startWatchdog(limits.Idle, limits.MaxRequest, func() {
		cancel(errLimitCut)
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
	})
	up.dog, down.dog = dog, dog
	// The connection may serve another keep-alive request after this one. Deferred
	// calls run when ReverseProxy aborts the handler with a panic, too.
	defer func() {
		state.cut = dog.stop()
		if controller.SetReadDeadline(time.Time{}) != nil || controller.SetWriteDeadline(time.Time{}) != nil {
			log.Print("gateway deadline reset failed")
		}
	}()
	proxy.ServeHTTP(down, r.WithContext(ctx))
}
