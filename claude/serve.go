package claude

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/Luolc/redcoast/session"
)

// Entrypoint is the gateway's two entrypoints and, when capture is on, its record
// store. NewHandler builds it; main serves Reverse and Forward and calls Finish.
type Entrypoint struct {
	Reverse http.Handler // the inference entrypoint
	Forward http.Handler // the side-traffic entrypoint
	forward *forwardProxy
	records *RecordStore // nil without capture
}

// Tunnels returns the forward-proxy tunnels open now and their limit.
func (e *Entrypoint) Tunnels() (open, limit int) {
	return len(e.forward.slots), cap(e.forward.slots)
}

// Finish runs after the servers have stopped. It waits for forward-proxy tunnels,
// which the servers no longer track, and then for record writes, so that the records
// of requests open at shutdown are saved. Both waits share one deadline: after a
// handoff, the end of the drain plus the write timeout; otherwise ctx is already
// canceled and they get the write timeout alone.
func (e *Entrypoint) Finish(ctx context.Context) error {
	drainCtx, cancel := afterGrace(requestsOf(ctx), captureWriteTimeout)
	defer cancel()
	if err := e.forward.wait(drainCtx); err != nil {
		return err
	}
	if e.records == nil {
		return nil
	}
	if err := e.records.drain(drainCtx); err != nil {
		return errors.New("capture writes did not finish")
	}
	return nil
}

// NewHandler builds the two entrypoints in front of upstream, bounded by limits. With a capture directory
// every request's record is saved there; the directory must already exist with mode
// 0700 so records are private to this user, and the accounts' credentials, those of
// every set a reload brings in included, are removed from every record.
func NewHandler(upstream *url.URL, captureDir string, store *session.Store, accounts *Accounts, limits Limits) (*Entrypoint, error) {
	if captureDir == "" {
		router, forward := newRouter(store, accounts, upstream, accountTransport{}), newForwardProxy(store, accounts, nil)
		router.limits = limits
		forward.setLimits(limits)
		return &Entrypoint{Reverse: router, Forward: forward, forward: forward}, nil
	}
	info, err := os.Stat(captureDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("capture directory must exist with mode 0700")
	}
	records := &RecordStore{dir: captureDir}
	accounts.onSwap(records.learn)
	router := newRouter(store, accounts, upstream, captureTransport{accountTransport{}})
	forward := newForwardProxy(store, accounts, records.writeConnect)
	router.limits = limits
	forward.setLimits(limits)
	return &Entrypoint{Reverse: newCapturedProxy(router, records.write), Forward: forward, forward: forward, records: records}, nil
}

// Run serves the entrypoints with servers until ctx is canceled or one of them fails,
// then finishes: tunnels and record writes still in progress complete before it
// returns, on the failure path as well as on shutdown. The servers' errors and the
// finishing error are joined.
func (e *Entrypoint) Run(ctx context.Context, servers ...func(context.Context) error) error {
	err := RunServers(ctx, servers...)
	return errors.Join(err, e.Finish(ctx))
}

// RunServers runs every server until ctx is canceled or one of them fails. A failure
// stops the others too; the errors are joined.
func RunServers(ctx context.Context, servers ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(servers))
	for _, server := range servers {
		go func() {
			err := server(ctx)
			cancel()
			done <- err
		}()
	}
	var errs []error
	for range servers {
		errs = append(errs, <-done)
	}
	return errors.Join(errs...)
}

// Handoff is the cause ctx is canceled with when a new process has taken over the
// listeners. Serve then stops accepting but lets the requests in progress, inference
// streams and forward-proxy tunnels alike, run until Requests ends.
type Handoff struct{ Requests context.Context }

func (Handoff) Error() string { return "handed off to a new process" }

// requestsOf returns the context that bounds requests in progress once ctx is
// canceled: the drain's after a handoff, ctx itself otherwise.
func requestsOf(ctx context.Context) context.Context {
	var handoff Handoff
	if errors.As(context.Cause(ctx), &handoff) {
		return handoff.Requests
	}
	return ctx
}

// afterGrace returns a context that ends grace after ctx does.
func afterGrace(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	extended, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	return extended, func() {
		stop()
		cancel()
	}
}

// Serve runs an HTTP server on listener until ctx is canceled, then stops accepting.
// Requests in progress are canceled at once, or after a Handoff when its Requests
// ends; those still running five seconds later are cut off with Close.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	// Hijacked tunnels outlive the server, so requests is cut when ctx ends (or its
	// handoff drain does), not when Serve returns.
	requests, cut := context.WithCancel(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, func() { context.AfterFunc(requestsOf(ctx), cut) })
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return requests },
	}
	// Shutdown closes a connection whose request it reads after Shutdown began, even
	// one accepted before. So Serve first stops accepting and waits for the accepted
	// connections to close (keep-alives off, each closes after its next response) for
	// as long as requests runs, then shuts down.
	var conns sync.WaitGroup
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			conns.Add(1)
		case http.StateHijacked, http.StateClosed:
			conns.Done()
		}
	}
	// Serve blocks until the server stops, so it runs in its own goroutine.
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("gateway listener failed")
		}
		return nil
	case <-ctx.Done():
	}
	// Serve returns the error of the Accept the Close interrupts.
	_ = listener.Close()
	<-done
	server.SetKeepAlivesEnabled(false)
	closed := make(chan struct{})
	go func() {
		conns.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-requests.Done():
	}
	shutdownCtx, cancel := afterGrace(requests, 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		if err := server.Close(); err != nil {
			log.Print("gateway close failed")
		}
	}
	return nil
}
