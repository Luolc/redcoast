package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestForwardAndReuse checks that requests and responses pass through unchanged and that
// the outbound connection is reused.
func TestForwardAndReuse(t *testing.T) {
	type received struct {
		method, uri, host, body, conn, encoding string
		values                                  []string
		hop                                     string
	}
	requests := make(chan received, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		requests <- received{r.Method, r.URL.RequestURI(), r.Host, string(body), r.RemoteAddr, r.Header.Get("Accept-Encoding"), r.Header.Values("X-Multi"), r.Header.Get("X-Hop")}
		w.Header().Add("X-Multi", "first")
		w.Header().Add("X-Multi", "second")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "removed")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte{0x1f, 0x8b, 0x00, 0xff}); err != nil {
			return
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	proxy := h.gateway(upstream.URL)
	client := h.client()
	var conn string
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages?x=1&x=2&q=a%20b", strings.NewReader("synthetic prompt"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "client.example"
		req.Header["X-Multi"] = []string{"one", "two"}
		req.Header.Set("Connection", "X-Hop")
		req.Header.Set("X-Hop", "removed")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(res.Body)
		closeErr := res.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(readErr, closeErr)
		}
		if res.StatusCode != 201 || !reflect.DeepEqual(body, []byte{0x1f, 0x8b, 0x00, 0xff}) || res.Header.Get("Content-Encoding") != "gzip" || !reflect.DeepEqual(res.Header.Values("X-Multi"), []string{"first", "second"}) || res.Header.Get("X-Hop") != "" {
			t.Fatalf("response changed: status=%d headers=%v bytes=%v", res.StatusCode, res.Header, body)
		}
		got := <-requests
		if got.method != "POST" || got.uri != "/v1/messages?x=1&x=2&q=a%20b" || got.host != strings.TrimPrefix(upstream.URL, "http://") || got.body != "synthetic prompt" || !reflect.DeepEqual(got.values, []string{"one", "two"}) || got.hop != "" || got.encoding != "" {
			t.Fatalf("upstream got %+v", got)
		}
		if conn != "" && got.conn != conn {
			t.Fatalf("connection not reused: %q != %q", got.conn, conn)
		}
		conn = got.conn
	}
}

// TestStreamingAndCancellation checks that streamed events arrive before the upstream
// finishes and that client cancellation reaches the upstream.
func TestStreamingAndCancellation(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "cancel"}[cancelClient], func(t *testing.T) {
			release := make(chan struct{})
			stopped := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(stopped)
				w.Header().Set("Content-Type", "text/event-stream")
				if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-release:
					if _, err := io.WriteString(w, "data: last\n\n"); err != nil {
						return
					}
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			proxy := h.gateway(upstream.URL)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/messages", nil)
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
			first := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(res.Body, first); err != nil {
				t.Fatal(err)
			}
			if string(first) != "data: first\n\n" {
				t.Fatalf("first=%q", first)
			}
			if cancelClient {
				cancel()
			} else {
				close(release)
				rest, err := io.ReadAll(res.Body)
				if err != nil || string(rest) != "data: last\n\n" {
					t.Fatalf("rest=%q err=%v", rest, err)
				}
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream did not stop")
			}
		})
	}
}

// TestInferenceIsNotReplayed checks that a request is not resent when the upstream drops
// the connection after processing it, even with an Idempotency-Key.
func TestInferenceIsNotReplayed(t *testing.T) {
	for _, body := range []string{"synthetic", ""} {
		t.Run(map[bool]string{true: "empty", false: "nonempty"}[body == ""], func(t *testing.T) {
			processed := make(chan struct{}, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/messages/count_tokens" {
					w.WriteHeader(204)
					return
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				processed <- struct{}{}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				}
			}))
			defer upstream.Close()
			h := newHarness(t, "sample-a")
			proxy := h.gateway(upstream.URL)
			for _, path := range []string{"/v1/messages/count_tokens", "/v1/messages"} {
				req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Idempotency-Key", "synthetic-key")
				res, err := h.client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, res.Body); err != nil {
					t.Fatal(err)
				}
				if err := res.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if path == "/v1/messages" && res.StatusCode != 502 {
					t.Fatalf("status=%d", res.StatusCode)
				}
			}
			if len(processed) != 1 {
				t.Fatalf("processed=%d want=1", len(processed))
			}

		})
	}
}

// TestTruncatedUpstream checks that a truncated upstream body reaches the client as an error.
func TestTruncatedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		if _, err := io.WriteString(w, "partial"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	proxy := h.gateway(upstream.URL)
	req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", nil)
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
	body, err := io.ReadAll(res.Body)
	if err == nil || string(body) != "partial" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

// TestStreamSurvivesRequestBodyClose is the regression for a stream cut mid-response:
// net/http closes the request body when the response header is written, the
// transport's read for EOF after the declared length came later and failed, and the
// transport closed the upstream connection.
func TestStreamSurvivesRequestBodyClose(t *testing.T) {
	release, cut := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: last\n\n")
		case <-r.Context().Done():
			close(cut)
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	late := lateEOFRead{accountTransport{}, make(chan struct{}), make(chan struct{})}
	server := httptest.NewServer(h.router(target, late))
	defer server.Close()
	res, err := h.client().Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, first); err != nil {
		t.Fatal(err)
	}
	close(late.returned)
	select {
	case <-late.probed:
	case <-time.After(2 * time.Second):
		t.Fatal("the transport did not read for EOF")
	}
	select {
	case <-cut:
		t.Fatal("the upstream request was cut after the request body was closed")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if rest, err := io.ReadAll(res.Body); err != nil || string(rest) != "data: last\n\n" {
		t.Fatalf("rest=%q err=%v", rest, err)
	}
}
