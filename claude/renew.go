package claude

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// ErrRenewalRefused is wrapped by a Provider's Renew when the account's credential can
// no longer be renewed: only a new import brings the account back.
var ErrRenewalRefused = errors.New("renewal refused")

// NeedsImportReason is the pause reason of an account whose credential renewal was
// refused; the pause lasts until a renewal succeeds after a new import.
const NeedsImportReason = "needs_reimport"

// renewTimeout bounds one renewal.
const renewTimeout = 30 * time.Second

// maxRenewBackoff bounds the wait before a queued renewal that failed is tried again;
// the wait starts at a minute and doubles.
const maxRenewBackoff = time.Hour

// renewals is the queue of forced renewals: one entry per account, so a renewal queued
// or failing for an account is not queued again.
type renewals struct {
	mu      sync.Mutex
	pending map[string]renewal
	wake    chan struct{}
}

// renewal is a queued renewal: when the upstream refused the credential, when the
// renewal runs next and how many times it failed.
type renewal struct {
	since    time.Time
	at       time.Time
	failures int
}

// queue queues a renewal of alias, whose credential the upstream refused at since, to
// run at once, unless one is queued already.
func (q *renewals) queue(alias string, since time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.init()
	if _, queued := q.pending[alias]; queued {
		return
	}
	q.pending[alias] = renewal{since: since, at: since}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// due returns the renewals due at now, by alias, and the earliest time another one is
// (zero when none is queued).
func (q *renewals) due(now time.Time) (due map[string]renewal, next time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	due = map[string]renewal{}
	for alias, r := range q.pending {
		switch {
		case !r.at.After(now):
			due[alias] = r
		case next.IsZero() || r.at.Before(next):
			next = r.at
		}
	}
	return due, next
}

// queued reports whether a renewal of alias is queued.
func (q *renewals) queued(alias string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.pending[alias]
	return ok
}

// retry queues alias's renewal again after one more failure, or drops it when done.
func (q *renewals) retry(alias string, now time.Time, done bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if done {
		delete(q.pending, alias)
		return
	}
	r := q.pending[alias]
	r.failures++
	r.at = now.Add(min(time.Minute<<min(r.failures-1, 10), maxRenewBackoff))
	q.pending[alias] = r
}

// wakeup returns the channel that receives when a renewal is queued.
func (q *renewals) wakeup() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.init()
	return q.wake
}

// init makes the zero queue ready; q.mu is held.
func (q *renewals) init() {
	if q.pending == nil {
		q.pending, q.wake = map[string]renewal{}, make(chan struct{}, 1)
	}
}

// RunRenewals renews the accounts' credentials until ctx is canceled, when the
// provider renews them: every account whose credential is due, at once and then every
// interval, and each account queued by an upstream 401 or a failed import renewal,
// retried after a failure until it succeeds or is refused. A refused renewal pauses the
// account for NeedsImportReason; a queued one that succeeds ends a pause for that
// reason or for upstream_401. A renewal leaves the queue before its outcome changes
// the pause, so a 401 that pauses the account after that is queued again.
func (e *Entrypoint) RunRenewals(ctx context.Context, interval time.Duration) error {
	rt := e.router
	if rt.provider.Renew == nil {
		<-ctx.Done()
		return nil
	}
	sweep := rt.now()
	for {
		now := rt.now()
		due, next := rt.renewals.due(now)
		for alias, r := range due {
			err := rt.renew(ctx, alias, r.since)
			rt.renewals.retry(alias, rt.now(), err == nil || errors.Is(err, ErrRenewalRefused) || errors.Is(err, errNoSuchAccount))
			rt.settle(ctx, alias, err, true)
		}
		if !now.Before(sweep) {
			for _, alias := range rt.accounts.Candidates() {
				if !rt.renewals.queued(alias) {
					rt.settle(ctx, alias, rt.renew(ctx, alias, time.Time{}), false)
				}
			}
			sweep = rt.now().Add(interval)
		}
		wait := sweep.Sub(rt.now())
		if !next.IsZero() {
			wait = min(wait, next.Sub(rt.now()))
		}
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-rt.renewals.wakeup():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// renew renews alias's credential through its exit: when the upstream refused it at
// refusedAt, unless a renewal settled since; with a zero refusedAt, when it is due.
// Failures are logged by alias.
func (rt *router) renew(ctx context.Context, alias string, refusedAt time.Time) error {
	a := rt.accounts.set.Load().lookup(alias)
	if a == nil {
		return errNoSuchAccount
	}
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	client := &http.Client{
		Transport: a.transport,
		// A credential goes only where the provider sends it, never where a redirect
		// points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	err := rt.provider.Renew(ctx, alias, client, refusedAt)
	switch {
	case errors.Is(err, ErrRenewalRefused):
		log.Printf("credential renewal refused alias=%s: %v", alias, err)
	case err != nil && !errors.Is(err, errNoSuchAccount):
		log.Printf("credential renewal failed alias=%s: %v", alias, err)
	}
	return err
}

// settle updates alias's pause by the outcome of a renewal: a refused one pauses it for
// NeedsImportReason, and a queued one that succeeded ends a pause for that reason or
// for upstream_401.
func (rt *router) settle(ctx context.Context, alias string, err error, queued bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	switch {
	case errors.Is(err, ErrRenewalRefused):
		if err := rt.store.Pause(ctx, alias, indefinitePause, NeedsImportReason); err != nil {
			log.Printf("account pause failed alias=%s", alias)
		}
	case err == nil && queued:
		if _, err := rt.store.EndPause(ctx, alias, "upstream_401", NeedsImportReason); err != nil {
			log.Printf("account resume failed alias=%s", alias)
		}
	}
}

// errNoImport refuses an import at a gateway whose provider has none.
var errNoImport = errors.New("this gateway cannot import logins")

// Import replaces account alias's login with login, then renews it at once: the
// renewal makes the gateway the only holder of the login and, when it succeeds, ends
// the account's pause for NeedsImportReason or upstream_401. When the renewal fails
// otherwise than refused, the login stays imported and the renewal is queued.
func (e *Entrypoint) Import(ctx context.Context, alias string, login []byte) error {
	rt := e.router
	if rt.provider.Import == nil || rt.provider.Renew == nil {
		return errNoImport
	}
	if rt.accounts.set.Load().lookup(alias) == nil {
		return errNoSuchAccount
	}
	if err := rt.provider.Import(alias, login); err != nil {
		return ImportError{err}
	}
	err := rt.renew(ctx, alias, rt.now())
	if err != nil && !errors.Is(err, ErrRenewalRefused) {
		rt.renewals.queue(alias, rt.now())
	}
	rt.settle(ctx, alias, err, true)
	return err
}

// ImportError is a login the provider would not import.
type ImportError struct{ err error }

// Error returns the provider's reason.
func (e ImportError) Error() string { return e.err.Error() }

// Unwrap returns the provider's error.
func (e ImportError) Unwrap() error { return e.err }
