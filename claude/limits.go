package claude

import (
	"errors"
	"io"
	"log"
	"sync"
	"time"
)

// Limits bounds how long an inference request or a forward-proxy tunnel may run, and
// how many tunnels may be open at once. A request or tunnel ends when no byte has moved
// either way for Idle, and in any case after its total limit.
type Limits struct {
	Idle       time.Duration // matches the CLI, which gives up after five minutes without a byte
	MaxRequest time.Duration // one inference request, however busy
	MaxTunnel  time.Duration // one CONNECT tunnel, however busy
	Tunnels    int           // open tunnels across all accounts; more are refused
}

// DefaultLimits are the production limits.
var DefaultLimits = Limits{Idle: 5 * time.Minute, MaxRequest: time.Hour, MaxTunnel: 6 * time.Hour, Tunnels: 256}

// The causes a watchdog ends a request or tunnel with; their text is the traffic row's
// result.
var (
	errIdleTimeout = errors.New("idle_timeout")
	errMaxDuration = errors.New("max_duration")
)

// watchdog ends one request or tunnel when it has been idle for idle or has run for its
// total limit, by calling cut once. Each byte moved either way is reported with touch.
type watchdog struct {
	mu        sync.Mutex
	idle      time.Duration
	last      time.Time
	idleTimer *time.Timer
	maxTimer  *time.Timer
	cut       func()
	cause     error
}

// startWatchdog starts watching; stop ends it.
func startWatchdog(idle, total time.Duration, cut func()) *watchdog {
	w := &watchdog{idle: idle, last: time.Now(), cut: cut}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.idleTimer = time.AfterFunc(idle, w.checkIdle)
	w.maxTimer = time.AfterFunc(total, func() { w.fire(errMaxDuration) })
	return w
}

// touch records that a byte moved.
func (w *watchdog) touch() {
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
}

// checkIdle cuts when the last byte is idle ago, and otherwise checks again when it
// would be.
func (w *watchdog) checkIdle() {
	w.mu.Lock()
	quiet := time.Since(w.last)
	if quiet < w.idle {
		w.idleTimer.Reset(w.idle - quiet)
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.fire(errIdleTimeout)
}

// fire cuts with cause unless a cut already happened or the watchdog stopped. The cut
// runs under the lock, so once stop returns no cut is running or still to come.
func (w *watchdog) fire(cause error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cause != nil || w.cut == nil {
		return
	}
	w.cause = cause
	w.cut()
}

// stop ends the watch and returns the cause of the cut, or nil when there was none.
func (w *watchdog) stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.idleTimer.Stop()
	w.maxTimer.Stop()
	w.cut = nil
	return w.cause
}

// touchWriter reports every write that moved bytes to its watchdog.
type touchWriter struct {
	w   io.Writer
	dog *watchdog
}

// Write writes p and touches the watchdog when bytes moved.
func (t touchWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.dog.touch()
	}
	return n, err
}

// refusedBeforeBinding are the results of requests refused before a session was
// recognised: forbidden on the reverse proxy, refused on the forward proxy, and
// auth_failed and wrong_source on both. Anyone who can reach an entrypoint can produce
// them, so their rows are limited per source address.
var refusedBeforeBinding = map[string]bool{"forbidden": true, "refused": true, "auth_failed": true, "wrong_source": true}

// Limits of refusalRows: rows per source address and window, and the number of
// addresses one window tracks.
const (
	refusalRowsPerSource = 10
	refusalWindow        = time.Minute
	refusalSources       = 1024
)

// refusalRows counts, per source address, the rows of refused requests written in the
// current window. Rows beyond the limit are dropped and only counted; the count is
// logged when the window ends.
type refusalRows struct {
	mu      sync.Mutex
	start   time.Time
	counts  map[string]int
	dropped int
}

// allow reports whether a refused request from source may still be recorded at now.
// A source first seen while the window already tracks refusalSources addresses is
// not recorded.
func (l *refusalRows) allow(source string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil || now.Sub(l.start) >= refusalWindow {
		if l.dropped > 0 {
			log.Printf("refused requests not recorded=%d", l.dropped)
		}
		l.start, l.counts, l.dropped = now, map[string]int{}, 0
	}
	count, seen := l.counts[source]
	if count >= refusalRowsPerSource || (!seen && len(l.counts) >= refusalSources) {
		l.dropped++
		return false
	}
	l.counts[source] = count + 1
	return true
}
