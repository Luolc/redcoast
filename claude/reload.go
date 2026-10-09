package claude

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Luolc/redcoast/session"
)

// reloadWait bounds how long a reload waits for the binding sections in progress and
// for the migration transaction. Past it the reload is abandoned and the old set stays.
const reloadWait = 10 * time.Second

// EgressCheck verifies an account's exit before the account joins the set on reload:
// step 3 of the plan's reload. The gateway's check is Egress.Check. An account the
// check refuses, for a different exit IP or because the check could not read one, is
// left out of the new set and reported in Reload.Rejected; the reload itself goes on.
// A nil check accepts every account.
type EgressCheck func(ctx context.Context, a *account) error

// Reload is the report of one reload: how the new set differs from the old one and
// how many sessions the migration moved or left without a binding.
type Reload struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Changed  []string `json:"changed"`  // same alias, new token or exit; served by a new outbound object
	Rejected []string `json:"rejected"` // refused by the egress check; not in the new set
	Moved    int      `json:"moved"`
	Unplaced int      `json:"unplaced"`
	Direct   []string `json:"direct_hosts"` // the new set's direct-hosts.yaml, in effect for new tunnels
}

// errBindingQueue is returned by bind when the request's context ends while its binding
// section queues behind a reload; the entrypoints answer it as a busy gateway.
var errBindingQueue = errors.New("binding section queued behind a reload until the request ended")

// Accounts holds the account set the entrypoints serve and replaces it on reload. The
// binding section of a request (the candidates, the store's Bind and the outbound
// object) runs under the region's shared lock and sees one version of the set; a reload
// holds the exclusive lock from the migration of the sessions to the swap of the set,
// so no binding section straddles them. Requests already forwarding, and open tunnels,
// hold their account object and finish on it. Reloads run one at a time, so each one
// diffs against the set it will replace.
type Accounts struct {
	region    *region
	reloading chan struct{} // a one-slot semaphore: the reload in progress holds it
	set       atomic.Pointer[AccountSet]
	store     *session.Store
	migrate   func(ctx context.Context, from, candidates []string, reason string) (session.Migration, error)
	swapped   []func(*AccountSet) // called with the new set after a swap, inside the exclusive lock

	// Test hooks: beforeBind runs between taking the candidates and Bind, so a test
	// can hold a request there while a reload runs; beforePause does the same for the
	// egress sweep's pause; noExclusion drops the locks, the
	// control arm that shows the error the exclusive region prevents.
	beforeBind  func()
	beforePause func() // runs in the egress sweep's shared lock, between its comparison and its pause
	noExclusion bool
}

// NewAccounts returns the holder of set, migrating sessions through store on reload.
func NewAccounts(set *AccountSet, store *session.Store) *Accounts {
	c := &Accounts{region: newRegion(), reloading: make(chan struct{}, 1), store: store, migrate: store.MigrateBindings}
	c.set.Store(set)
	return c
}

// Candidates returns the current set's aliases, sorted.
func (c *Accounts) Candidates() []string {
	return c.set.Load().Candidates()
}

// onSwap registers f to run with every new set; f is called once now with the current one.
func (c *Accounts) onSwap(f func(*AccountSet)) {
	c.swapped = append(c.swapped, f)
	f(c.set.Load())
}

// CloseIdleConnections closes every current account's idle connections.
func (c *Accounts) CloseIdleConnections() {
	c.set.Load().CloseIdleConnections()
}

// bind is a request's binding section: the candidates, the store's Bind and the bound
// account's outbound object, all from one version of the set. The set is read again
// after Bind on purpose: under the shared lock both reads see the same version, and
// the control arm without the lock shows the mix of versions the lock prevents. The
// returned account is nil when err is set or the bound alias is not in the set.
func (c *Accounts) bind(ctx context.Context, token, source string) (session.Binding, *account, error) {
	if !c.noExclusion {
		if err := c.region.rlock(ctx); err != nil {
			return session.Binding{}, nil, errBindingQueue
		}
		defer c.region.runlock()
	}
	candidates := c.set.Load().Candidates()
	if c.beforeBind != nil {
		c.beforeBind()
	}
	binding, err := c.store.BindFrom(ctx, token, source, candidates)
	if err != nil {
		return binding, nil, err
	}
	return binding, c.set.Load().lookup(binding.Alias), nil
}

// Reload loads the inventory in dir with resolve and, when everything loaded, replaces
// the set by the plan's steps: the egress check on new and changed accounts, the diff
// against the old set, the migration of the sessions bound to removed and changed
// accounts in one transaction, then the swap, the last three under the exclusive lock.
// Reloads run one at a time; a second one waits for the first, or returns when its
// context ends. Any failure leaves the old set and the old bindings in place; the
// error says so.
func (c *Accounts) Reload(ctx context.Context, dir string, resolve Resolver, verify EgressCheck) (Reload, error) {
	select {
	case c.reloading <- struct{}{}:
		defer func() { <-c.reloading }()
	case <-ctx.Done():
		return Reload{}, fmt.Errorf("reload: another reload in progress, this one abandoned: %v", ctx.Err())
	}
	next, err := LoadAccounts(ctx, dir, resolve, c.set.Load().concurrency)
	if err != nil {
		return Reload{}, fmt.Errorf("reload: inventory not loaded, accounts unchanged: %w", err)
	}
	old := c.set.Load()
	r := diff(ctx, old, next, verify)
	// Unchanged accounts keep their outbound object, so their connection pools stay.
	for _, alias := range next.Candidates() {
		if !slices.Contains(r.Added, alias) && !slices.Contains(r.Changed, alias) {
			next.byAlias[alias] = old.byAlias[alias]
		}
	}
	from := slices.Concat(r.Removed, r.Changed)
	candidates := slices.DeleteFunc(next.Candidates(), func(alias string) bool { return slices.Contains(r.Changed, alias) })
	if err := c.lock(ctx); err != nil {
		return r, err
	}
	defer c.unlock()
	// Cancellation is honoured up to here and not after: once the migration has
	// committed, the set is published whatever ctx says, or the bindings would point
	// at accounts the entrypoints do not serve.
	if err := ctx.Err(); err != nil {
		return r, fmt.Errorf("reload: abandoned before the migration, accounts unchanged: %v", err)
	}
	migrateCtx, cancel := context.WithTimeout(ctx, reloadWait)
	defer cancel()
	m, err := c.migrate(migrateCtx, from, candidates, "reload")
	if err != nil {
		return r, fmt.Errorf("reload: session migration failed, accounts and bindings unchanged: %v", err)
	}
	r.Moved, r.Unplaced, r.Direct = m.Moved, m.Unplaced, next.DirectHosts()
	c.set.Store(next)
	for _, f := range c.swapped {
		f(next)
	}
	// The old outbound objects of removed and changed accounts serve only the requests
	// still on them; their idle connections close now and the rest when they go idle.
	for _, alias := range from {
		old.byAlias[alias].transport.CloseIdleConnections()
	}
	log.Printf("reload: added=%v removed=%v changed=%v rejected=%v moved=%d unplaced=%d direct_hosts=%d", r.Added, r.Removed, r.Changed, r.Rejected, r.Moved, r.Unplaced, len(r.Direct))
	return r, nil
}

// diff classifies next against old, runs verify on the new and changed accounts and
// removes the refused ones from next. The lists are sorted.
func diff(ctx context.Context, old, next *AccountSet, verify EgressCheck) Reload {
	var r Reload
	for _, alias := range next.Candidates() {
		a, before := next.byAlias[alias], old.byAlias[alias]
		if before != nil && before.sameConfig(a) {
			continue
		}
		if verify != nil {
			if err := verify(ctx, a); err != nil {
				log.Printf("reload: egress check refused alias=%s: %v", alias, err)
				delete(next.byAlias, alias)
				r.Rejected = append(r.Rejected, alias)
				continue
			}
		}
		if before == nil {
			r.Added = append(r.Added, alias)
		} else {
			r.Changed = append(r.Changed, alias)
		}
	}
	for _, alias := range old.Candidates() {
		if next.byAlias[alias] == nil {
			r.Removed = append(r.Removed, alias)
		}
	}
	return r
}

// sameConfig reports whether b has a's token, exit, expected exit IP and proxy credentials.
func (a *account) sameConfig(b *account) bool {
	return a.token == b.token && a.exit.String() == b.exit.String() && a.expectedIP == b.expectedIP && a.proxyUsername == b.proxyUsername && a.proxyPassword == b.proxyPassword
}

// lock takes the exclusive lock, waiting at most reloadWait for the binding sections
// in progress. New binding sections queue while it waits; when it gives up, on the
// timeout or on ctx, nothing stays pending and they go on.
func (c *Accounts) lock(ctx context.Context) error {
	if c.noExclusion {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, reloadWait)
	defer cancel()
	if err := c.region.lock(waitCtx); err != nil {
		return errors.New("reload: binding sections in progress did not finish in time, accounts unchanged")
	}
	return nil
}

// unlock releases the exclusive lock.
func (c *Accounts) unlock() {
	if !c.noExclusion {
		c.region.unlock()
	}
}
