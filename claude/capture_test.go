package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// TestFourPointCapture checks that recording leaves forwarding unchanged, keeps no
// synthetic secret or session credential, and marks all four points complete.
func TestFourPointCapture(t *testing.T) {
	requestBody := `{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"Hi"}],"metadata":{"user_id":"private-identity"},"token":{"text":"private-token"},"extension":{"max_tokens":884466}}`
	responseBody := `{"type":"message","id":"private-response-id","content":[{"type":"text","text":"Hi!"}],"usage":{"output_tokens":2}}`
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		received <- string(b) + "|" + r.Header.Get("Authorization") + "|" + r.URL.Query().Get("api_key") + "|" + r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "private-cookie")
		w.Header()["X-Multi"] = []string{"private-one", "private-two"}
		if _, err := io.WriteString(w, responseBody); err != nil {
			return
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	records := make(chan captureRecord, 1)
	server := h.captured(upstream.URL, func(r captureRecord) error { records <- r; return nil })
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages?api_key=private-query", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "private-key")
	req.Header.Set("Cookie", "private-client-cookie")
	req.Header.Set("Proxy-Authorization", "private-proxy-auth")
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != responseBody || res.Header.Get("Set-Cookie") != "private-cookie" || !reflect.DeepEqual(res.Header.Values("X-Multi"), []string{"private-one", "private-two"}) {
		t.Fatal("capture changed response")
	}
	// The client's credentials are replaced by the account's token; the query is forwarded as sent.
	if got := <-received; got != requestBody+"|Bearer "+h.accounts.lookup("sample-a").token+"|private-query||" {
		t.Fatalf("forwarded=%q", got)
	}
	var record captureRecord
	select {
	case record = <-records:
	case <-time.After(time.Second):
		t.Fatal("capture missing")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range record.Points {
		analysis, ok := point.Body.Analysis.(map[string]any)
		if ok && analysis["format"] == "raw-base64" {
			raw, err := base64.StdEncoding.DecodeString(analysis["data"].(string))
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, raw...)
		}
	}
	for _, secret := range []string{"private-key", "private-client-cookie", "private-proxy-auth", h.token, "private-query", "private-cookie", "private-one", "private-two", "private-identity", "private-token", "private-response-id", "884466"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("capture contains %s", secret)
		}
	}
	for _, stage := range []string{"client_in", "upstream_out", "upstream_in", "client_out"} {
		p := record.Points[stage]
		if !p.Reached || !p.Body.Complete || p.Body.Failed {
			t.Fatalf("%s=%+v", stage, p)
		}
	}
	if len(record.Points["upstream_in"].Headers["X-Multi"]) != 2 {
		t.Fatal("header multiplicity lost")
	}
	if !strings.Contains(string(encoded), "Hi!") || !strings.Contains(string(encoded), "output_tokens") || !strings.Contains(string(encoded), "application/json") {
		t.Fatal("safe analysis lost")
	}
}

// TestCaptureBudgetAndFailurePreserveStream checks that a body beyond the capture limit
// and a failing sink both leave streaming intact.
func TestCaptureBudgetAndFailurePreserveStream(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[capture], func(t *testing.T) {
			release := make(chan struct{})
			records := make(chan captureRecord, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if _, err := io.WriteString(w, strings.Repeat("x", captureBodyLimit*2)); err != nil {
					return
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			server := h.gateway(upstream.URL)
			if capture {
				server = h.captured(upstream.URL, func(r captureRecord) error { records <- r; return errors.New("persistence unavailable") })
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := res.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			first := make([]byte, 13)
			if _, err := io.ReadFull(res.Body, first); err != nil {
				t.Fatal(err)
			}
			if string(first) != "data: first\n\n" {
				t.Fatalf("first=%q", first)
			}
			close(release)
			rest, err := io.ReadAll(res.Body)
			if err != nil || len(rest) != captureBodyLimit*2 {
				t.Fatalf("len=%d err=%v", len(rest), err)
			}
			if capture {
				var record captureRecord
				select {
				case record = <-records:
				case <-time.After(time.Second):
					t.Fatal("capture missing")
				}
				for _, stage := range []string{"upstream_in", "client_out"} {
					b := record.Points[stage].Body
					if !b.Complete || !b.Truncated || b.Observed != int64(13+captureBodyLimit*2) {
						t.Fatalf("%s=%+v", stage, b)
					}
				}
			}
		})
	}
}

// TestRecordStoreCommit checks that a saved record is committed as .json without a .partial.
func TestRecordStoreCommit(t *testing.T) {
	dir := t.TempDir()
	store := RecordStore{dir: dir}
	record := captureRecord{ID: "synthetic-id"}
	if err := store.write(record); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "synthetic-id.json"))
	if err != nil {
		t.Fatal(err)
	}
	var read captureRecord
	if err := json.Unmarshal(data, &read); err != nil || read.ID != record.ID {
		t.Fatal("invalid committed record")
	}
	if _, err := os.Stat(filepath.Join(dir, "synthetic-id.partial")); !os.IsNotExist(err) {
		t.Fatal("partial remains")
	}
	failed := RecordStore{dir: filepath.Join(dir, "absent")}
	if err := failed.write(record); err == nil {
		t.Fatal("missing directory accepted")
	}
}

// TestCapturedCancellationAndTruncation checks how client cancellation and a truncated
// upstream body are recorded.
func TestCapturedCancellationAndTruncation(t *testing.T) {
	for _, cancelClient := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "truncated"}[cancelClient], func(t *testing.T) {
			records := make(chan captureRecord, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !cancelClient {
					w.Header().Set("Content-Length", "100")
				}
				if _, err := io.WriteString(w, "partial"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				if cancelClient {
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			server := h.captured(upstream.URL, func(r captureRecord) error { records <- r; return nil })
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := res.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			first := make([]byte, 7)
			if _, err := io.ReadFull(res.Body, first); err != nil {
				t.Fatal(err)
			}
			if cancelClient {
				cancel()
			} else {
				if _, err := io.ReadAll(res.Body); err == nil {
					t.Fatal("truncation accepted")
				}
			}
			var r captureRecord
			select {
			case r = <-records:
			case <-time.After(time.Second):
				t.Fatal("capture missing")
			}
			if !r.Aborted || r.Canceled != cancelClient || r.Points["upstream_in"].Body.Complete || r.Points["client_out"].Body.Complete {
				t.Fatalf("record=%+v", r)
			}
		})
	}
}

// TestRawCaptureAcrossReads checks that capturedBody keeps exact bytes whatever the read sizes.
func TestRawCaptureAcrossReads(t *testing.T) {
	body := []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi!\"}}\r\n\r\n")
	for _, size := range []int{1, 7, 1024} {
		b := bodyCapture{}
		reader := capturedBody{io.NopCloser(iotest.DataErrReader(bytes.NewReader(body))), &b, time.Now()}
		var received bytes.Buffer
		buffer := make([]byte, size)
		if _, err := io.CopyBuffer(struct{ io.Writer }{&received}, reader, buffer); err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		view := b.view("")
		analysis := view.Analysis.(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(analysis["data"].(string))
		if err != nil || !view.Complete || !bytes.Equal(raw, body) || !bytes.Equal(received.Bytes(), body) {
			t.Fatalf("size=%d view=%+v err=%v", size, view, err)
		}
	}
}

// faultFile injects a write, short-write or close failure into a record file.
type faultFile struct {
	*os.File
	fault string
}

// Write fails or writes half of p when the fault asks for it.
func (f faultFile) Write(p []byte) (int, error) {
	if f.fault == "write" {
		return 0, io.ErrShortWrite
	}
	if f.fault == "short" {
		return f.File.Write(p[:len(p)/2])
	}
	return f.File.Write(p)
}

// Close closes the file and reports a failure when the fault asks for it.
func (f faultFile) Close() error {
	err := f.File.Close()
	if f.fault == "close" {
		return errors.New("close failed")
	}
	return err
}

// TestPersistenceFailuresLeaveNoCommittedRecord checks that a failed save leaves only a .partial file.
func TestPersistenceFailuresLeaveNoCommittedRecord(t *testing.T) {
	for _, fault := range []string{"write", "short", "close"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			store := RecordStore{dir: dir, create: func(path string) (io.WriteCloser, error) {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return nil, err
				}
				return faultFile{f, fault}, nil
			}}
			if err := store.write(captureRecord{ID: "synthetic"}); err == nil {
				t.Fatal("persistence failure reported success")
			}
			if _, err := os.Stat(filepath.Join(dir, "synthetic.json")); !os.IsNotExist(err) {
				t.Fatal("failed record committed")
			}
			if _, err := os.Stat(filepath.Join(dir, "synthetic.partial")); err != nil {
				t.Fatal("failure artifact missing")
			}
		})
	}
}

// TestGzipAnalysisPreservesForwarding checks that gzip bodies are forwarded unchanged
// and classified after decompression.
func TestGzipAnalysisPreservesForwarding(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(writer, `{"content":[{"type":"text","text":"Hi!"}],"token":"private-compressed-token"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		if _, err := w.Write(compressed.Bytes()); err != nil {
			return
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	records := make(chan captureRecord, 1)
	server := h.captured(upstream.URL, func(r captureRecord) error { records <- r; return nil })
	client := h.client()
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, compressed.Bytes()) || res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("compressed forwarding changed")
	}
	var record captureRecord
	select {
	case record = <-records:
	case <-time.After(time.Second):
		t.Fatal("capture missing")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-compressed-token") || !strings.Contains(string(encoded), "Hi!") || !strings.Contains(string(encoded), "gzip-analysis") {
		t.Fatal("unsafe or missing gzip analysis")
	}
}

// artifactContains reports whether text occurs in a decoded record, including in field
// names and in raw-base64 data.
func artifactContains(t *testing.T, value any, text string) bool {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		if v["format"] == "raw-base64" {
			raw, err := base64.StdEncoding.DecodeString(v["data"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(text)) {
				return true
			}
		}
		for key, child := range v {
			if strings.Contains(key, text) || artifactContains(t, child, text) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if artifactContains(t, child, text) {
				return true
			}
		}
	case string:
		return strings.Contains(v, text)
	}
	return false
}

// TestDuplicateKeysSavedCapture checks that the value discarded for a duplicate JSON key
// never reaches a saved record, for JSON, SSE and gzip bodies.
func TestDuplicateKeysSavedCapture(t *testing.T) {
	const marker = "synthetic-discarded-value"
	for _, kind := range []string{"json", "sse", "gzip"} {
		for _, duplicate := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/single", true: "/duplicate"}[duplicate], func(t *testing.T) {
				payload := []byte(`{"text":"Hi"}`)
				if duplicate {
					payload = []byte(`{"text":"` + marker + `","text":"Hi"}`)
				}
				contentType, encoding := "application/json", ""
				if kind == "sse" {
					payload = append(append([]byte("data: "), payload...), []byte("\r\n\r\n")...)
					contentType = "text/event-stream"
				}
				if kind == "gzip" {
					var compressed bytes.Buffer
					writer := gzip.NewWriter(&compressed)
					if _, err := writer.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					payload = compressed.Bytes()
					encoding = "gzip"
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", contentType)
					if encoding != "" {
						w.Header().Set("Content-Encoding", encoding)
					}
					if _, err := w.Write(payload); err != nil {
						return
					}
				}))
				defer upstream.Close()
				h := newHarness(t, "sample-a")
				store := RecordStore{dir: t.TempDir()}
				committed := make(chan error, 1)
				proxy := h.captured(upstream.URL, func(r captureRecord) error { err := store.write(r); committed <- err; return err })
				client := h.client()
				req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", nil)
				if err != nil {
					t.Fatal(err)
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(res.Body)
				closeErr := res.Body.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
					t.Fatal("forwarding changed")
				}
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("record missing")
				}
				files, err := filepath.Glob(filepath.Join(store.dir, "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatal("committed file missing")
				}
				data, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				var decoded any
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if artifactContains(t, decoded, marker) {
					t.Fatal("discarded value persisted")
				}
				if !artifactContains(t, decoded, "Hi") {
					t.Fatal("safe greeting lost")
				}
			})
		}
	}
}

// TestCapturedUpgradeRejection checks that an upstream 101 becomes a complete local 502.
func TestCapturedUpgradeRejection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	records := make(chan captureRecord, 1)
	server := h.captured(upstream.URL, func(record captureRecord) error {
		records <- record
		return nil
	})
	client := h.client()
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if readErr != nil || closeErr != nil || res.StatusCode != http.StatusBadGateway || string(body) != "upstream unavailable\n" {
		t.Fatalf("status=%d body=%q read=%v close=%v", res.StatusCode, body, readErr, closeErr)
	}
	select {
	case record := <-records:
		out := record.Points["client_out"]
		if record.Points["upstream_in"].Status != http.StatusSwitchingProtocols || out.Status != http.StatusBadGateway || !out.Body.Complete || out.Body.Failed || record.Aborted || out.Body.Observed != int64(len(body)) {
			t.Fatalf("complete local response misclassified: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("capture missing")
	}
}

// TestQuotaAndErrorSavedCapture checks which quota headers and error fields a saved
// record keeps.
func TestQuotaAndErrorSavedCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790000000")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "synthetic_status_17")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "synthetic_claim_23")
		w.Header().Set("Anthropic-Ratelimit-Unified-Unlisted", "synthetic_unlisted_31")
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"synthetic_detail_47"}}`); err != nil {
			return
		}
	}))
	defer upstream.Close()
	// The upstream's 401 pauses the account that sent the first request; the second
	// request rebinds to the other account and reaches the upstream too.
	h := newHarness(t, "sample-a", "sample-b")
	store := &RecordStore{dir: t.TempDir()}
	// The sink runs after the handler returns, so the client can see the response first.
	committed := make(chan error, 2)
	server := h.captured(upstream.URL, func(record captureRecord) error {
		err := store.write(record)
		committed <- err
		return err
	})
	req, err := http.NewRequestWithContext(t.Context(), "HEAD", server.URL+"/api/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Request headers share the classifier: a quota-prefixed client header must not carry an identifier through.
	req.Header.Set("Anthropic-Ratelimit-Unified-Private-Identifier", "synthetic_account_42")
	req.Header.Set("Anthropic-Ratelimit-Unified-Status", "synthetic_account_53")
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	req, err = http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-committed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("observation not committed in time")
		}
	}
	files, err := os.ReadDir(store.dir)
	if err != nil || len(files) != 2 {
		t.Fatal("observations not committed")
	}
	var saved string
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(store.dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		saved += string(data)
	}
	for _, kept := range []string{`"Anthropic-Ratelimit-Unified-Status":["allowed"]`, `"Anthropic-Ratelimit-Unified-5h-Utilization":["0.42"]`, `"Anthropic-Ratelimit-Unified-5h-Reset":["1790000000"]`, `"Retry-After":["30"]`, `"X-Should-Retry":["false"]`, `"Anthropic-Ratelimit-Unified-Unlisted":["` + hidden, `"Anthropic-Ratelimit-Unified-Representative-Claim":["` + redacted, `"path":"/api/hello"`, `authentication_error`} {
		if !strings.Contains(saved, kept) {
			t.Errorf("saved capture lost %s", kept)
		}
	}
	if strings.Contains(saved, "synthetic_") {
		t.Error("saved capture kept an unclassified value")
	}
}

// TestDynamicFieldNamesSavedCapture checks that client-chosen JSON field names and query
// names never reach a saved record, while protocol fields remain.
func TestDynamicFieldNamesSavedCapture(t *testing.T) {
	const requestBody = `{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"Hi"}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"synthetic-private-property":{"type":"string"}}}}],` +
		`"metadata":{"synthetic-private-metadata":"value"},"extension":{"synthetic-private@example.invalid":"synthetic-private-value"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	store := &RecordStore{dir: t.TempDir()}
	committed := make(chan error, 1)
	server := h.captured(upstream.URL, func(r captureRecord) error {
		err := store.write(r)
		committed <- err
		return err
	})
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages?synthetic-private-query=1&api_key=synthetic-key", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("record missing")
	}
	files, err := filepath.Glob(filepath.Join(store.dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("committed file missing")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Points map[string]struct {
			Query map[string][]string `json:"query"`
			Body  struct {
				Analysis struct {
					Data any `json:"data"`
				} `json:"safe_analysis"`
			} `json:"body"`
		} `json:"points"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if artifactContains(t, decoded, "synthetic-private") {
		t.Fatal("dynamic field name persisted")
	}
	in := saved.Points["client_in"]
	if !reflect.DeepEqual(in.Query["api_key"], []string{redacted}) || !reflect.DeepEqual(in.Query[hidden], []string{hidden}) {
		t.Fatalf("query classification changed: %v", in.Query)
	}
	body, _ := in.Body.Analysis.Data.(map[string]any)
	if body["max_tokens"] != 8.0 || body["model"] != "claude-test" || !artifactContains(t, body["messages"], "Hi") {
		t.Fatalf("protocol fields lost: %v", body)
	}
	if !reflect.DeepEqual(body[hidden], map[string]any{"fields": 1.0}) {
		t.Fatalf("unclassified top-level fields not counted: %v", body[hidden])
	}
}

// TestBlockedRecordStoreIsBoundedAndReported checks that blocked storage delays the
// handler only until the write deadline, is reported by drain, and commits nothing.
func TestBlockedRecordStoreIsBoundedAndReported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "Hi"); err != nil {
			return
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	release := make(chan struct{})
	store := &RecordStore{dir: t.TempDir(), timeout: 50 * time.Millisecond, create: func(path string) (io.WriteCloser, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		<-release
		return file, err
	}}
	saved := make(chan error, 1)
	server := h.captured(upstream.URL, func(r captureRecord) error {
		err := store.write(r)
		saved <- err
		return err
	})
	// Runs before server.Close so a handler stuck on storage cannot hang the test.
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	res, err := h.client().Post(server.URL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "Hi" {
		t.Fatal("forwarding changed")
	}
	select {
	case err := <-saved:
		if err == nil {
			t.Fatal("blocked save reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler still waiting on blocked storage")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := store.drain(waitCtx); err == nil {
		t.Fatal("blocked save not reported as pending")
	}
	releaseOnce.Do(func() { close(release) })
	if err := store.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	committed, err := filepath.Glob(filepath.Join(store.dir, "*.json"))
	if err != nil || len(committed) != 0 {
		t.Fatal("late save was committed")
	}
	partial, err := filepath.Glob(filepath.Join(store.dir, "*.partial"))
	if err != nil || len(partial) != 1 {
		t.Fatal("late save left no failure artifact")
	}
}

// TestLateRecordAfterDrainIsRefused checks that a handler reaching its sink after drain
// cannot start a save.
func TestLateRecordAfterDrainIsRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "Hi"); err != nil {
			return
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	store := &RecordStore{dir: t.TempDir()}
	entered := make(chan struct{})
	release := make(chan struct{})
	saved := make(chan error, 1)
	// The handler is held before its record is saved, like a handler that Server.Close
	// abandoned and that reaches its deferred sink only after main has drained the store.
	server := h.captured(upstream.URL, func(r captureRecord) error {
		close(entered)
		<-release
		err := store.write(r)
		saved <- err
		return err
	})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	res, err := h.client().Post(server.URL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "Hi" {
		t.Fatal("forwarding changed")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not reach its sink")
	}
	server.CloseClientConnections()
	if err := store.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-saved:
		if err == nil {
			t.Fatal("record saved after drain")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late handler did not finish")
	}
	files, err := os.ReadDir(store.dir)
	if err != nil || len(files) != 0 {
		t.Fatal("late record reached the capture directory")
	}
}

// lateEOFRead holds the transport's read for EOF, the one after the declared length,
// until returned is closed, and closes probed once that read is done.
type lateEOFRead struct {
	base             http.RoundTripper
	returned, probed chan struct{}
}

func (t lateEOFRead) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Body = &lateEOFBody{ReadCloser: r.Body, length: r.ContentLength, t: t}
	return t.base.RoundTrip(r)
}

type lateEOFBody struct {
	io.ReadCloser
	length, count int64
	t             lateEOFRead
}

func (b *lateEOFBody) Read(p []byte) (int, error) {
	if b.count < b.length {
		n, err := b.ReadCloser.Read(p)
		b.count += int64(n)
		return n, err
	}
	<-b.t.returned
	defer close(b.t.probed)
	return b.ReadCloser.Read(p)
}

// TestCaptureCompleteBodyReadAfterClose is the regression for a complete request body
// recorded as failed: the transport's read for EOF came after ReverseProxy had closed
// the body, before the record was made.
func TestCaptureCompleteBodyReadAfterClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	late := lateEOFRead{accountTransport{}, make(chan struct{}), make(chan struct{})}
	router := h.router(target, captureTransport{late})
	records := make(chan captureRecord, 1)
	server := httptest.NewServer(newCapturedProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		close(late.returned)
		<-late.probed
	}), func(r captureRecord) error { records <- r; return nil }))
	defer server.Close()
	res, err := h.client().Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	var record captureRecord
	select {
	case record = <-records:
	case <-time.After(5 * time.Second):
		t.Fatal("capture missing")
	}
	for _, stage := range []string{"client_in", "upstream_out"} {
		if p := record.Points[stage]; !p.Body.Complete || p.Body.Failed {
			t.Fatalf("%s=%+v", stage, p.Body)
		}
	}
}
