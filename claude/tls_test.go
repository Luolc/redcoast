package claude

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/internal/testtls"
)

// serveTLS serves handler with Serve on a loopback listener wrapped in ServerTLS and
// returns its address.
func serveTLS(t *testing.T, cert tls.Certificate, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, tls.NewListener(listener, ServerTLS(cert)), handler) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String()
}

// TestEntrypointsOverTLS serves both entrypoints over TLS with a test CA: an inference
// request reaches the account's exit, and a CONNECT without a credential gets 407 with
// the Basic challenge git waits for, after which a CONNECT with it opens a tunnel. A
// client that does not trust the CA fails (control arm); one that offers h2 and
// http/1.1 gets http/1.1, and one that offers only h2 fails the handshake while the
// listener keeps serving.
func TestEntrypointsOverTLS(t *testing.T) {
	certs := testtls.New(t)
	trusted := &tls.Config{RootCAs: certs.Roots, ServerName: testtls.ServerName}
	// One harness per entrypoint: the reverse proxy's exit answers requests, the forward
	// proxy's echoes tunnels.
	h := newHarness(t)
	h.addWithExit("sample-a", "sk-ant-oat01-synthetic-token-a", startAccountExit(t, "A", 200).url(t))
	h.token, h.sessionID = h.issue()
	reverse := serveTLS(t, certs.Certificate, h.router(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}, accountTransport{}))
	hf := newHarness(t)
	hf.addWithExit("sample-b", "sk-ant-oat01-synthetic-token-b", &url.URL{Scheme: "http", Host: startFakeExit(t, 200).listener.Addr().String()})
	hf.token, hf.sessionID = hf.issue()
	forward := serveTLS(t, certs.Certificate, newForwardProxy(hf.store, hf.accounts, nil))

	request := func(roots *tls.Config) (string, error) {
		transport := &http.Transport{TLSClientConfig: roots}
		defer transport.CloseIdleConnections()
		client := &http.Client{Timeout: 5 * time.Second, Transport: authorizing{h.token, transport}}
		res, err := client.Post("https://"+reverse+"/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		return string(body), err
	}
	if body, err := request(trusted); err != nil || body != `{"exit":"A"}` {
		t.Fatalf("inference over TLS: body=%q err=%v", body, err)
	}
	if _, err := request(&tls.Config{ServerName: testtls.ServerName}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a client without the CA got through: %v", err)
	}

	dial := func(protocols ...string) (*tls.Conn, error) {
		config := trusted.Clone()
		config.NextProtos = protocols
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", forward, config)
	}
	conn, err := dial("h2", "http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("negotiated %q, want http/1.1", got)
	}
	_, _, response := connectOn(t, conn, "api.example.test:443", "")
	if response.StatusCode != http.StatusProxyAuthRequired || !strings.HasPrefix(response.Header.Get("Proxy-Authenticate"), "Basic") {
		t.Fatalf("CONNECT without a credential: %d %q", response.StatusCode, response.Header.Get("Proxy-Authenticate"))
	}
	if conn, err := dial("h2"); err == nil {
		closeQuietly(conn)
		t.Fatalf("an h2-only client negotiated %q", conn.ConnectionState().NegotiatedProtocol)
	}
	if conn, err = dial("http/1.1"); err != nil {
		t.Fatal(err)
	}
	conn2, reader, response := connectOn(t, conn, "api.example.test:443", proxyAuthorization(hf.token))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT with the credential: %d", response.StatusCode)
	}
	if _, err := io.WriteString(conn2, "ping"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatal("tunnel over TLS did not carry bytes both ways")
	}
	closeQuietly(conn2)
}
