package claude

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"regexp"

	"github.com/Luolc/redcoast/session"
)

// Bounds of what a usage meter holds. A body it reads only at its end, a JSON body or a
// compressed one, is kept up to usageBodyLimit bytes and decompressed up to
// usageDecodedLimit; an event stream is read line by line and a line longer than
// usageLineLimit is skipped. Past a bound the meter reports less or nothing, never
// more.
const (
	usageBodyLimit    = 1 << 20
	usageDecodedLimit = 8 << 20
	usageLineLimit    = 64 << 10
)

// maxTokens bounds one token count the meter accepts, far above any response's, so
// that sums over the traffic table cannot overflow.
const maxTokens = 1 << 40

// UsageReport is what one JSON document of an upstream response says of the request's
// usage: the model, and the token counts as the provider counts them (session.Usage).
// An empty Model or a nil count says nothing about it. A count is the total so far for
// the request, so a later document's count replaces an earlier one's.
type UsageReport struct {
	Model         string
	InputTokens   *int64
	OutputTokens  *int64
	CacheRead     *int64
	CacheCreation *int64
}

// modelName is the shape of a model name the traffic table keeps. The name comes from
// the upstream's response and the dashboard shows it, so another shape is dropped and
// the row keeps its counts without a model.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+-]{0,127}$`)

// usageMeter wraps the body of an upstream response and reads the model and token
// counts out of the bytes the reverse proxy reads through it, on the proxy's own
// reads: the client gets the same bytes and errors at the same time as without it.
// Only the goroutine that reads the body uses it, until the handler reads result.
type usageMeter struct {
	io.ReadCloser
	read     func(document []byte) UsageReport
	stream   bool // an event stream, whose data lines are the documents
	gzipped  bool
	line     []byte // the unfinished line of an uncompressed stream
	skipping bool   // the current line passed usageLineLimit
	kept     []byte // a body read at its end
	overflow bool   // kept passed usageBodyLimit
	usage    session.Usage
	found    bool // a document carried token counts
}

// newUsageMeter returns a meter for res with read, or nil when res is not a 200 with
// a JSON or event-stream body in an encoding the meter reads (none or gzip). A body
// without a Content-Type is read as a stream: the Codex upstream's stream may come
// without one.
func newUsageMeter(res *http.Response, read func([]byte) UsageReport) *usageMeter {
	if read == nil || res.StatusCode != http.StatusOK || res.Body == nil || res.Body == http.NoBody {
		return nil
	}
	m := &usageMeter{ReadCloser: res.Body, read: read}
	switch mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mediaType {
	case "text/event-stream", "":
		m.stream = true
	case "application/json":
	default:
		return nil
	}
	switch res.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		m.gzipped = true
	default:
		return nil
	}
	return m
}

// Read reads from the wrapped body and looks at what it read.
func (m *usageMeter) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	if m.stream && !m.gzipped {
		m.scan(p[:n])
	} else if !m.overflow {
		if len(m.kept)+n > usageBodyLimit {
			m.kept, m.overflow = nil, true
		} else {
			m.kept = append(m.kept, p[:n]...)
		}
	}
	return n, err
}

// scan splits stream bytes into lines and reads each complete one.
func (m *usageMeter) scan(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.extend(p)
			return
		}
		m.extend(p[:i])
		if !m.skipping {
			m.event(m.line)
		}
		m.line, m.skipping = m.line[:0], false
		p = p[i+1:]
	}
}

// extend adds part of a line, or starts skipping the line when it gets too long.
func (m *usageMeter) extend(part []byte) {
	if m.skipping {
		return
	}
	if len(m.line)+len(part) > usageLineLimit {
		m.line, m.skipping = m.line[:0], true
		return
	}
	m.line = append(m.line, part...)
}

// event reads the data of one stream line. Both providers put each event's JSON on a
// single data line; a line naming neither usage nor a model is not decoded.
func (m *usageMeter) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimPrefix(data, []byte(" "))
	if bytes.Contains(data, []byte(`"usage"`)) || bytes.Contains(data, []byte(`"model"`)) {
		m.document(data)
	}
}

// document takes what one JSON document reports. A document with a count that is
// negative or over maxTokens gives no counts.
func (m *usageMeter) document(data []byte) {
	report := m.read(data)
	if report.Model != "" {
		m.usage.Model = report.Model
	}
	counts := []struct {
		reported *int64
		into     *int64
	}{{report.InputTokens, &m.usage.InputTokens}, {report.OutputTokens, &m.usage.OutputTokens}, {report.CacheRead, &m.usage.CacheRead}, {report.CacheCreation, &m.usage.CacheCreation}}
	for _, c := range counts {
		if c.reported != nil && (*c.reported < 0 || *c.reported > maxTokens) {
			return
		}
	}
	for _, c := range counts {
		if c.reported != nil {
			*c.into, m.found = *c.reported, true
		}
	}
}

// result reads what was kept for the end and returns the usage the response reported,
// or nil when it reported no token counts. A model of another shape than modelName is
// left out.
func (m *usageMeter) result() *session.Usage {
	if len(m.kept) > 0 && !m.overflow {
		m.finish()
	}
	if !m.found {
		return nil
	}
	usage := m.usage
	if !modelName.MatchString(usage.Model) {
		usage.Model = ""
	}
	return &usage
}

// finish reads the kept body: decompressed when it is gzip, then as one document or,
// for a stream, line by line.
func (m *usageMeter) finish() {
	body := io.Reader(bytes.NewReader(m.kept))
	if m.gzipped {
		reader, err := gzip.NewReader(body)
		if err != nil {
			return
		}
		body = io.LimitReader(reader, usageDecodedLimit)
	}
	if !m.stream {
		data, err := io.ReadAll(body)
		if err == nil && len(data) < usageDecodedLimit {
			m.document(data)
		}
		return
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := body.Read(buffer)
		m.scan(buffer[:n])
		if err != nil {
			return
		}
	}
}

// claudeUsage reads a Messages API document: a response that is not streamed (type
// message) carries the model and the usage; in a stream message_start carries them in
// its message, and message_delta carries the counts so far in its usage. Other events
// report nothing.
func claudeUsage(document []byte) UsageReport {
	type tokens struct {
		InputTokens   *int64 `json:"input_tokens"`
		OutputTokens  *int64 `json:"output_tokens"`
		CacheRead     *int64 `json:"cache_read_input_tokens"`
		CacheCreation *int64 `json:"cache_creation_input_tokens"`
	}
	type message struct {
		Model string  `json:"model"`
		Usage *tokens `json:"usage"`
	}
	var d struct {
		Type    string   `json:"type"`
		Message *message `json:"message"`
		message
	}
	if json.Unmarshal(document, &d) != nil {
		return UsageReport{}
	}
	switch {
	case d.Type == "message_start" && d.Message != nil:
		d.message = *d.Message
	case d.Type == "message", d.Type == "message_delta":
	default:
		return UsageReport{}
	}
	report := UsageReport{Model: d.Model}
	if u := d.Usage; u != nil {
		report.InputTokens, report.OutputTokens, report.CacheRead, report.CacheCreation = u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheCreation
	}
	return report
}
