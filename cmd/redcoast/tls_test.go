package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/internal/testtls"
	"github.com/Luolc/redcoast/session"
	"github.com/cloudflare/tableflip"
)

// writeTLSFiles writes the certificate and key of certs into a new directory and
// returns their paths.
func writeTLSFiles(t *testing.T, certs testtls.Server) (cert, key string) {
	t.Helper()
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, certs.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, certs.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// TestTLSConfiguration checks listen.tls: a certificate and key that pair and cover
// server_name turn on TLS and the https entrypoints; files that do not pair, cannot be
// read or leave server_name out are refused, naming the key. A public listener without
// TLS starts with a warning; with TLS, or on a private address, there is none.
func TestTLSConfiguration(t *testing.T) {
	cert, key := writeTLSFiles(t, testtls.New(t))
	_, otherKey := writeTLSFiles(t, testtls.New(t))
	tlsYAML := func(cert, key, name string) string {
		return "  tls: {cert_file: '" + cert + "', key_file: '" + key + "', server_name: '" + name + "'}\n"
	}
	for _, test := range []struct {
		name, listen string
		problem      string // part of the error when refused
		warning      string // part of the warning when accepted; empty for none
	}{
		{"tls", tlsYAML(cert, key, testtls.ServerName), "", ""},
		{"not_paired", tlsYAML(cert, otherKey, testtls.ServerName), "listen.tls: cert_file and key_file: tls: private key does not match public key", ""},
		{"key_is_cert", tlsYAML(cert, cert, testtls.ServerName), "listen.tls: cert_file and key_file:", ""},
		{"unreadable_cert", tlsYAML(cert+".missing", key, testtls.ServerName), "listen.tls.cert_file: open ", ""},
		{"no_key", tlsYAML(cert, "", testtls.ServerName), "listen.tls.key_file: missing", ""},
		{"no_server_name", tlsYAML(cert, key, ""), "listen.tls.server_name: missing", ""},
		{"other_server_name", tlsYAML(cert, key, "other.example.test"), "listen.tls.server_name: not covered", ""},
		{"public_plain", "  session: '203.0.113.7:7801'\n  allow_public: true\n", "", "listen.session on a public IP without listen.tls"},
		{"public_tls", "  session: '203.0.113.7:7801'\n  allow_public: true\n" + tlsYAML(cert, key, testtls.ServerName), "", ""},
		{"private_plain", "  session: '10.0.0.7:7801'\n", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(baseConfig + "listen:\n" + test.listen))
			if test.problem != "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want an error with %q", err, test.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.warning == "" && cfg.warning != "" || !strings.Contains(cfg.warning, test.warning) {
				t.Fatalf("warning %q, want %q", cfg.warning, test.warning)
			}
			want := session.Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}
			if strings.Contains(test.listen, "tls:") {
				want = session.Entrypoints{ReverseProxy: "https://gateway.example.test:8789", ForwardProxy: "gateway.example.test:8791"}
			}
			if got := entrypoints(cfg); got != want || (cfg.tls != nil) != strings.Contains(test.listen, "tls:") {
				t.Fatalf("entrypoints %+v (TLS %v), want %+v", got, cfg.tls != nil, want)
			}
		})
	}
}

// TestListenTLS opens the listeners with TLS: every TCP listener completes a handshake
// with a client that trusts the test CA and agrees on http/1.1, and the session
// interface answers over it. A client without the CA fails (control arm).
func TestListenTLS(t *testing.T) {
	certs := testtls.New(t)
	cert, key := writeTLSFiles(t, certs)
	dir := t.TempDir()
	cfg, err := parseConfig([]byte(baseConfig + "listen:\n  reverse: 127.0.0.1:0\n  forward: 127.0.0.2:0\n  health: 127.0.0.3:0\n  dashboard: 127.0.0.4:0\n  session: 127.0.0.5:0\n" +
		"  tls: {cert_file: '" + cert + "', key_file: '" + key + "', server_name: " + testtls.ServerName + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.socket, cfg.adminSocket = filepath.Join(dir, "session.sock"), filepath.Join(dir, "admin.sock")
	upg, err := tableflip.New(tableflip.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upg.Stop)
	l, err := listen(upg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.Context(), filepath.Join(dir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- session.NewServer(store, entrypoints(cfg), func() []string { return nil }).ServeTCPListener(ctx, l.sessionTCP)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	trusted := &tls.Config{RootCAs: certs.Roots, ServerName: testtls.ServerName, NextProtos: []string{"h2", "http/1.1"}}
	for name, listener := range map[string]net.Listener{"reverse": l.reverse, "forward": l.forward, "health": l.health, "dashboard": l.dashboard} {
		// The server side of the handshake runs when the accepted connection is first used.
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if conn, ok := conn.(*tls.Conn); ok {
				_ = conn.Handshake()
			}
			_ = conn.Close()
		}()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", listener.Addr().String(), trusted)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
			t.Fatalf("%s negotiated %q", name, got)
		}
		_ = conn.Close()
	}
	post := func(config *tls.Config) (int, string, error) {
		transport := &http.Transport{TLSClientConfig: config}
		defer transport.CloseIdleConnections()
		res, err := (&http.Client{Timeout: 5 * time.Second, Transport: transport}).Post("https://"+l.sessionTCP.Addr().String()+"/sessions", "application/json", strings.NewReader("{}"))
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		return res.StatusCode, string(body), err
	}
	if status, body, err := post(trusted); err != nil || status != http.StatusUnauthorized || !strings.Contains(body, `"unknown_machine"`) {
		t.Fatalf("session interface over TLS: %d %q %v", status, body, err)
	}
	if _, _, err := post(&tls.Config{ServerName: testtls.ServerName}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a client without the CA got through: %v", err)
	}
}
