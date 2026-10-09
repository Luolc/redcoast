package claude

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// deadlineRecorder is a recorder the router can set read and write deadlines on.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
}

func (deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }
func (deadlineRecorder) SetReadDeadline(time.Time) error  { return nil }

// TestSourceBinding checks that a session issued to one address serves requests from
// that address only: inference from another address is 401 with result wrong_source
// and nothing upstream, a CONNECT from another address is 407 with the same result, and
// the same requests from the issuing address go through (control arm). The handlers
// are driven directly so that the peer address can be set.
func TestSourceBinding(t *testing.T) {
	h := newHarness(t, "sample-a")
	issued, err := h.store.Issue(t.Context(), "client-a", "198.51.100.7", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	rt := h.router(target, accountTransport{})
	inference := func(remote string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Authorization", "Bearer "+issued.Token)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		rt.ServeHTTP(deadlineRecorder{rec}, req)
		return rec.Code
	}
	if status := inference("198.51.100.8:5000"); status != http.StatusUnauthorized || upstreamHits != 0 {
		t.Fatalf("inference from another address: status=%d upstream=%d", status, upstreamHits)
	}
	if status := inference("127.0.0.1:5000"); status != http.StatusUnauthorized || upstreamHits != 0 {
		t.Fatalf("inference from loopback: status=%d upstream=%d", status, upstreamHits)
	}
	if status := inference("198.51.100.7:5000"); status != http.StatusOK || upstreamHits != 1 {
		t.Fatalf("inference from the issuing address: status=%d upstream=%d", status, upstreamHits)
	}
	forward := newForwardProxy(h.store, h.accounts, nil)
	tunnel := func(remote string) (string, int) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodConnect, "example.test:443", nil)
		req.Host = "example.test:443"
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("session:"+issued.Token)))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		forward.ServeHTTP(rec, req)
		_, _, result := forward.authenticate(req)
		return result, rec.Code
	}
	if result, status := tunnel("198.51.100.8:5000"); result != "wrong_source" || status != http.StatusProxyAuthRequired {
		t.Fatalf("CONNECT from another address: result=%s status=%d", result, status)
	}
	if result, _ := tunnel("198.51.100.7:5000"); result != "" {
		t.Fatalf("CONNECT from the issuing address: result=%s", result)
	}
	rows := h.traffic()
	var wrong, ok int
	for _, row := range rows {
		switch row.result {
		case "wrong_source":
			wrong++
			if row.sessionID != "" {
				t.Fatalf("wrong_source row carries the session: %+v", row)
			}
		case "ok":
			ok++
		}
	}
	if wrong != 3 || ok != 1 {
		t.Fatalf("traffic: wrong_source=%d ok=%d rows=%+v", wrong, ok, rows)
	}
}
