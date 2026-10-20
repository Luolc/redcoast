package claude

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Luolc/redcoast/session"
)

// Synthetic Messages API bodies: a stream and the same response not streamed.
const (
	claudeStream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-sample-1","content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the \"usage\" and \"model\" of a synthetic reply"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	claudeJSON = `{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-sample-1","content":[{"type":"text","text":"synthetic"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":42}}`
)

// sampleUsage is what both bodies report.
var sampleUsage = session.Usage{Model: "claude-sample-1", InputTokens: 10, OutputTokens: 42, CacheRead: 3, CacheCreation: 2}

// gzipped returns data compressed with gzip.
func gzipped(t *testing.T, data string) string {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestUsageMeter reads Claude bodies through a meter one byte per read and in one read:
// the reader gets the body unchanged, and the meter the model and counts of an
// uncompressed or gzip stream or JSON body. A line too long to keep is skipped; a
// model of another shape is dropped and the counts kept; a document with a negative
// count gives none of its counts. A JSON body over the limit,
// or a body that reports no counts, gives nothing; a response that is not a 200, or
// whose type or encoding the meter does not read, gets no meter.
func TestUsageMeter(t *testing.T) {
	// A line one byte over the limit that would replace the output count were it read.
	longLine := `data: {"type":"message_delta","usage":{"output_tokens":7},"pad":"`
	longLine += strings.Repeat("x", usageLineLimit+1-len(longLine)-2) + `"}` + "\n\n"
	overLimit := `{"type":"message","model":"claude-sample-1","content":[{"type":"text","text":"` + strings.Repeat("x", usageBodyLimit) + `"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	withModel := func(model string) session.Usage { u := sampleUsage; u.Model = model; return u }
	withOutput := func(output int64) session.Usage { u := sampleUsage; u.OutputTokens = output; return u }
	for _, arm := range []struct {
		name, contentType, encoding, body string
		want                              *session.Usage
	}{
		{"stream", "text/event-stream; charset=utf-8", "", claudeStream, &sampleUsage},
		{"stream_crlf", "text/event-stream", "", strings.ReplaceAll(claudeStream, "\n", "\r\n"), &sampleUsage},
		{"stream_gzip", "text/event-stream", "gzip", gzipped(t, claudeStream), &sampleUsage},
		{"stream_without_type", "", "", claudeStream, &sampleUsage},
		{"json", "application/json", "", claudeJSON, &sampleUsage},
		{"json_gzip", "application/json", "gzip", gzipped(t, claudeJSON), &sampleUsage},
		{"long_line_skipped", "text/event-stream", "", claudeStream + longLine, &sampleUsage},
		{"bad_model", "application/json", "", strings.Replace(claudeJSON, "claude-sample-1", "<b>synthetic</b>", 1), new(withModel(""))},
		{"long_model", "application/json", "", strings.Replace(claudeJSON, "claude-sample-1", strings.Repeat("m", 129), 1), new(withModel(""))},
		{"negative_count", "text/event-stream", "", strings.Replace(claudeStream, `"output_tokens":42`, `"output_tokens":-42`, 1), new(withOutput(1))},
		{"json_over_limit", "application/json", "", overLimit, nil},
		{"count_tokens", "application/json", "", `{"input_tokens":5}`, nil},
		{"error_event", "text/event-stream", "", "event: error\ndata: {\"type\":\"error\",\"usage\":{\"input_tokens\":5}}\n\n", nil},
	} {
		for _, oneByte := range []bool{false, true} {
			name := arm.name
			if oneByte {
				name += "_one_byte"
			}
			t.Run(name, func(t *testing.T) {
				res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(arm.body))}
				if oneByte {
					res.Body = io.NopCloser(iotest.OneByteReader(strings.NewReader(arm.body)))
				}
				if arm.contentType != "" {
					res.Header.Set("Content-Type", arm.contentType)
				}
				if arm.encoding != "" {
					res.Header.Set("Content-Encoding", arm.encoding)
				}
				m := newUsageMeter(res, claudeUsage)
				if m == nil {
					t.Fatal("no meter")
				}
				got, err := io.ReadAll(m)
				if err != nil || string(got) != arm.body {
					t.Fatalf("the reader got another body: err=%v", err)
				}
				usage := m.result()
				switch {
				case arm.want == nil && usage != nil:
					t.Fatalf("usage %+v, want none", *usage)
				case arm.want != nil && (usage == nil || *usage != *arm.want):
					t.Fatalf("usage %+v, want %+v", usage, *arm.want)
				}
			})
		}
	}
	for _, arm := range []struct {
		name                       string
		status                     int
		contentType, encoding, src string
	}{
		{"not_200", http.StatusBadRequest, "application/json", "", claudeJSON},
		{"other_type", http.StatusOK, "text/plain", "", claudeJSON},
		{"other_encoding", http.StatusOK, "application/json", "br", claudeJSON},
	} {
		t.Run(arm.name, func(t *testing.T) {
			res := &http.Response{StatusCode: arm.status, Header: http.Header{"Content-Type": {arm.contentType}}, Body: io.NopCloser(strings.NewReader(arm.src))}
			if arm.encoding != "" {
				res.Header.Set("Content-Encoding", arm.encoding)
			}
			if m := newUsageMeter(res, claudeUsage); m != nil {
				t.Fatal("got a meter")
			}
		})
	}
}

// TestUsageRecordedFromStream forwards a stream whose second half the upstream sends
// only once the client has read the first: the client gets each part as it is sent and
// the whole body unchanged, and the traffic row keeps the model and counts. Control
// arm: a request whose response reports no usage leaves the usage columns NULL.
func TestUsageRecordedFromStream(t *testing.T) {
	first, rest, _ := strings.Cut(claudeStream, "event: content_block_delta\n")
	rest = "event: content_block_delta\n" + rest
	clientRead := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		if r.Header.Get("X-Synthetic-Plain") != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-clientRead:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, rest)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	server := h.gateway(upstream.URL)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	head := make([]byte, len(first))
	if _, err := io.ReadFull(res.Body, head); err != nil || string(head) != first {
		t.Fatalf("first part %q err=%v", head, err)
	}
	close(clientRead)
	tail, err := io.ReadAll(res.Body)
	if err != nil || string(tail) != rest {
		t.Fatalf("rest %q err=%v", tail, err)
	}
	plain, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	plain.Header.Set("X-Synthetic-Plain", "1")
	plainRes, err := h.client().Do(plain)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, plainRes.Body)
	_ = plainRes.Body.Close()
	if rows := h.trafficRows(2); len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	type usageRow struct {
		model                                   string
		input, output, cacheRead, cacheCreation int64
		reported                                bool
	}
	query, err := h.openFile().Query(`SELECT COALESCE(model, ''), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cache_read_tokens, 0),
		COALESCE(cache_creation_tokens, 0), input_tokens IS NOT NULL FROM traffic ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = query.Close() }()
	var got []usageRow
	for query.Next() {
		var row usageRow
		if err := query.Scan(&row.model, &row.input, &row.output, &row.cacheRead, &row.cacheCreation, &row.reported); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	want := []usageRow{{"claude-sample-1", 10, 42, 3, 2, true}, {}}
	if query.Err() != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("usage rows %+v, want %+v", got, want)
	}
}

// TestClientLeavesAtMessageStop has the upstream send a stream up to one event and then
// hold its body open, and the client close the stream once it has read that event: a
// client that leaves after message_stop leaves an ok row. Control arm: one that leaves
// after message_delta, before message_stop, leaves an incomplete row.
func TestClientLeavesAtMessageStop(t *testing.T) {
	beforeStop, _, _ := strings.Cut(claudeStream, "event: message_stop\n")
	for _, arm := range []struct{ name, stream, result string }{
		{"message_stop", claudeStream, "ok"},
		{"message_delta", beforeStop, "incomplete"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, arm.stream)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			server := h.gateway(upstream.URL)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/messages", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			head := make([]byte, len(arm.stream))
			_, err = io.ReadFull(res.Body, head)
			_ = res.Body.Close()
			if err != nil || string(head) != arm.stream {
				t.Fatalf("stream %q err=%v", head, err)
			}
			if rows := h.trafficRows(1); len(rows) != 1 || rows[0].result != arm.result || rows[0].status != http.StatusOK {
				t.Fatalf("rows %+v, want one %s row", rows, arm.result)
			}
		})
	}
}
