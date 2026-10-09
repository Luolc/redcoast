package claude

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// captureBodyLimit is how many bytes of each body are kept for analysis. Byte counts
// cover the whole body regardless.
const captureBodyLimit = 64 << 10

// Indexes of the four observation points, in the order a request passes them.
const (
	clientIn    = iota // request as received from the client
	upstreamOut        // request as sent upstream, after Rewrite
	upstreamIn         // response as received from upstream
	clientOut          // response as written to the client
)

// pointNames are the record keys of the observation points, by index.
var pointNames = [...]string{"client_in", "upstream_out", "upstream_in", "client_out"}

// bodyCapture accumulates what passed through one body: the first bytes, the total
// count and how the stream ended. Reads and the final record can happen on different
// goroutines, hence the mutex.
type bodyCapture struct {
	mu          sync.Mutex
	bytes       []byte // first captureBodyLimit bytes
	count       int64  // all bytes seen
	first       *int64 // nanoseconds from request start to the first byte; nil before it
	length      int64  // declared Content-Length of a request body; 0 when unknown
	eof, failed bool   // ended with io.EOF or the declared length; saw any other error
}

// add records one read or write of p that returned err. start is the request start.
func (b *bodyCapture) add(p []byte, err error, start time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > 0 && b.first == nil {
		n := time.Since(start).Nanoseconds()
		b.first = &n
	}
	b.count += int64(len(p))
	room := captureBodyLimit - len(b.bytes)
	b.bytes = append(b.bytes, p[:min(room, len(p))]...)
	// A request body is complete at its declared length. ReverseProxy closes it when
	// the handler returns, and the transport's read for EOF can come after that.
	complete := b.length > 0 && b.count == b.length
	b.eof = b.eof || err == io.EOF || complete
	b.failed = b.failed || (err != nil && err != io.EOF && (!complete || !errors.Is(err, http.ErrBodyReadAfterClose)))
}

// bodyView is the saved summary of one body.
type bodyView struct {
	Observed  int64  `json:"observed_bytes"`
	First     *int64 `json:"first_byte_ns,omitempty"`
	Complete  bool   `json:"http_body_complete"`
	Failed    bool   `json:"io_failed"`
	Truncated bool   `json:"capture_truncated"`
	Analysis  any    `json:"safe_analysis"`
}

// view summarizes the body. encoding is its Content-Encoding. The analysis runs after
// the lock is released, so decompression and parsing do not block readers.
func (b *bodyCapture) view(encoding string) bodyView {
	b.mu.Lock()
	data := append([]byte(nil), b.bytes...)
	view := bodyView{Observed: b.count, First: b.first, Complete: b.eof && !b.failed, Failed: b.failed, Truncated: b.count > int64(len(b.bytes))}
	b.mu.Unlock()
	view.Analysis = safeEncodedBody(data, encoding)
	return view
}

// capturedBody wraps a request or response body and records every read into capture.
// Callers see the same data and errors as without the wrapper.
type capturedBody struct {
	io.ReadCloser
	capture *bodyCapture
	start   time.Time
}

// Read reads from the wrapped body and records the bytes and error.
func (r capturedBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.capture.add(p[:n], err, r.start)
	return n, err
}

// Close closes the wrapped body and records a failure if closing fails.
func (r capturedBody) Close() error {
	err := r.ReadCloser.Close()
	if err != nil {
		r.capture.add(nil, err, r.start)
	}
	return err
}

// point is the saved record of one observation point. Request points fill Method,
// Path and Query; response points fill Status.
type point struct {
	Reached bool        `json:"reached"`
	At      int64       `json:"at_ns"`
	Method  string      `json:"method,omitempty"`
	Path    string      `json:"path,omitempty"`
	Query   url.Values  `json:"query,omitempty"`
	Host    string      `json:"host,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Status  int         `json:"status,omitempty"`
	Body    bodyView    `json:"body"`
}

// observation collects the four points of one request while it is forwarded.
type observation struct {
	id     string
	start  time.Time
	points [4]point
	bodies [4]bodyCapture
}

// observationKey is the context key under which newCapturedProxy stores the
// observation for captureTransport. An unexported type cannot collide with other keys.
type observationKey struct{}

// newObservation starts observing r: it records the client_in point and wraps the
// request body so the bytes the proxy reads are counted.
func newObservation(id string, r *http.Request) *observation {
	o := &observation{id: id, start: time.Now()}
	o.points[clientIn] = requestPoint(r, o.start)
	if r.Body == nil || r.Body == http.NoBody {
		o.bodies[clientIn].eof = true
	} else {
		o.bodies[clientIn].length = r.ContentLength
		r.Body = capturedBody{r.Body, &o.bodies[clientIn], o.start}
	}
	return o
}

// requestPoint classifies a request into a point.
func requestPoint(r *http.Request, start time.Time) point {
	return point{Reached: true, At: time.Since(start).Nanoseconds(), Method: r.Method, Path: safePath(r.URL.EscapedPath()), Query: safeQuery(r.URL.Query()), Host: safeHost(r.Host), Headers: safeHeaders(r.Header)}
}

// record builds the saved record once the handler has finished. r is the original
// request, locallyGenerated tells whether the gateway wrote the response itself, and
// aborted whether forwarding panicked.
func (o *observation) record(r *http.Request, locallyGenerated, aborted bool) captureRecord {
	var views [4]bodyView
	for i := range views {
		views[i] = o.bodies[i].view(o.points[i].Headers.Get("Content-Encoding"))
	}
	// A successful write is local acceptance, not remote consumption. The client
	// response counts as complete only when it also carried the whole upstream body,
	// unless there was no upstream response to carry.
	views[clientOut].Complete = o.points[clientOut].Reached && !aborted && !views[clientOut].Failed &&
		(locallyGenerated || !o.points[upstreamIn].Reached ||
			(views[upstreamIn].Complete && views[clientOut].Observed == views[upstreamIn].Observed))
	record := captureRecord{ID: o.id, Started: o.start.UTC(), Duration: time.Since(o.start).Nanoseconds(), Canceled: r.Context().Err() != nil, Aborted: aborted, Points: make(map[string]point)}
	for i, name := range pointNames {
		p := o.points[i]
		if i == clientOut && p.Reached {
			p.Host = safeHost(r.Host)
		}
		p.Body = views[i]
		record.Points[name] = p
	}
	return record
}

// captureTransport sits between the ReverseProxy and the real transport and records
// the upstream_out and upstream_in points. It sees requests after Rewrite, so it
// records the replaced authentication.
type captureTransport struct{ base http.RoundTripper }

// RoundTrip records the outgoing request, sends it with the base transport and
// records the response. Requests without an observation pass through unrecorded.
func (t captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	o, _ := r.Context().Value(observationKey{}).(*observation)
	if o == nil {
		return t.base.RoundTrip(r)
	}
	o.points[upstreamOut] = requestPoint(r, o.start)
	if r.Body != nil {
		o.bodies[upstreamOut].length = r.ContentLength
		r.Body = capturedBody{r.Body, &o.bodies[upstreamOut], o.start}
	} else {
		o.bodies[upstreamOut].eof = true
	}
	res, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	o.points[upstreamIn] = point{Reached: true, At: time.Since(o.start).Nanoseconds(), Status: res.StatusCode, Host: safeHost(r.Host), Headers: safeHeaders(res.Header)}
	if res.Body == http.NoBody || r.Method == "HEAD" {
		o.bodies[upstreamIn].eof = true
	}
	res.Body = capturedBody{res.Body, &o.bodies[upstreamIn], o.start}
	return res, nil
}

// captureWriter wraps the client's ResponseWriter and records the client_out point.
type captureWriter struct {
	http.ResponseWriter
	o *observation
}

// Unwrap lets http.ResponseController reach the wrapped writer, for example to set
// the write deadline.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteHeader records the final status and headers on the first call. Informational
// 1xx responses pass through unrecorded.
func (w *captureWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if !w.o.points[clientOut].Reached {
		w.o.points[clientOut] = point{Reached: true, At: time.Since(w.o.start).Nanoseconds(), Status: status, Headers: safeHeaders(w.Header())}
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write writes p to the client and records the bytes the writer accepted.
func (w *captureWriter) Write(p []byte) (int, error) {
	if !w.o.points[clientOut].Reached {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.o.bodies[clientOut].add(p[:n], err, w.o.start)
	return n, err
}

// FlushError flushes the wrapped writer and records a failure. ResponseController
// prefers this method, so flush errors from streaming reach the record.
func (w *captureWriter) FlushError() error {
	if !w.o.points[clientOut].Reached {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.o.bodies[clientOut].add(nil, err, w.o.start)
	}
	return err
}

// captureRecord is the saved record of one request.
type captureRecord struct {
	ID       string           `json:"request_id"`
	Account  string           `json:"account_alias,omitempty"`
	Started  time.Time        `json:"started_utc"`
	Duration int64            `json:"duration_ns"`
	Canceled bool             `json:"client_context_canceled"`
	Aborted  bool             `json:"forward_aborted"`
	Points   map[string]point `json:"points"`
}

// newCapturedProxy returns inner with recording: every request is observed at the
// four points and its record is passed to sink when the handler ends. A sink failure
// is logged and does not affect forwarding. inner's outgoing transport must be wrapped
// in captureTransport for the upstream points to be recorded.
func newCapturedProxy(inner http.Handler, sink func(captureRecord) error) http.Handler {
	// A random prefix keeps request IDs, and so file names, unique across runs.
	prefix := rand.Text()
	var sequence atomic.Uint64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, state := withState(r.Context())
		o := newObservation(fmt.Sprintf("%s-%d", prefix, sequence.Add(1)), r)
		out := &captureWriter{ResponseWriter: w, o: o}
		defer func() {
			// ReverseProxy panics with http.ErrAbortHandler when the upstream body fails
			// after the response started. Record first, then re-panic so net/http still
			// aborts the client connection.
			aborted := recover()
			record := o.record(r, state.upstreamUnavailable, aborted != nil)
			record.Account = state.alias
			if err := sink(record); err != nil {
				log.Printf("capture persistence failed request_id=%s", o.id)
			}
			if aborted != nil {
				panic(aborted)
			}
		}()
		inner.ServeHTTP(out, r.WithContext(context.WithValue(ctx, observationKey{}, o)))
	})
}

// captureWriteTimeout bounds how long a request handler waits for its record to be saved.
const captureWriteTimeout = 5 * time.Second

// RecordStore saves records as files in dir, at most 64 per process.
type RecordStore struct {
	dir      string
	attempts atomic.Uint64
	create   func(string) (io.WriteCloser, error) // nil means os.OpenFile; tests inject faults
	timeout  time.Duration                        // 0 means captureWriteTimeout
	known    atomic.Pointer[strings.Replacer]     // removes known credentials; nil without them
	seen     []*account                           // every account whose credentials known removes

	mu      sync.Mutex // guards closed and every pending.Go, so drain sees all saves
	closed  bool
	pending sync.WaitGroup
}

// learn adds set's credentials to the ones known removes. The earlier ones stay: a
// request still forwarding on an account the set replaced can still produce a record
// that carries that account's credentials.
func (s *RecordStore) learn(set *AccountSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range set.sorted() {
		if !slices.Contains(s.seen, a) {
			s.seen = append(s.seen, a)
		}
	}
	s.known.Store(knownValueReplacerFor(s.seen))
}

// write saves r as <dir>/<ID>.json, waiting at most s.timeout. The record has its own
// deadline instead of the request context so a canceled request is still recorded.
// A file system call cannot be interrupted, so a save that is still blocked when the
// deadline passes keeps running under s.pending, is never committed, and is reported
// by drain at shutdown.
func (s *RecordStore) write(r captureRecord) error {
	return s.commit(r.ID, r)
}

// writeConnect saves a forward-proxy record as <dir>/connect-<ID>.json under the same
// budget, deadline and known-value replacement as write.
func (s *RecordStore) writeConnect(r connectRecord) error {
	return s.commit("connect-"+r.ID, r)
}

// commit encodes record, removes the known credentials and saves it as <dir>/<id>.json.
func (s *RecordStore) commit(id string, record any) error {
	if s.attempts.Add(1) > 64 {
		return fmt.Errorf("capture session budget exhausted")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("capture encoding failed")
	}
	// A second layer behind the classifiers: it runs on the encoded record, so it
	// covers every field, including the ones a classifier keeps verbatim.
	if known := s.known.Load(); known != nil {
		data = []byte(known.Replace(string(data)))
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("capture record budget exceeded")
	}
	timeout := s.timeout
	if timeout == 0 {
		timeout = captureWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	saved := make(chan error, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("capture store closed")
	}
	s.pending.Go(func() { saved <- s.save(ctx, id, data) })
	s.mu.Unlock()
	select {
	case err := <-saved:
		return err
	case <-ctx.Done():
		return fmt.Errorf("capture write timed out")
	}
}

// save writes data to <id>.partial and renames it to <id>.json. The rename is atomic,
// so a .json file is always complete; on failure, or once ctx has ended, the .partial
// file stays behind and nothing is committed.
func (s *RecordStore) save(ctx context.Context, id string, data []byte) error {
	path := filepath.Join(s.dir, id+".partial")
	create := s.create
	if create == nil {
		create = func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		}
	}
	file, err := create(path)
	if err != nil {
		return fmt.Errorf("capture create failed")
	}
	n, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || n != len(data) || closeErr != nil {
		return fmt.Errorf("capture write or close failed")
	}
	if ctx.Err() != nil {
		return fmt.Errorf("capture write timed out")
	}
	if err := os.Rename(path, filepath.Join(s.dir, id+".json")); err != nil {
		return fmt.Errorf("capture commit failed")
	}
	return nil
}

// drain stops the store from starting new saves, then waits until every started save
// has returned, or fails when ctx ends first. Handlers can outlive Server.Close, so a
// write that arrives after drain is refused instead of starting an unowned save.
// The helper goroutine only outlives a failed drain while the process is exiting.
func (s *RecordStore) drain(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("capture writes still pending")
	}
}
