package claude

import (
	"context"
	"sync"
)

// region is the exclusive region of reload: a readers-writer lock whose waits follow a
// context. A binding section holds it shared; a reload holds it exclusively. A pending
// writer makes new readers queue, as the plan asks, but a writer that gives up (its
// context ends) leaves nothing behind that would keep queuing them, and a queued reader
// leaves when its own context ends. One writer at a time: reloads are serialized
// before they get here.
type region struct {
	mu            sync.Mutex
	readers       int
	writer        bool
	writerWaiting bool
	changed       chan struct{} // closed and replaced on every state change
}

// newRegion returns an unlocked region.
func newRegion() *region {
	return &region{changed: make(chan struct{})}
}

// broadcast wakes every waiter; the caller holds mu.
func (r *region) broadcast() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// rlock takes the shared lock, or returns ctx's error when ctx ends first.
func (r *region) rlock(ctx context.Context) error {
	for {
		r.mu.Lock()
		if !r.writer && !r.writerWaiting {
			r.readers++
			r.mu.Unlock()
			return nil
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runlock releases the shared lock.
func (r *region) runlock() {
	r.mu.Lock()
	r.readers--
	r.broadcast()
	r.mu.Unlock()
}

// lock takes the exclusive lock, or returns ctx's error when ctx ends first, a ctx
// already over included; on that path no writer stays pending.
func (r *region) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.writerWaiting = true
	r.broadcast()
	for r.writer || r.readers > 0 {
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			r.mu.Lock()
			r.writerWaiting = false
			r.broadcast()
			r.mu.Unlock()
			return ctx.Err()
		}
		r.mu.Lock()
	}
	// A wake-up and the end of ctx can arrive together; the lock is not granted to a
	// caller whose ctx is over.
	if err := ctx.Err(); err != nil {
		r.writerWaiting = false
		r.broadcast()
		r.mu.Unlock()
		return err
	}
	r.writer, r.writerWaiting = true, false
	r.mu.Unlock()
	return nil
}

// unlock releases the exclusive lock.
func (r *region) unlock() {
	r.mu.Lock()
	r.writer = false
	r.broadcast()
	r.mu.Unlock()
}
