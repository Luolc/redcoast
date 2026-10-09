package session

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/internal/testtls"
)

// TestClient checks the client against the in-process interface: an issued grant has
// the credential and both entrypoints and the credential is accepted by Bind; revoking
// it makes Bind refuse it and a second revoke reports an unknown session; a missing
// socket reports ErrSocketUnavailable and issues nothing.
func TestClient(t *testing.T) {
	store, _ := open(t, newClock())
	entrypoints := Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}
	socket, _, _ := startUnix(t, NewServer(store, entrypoints, func() []string { return accounts }))
	client := &Client{Socket: socket}
	ctx := context.Background()
	grant, err := client.Issue(ctx, "machine-a", []byte(`{"pid": 7}`))
	if err != nil || grant.Entrypoints != entrypoints || grant.SessionID == "" {
		t.Fatalf("issue returned %+v, %v", grant, err)
	}
	if b, err := store.Bind(ctx, grant.Token, accounts); err != nil || b.SessionID != grant.SessionID {
		t.Fatalf("issued credential not accepted: %+v %v", b, err)
	}
	if err := client.Revoke(ctx, grant.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, grant.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("got %v after revoke, want ErrUnknownSession", err)
	}
	if err := client.Revoke(ctx, grant.Token); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("second revoke got %v, want ErrUnknownSession", err)
	}
	if _, err := client.Issue(ctx, "machine-a", nil); err != nil {
		t.Fatalf("control arm: issue over the live socket failed: %v", err)
	}
	absent := &Client{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if _, err := absent.Issue(ctx, "machine-a", nil); !errors.Is(err, ErrSocketUnavailable) {
		t.Fatalf("got %v with no socket, want ErrSocketUnavailable", err)
	}
	if err := absent.Revoke(ctx, grant.Token); !errors.Is(err, ErrSocketUnavailable) {
		t.Fatalf("got %v with no socket, want ErrSocketUnavailable", err)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Fatalf("%d sessions stored, want 2", got)
	}
	// A gateway with every account over hard refuses with a *Refusal the caller can
	// inspect; the credential-free message is the gateway's own.
	reset := store.cfg.Now().Add(time.Hour)
	for _, alias := range accounts {
		read(t, store, alias, Window7d, 0.95, "allowed_warning", reset)
	}
	var refused *Refusal
	if _, err := client.Issue(ctx, "machine-a", nil); !errors.As(err, &refused) || refused.Code != "quota_exhausted" || !refused.ResetAt.Equal(reset) || !strings.HasPrefix(err.Error(), "redcoast:") {
		t.Fatalf("got %v with every account over hard, want a quota_exhausted Refusal", err)
	}
}

// TestClientOverTLS issues and revokes a session over an https address with a client
// that trusts the test CA. Control arm: a client checking against the system's CAs
// fails before any session is issued.
func TestClientOverTLS(t *testing.T) {
	certs := testtls.New(t)
	store, _ := open(t, newClock())
	ctx := context.Background()
	schedule(t, store, "sample-a", "pro")
	if err := store.AddMachine(ctx, "client-a", hashOf("machine-a-synthetic-credential"), 0); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, Entrypoints{ReverseProxy: "https://gateway.example.test:8789", ForwardProxy: "gateway.example.test:8791"}, func() []string { return accounts })
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- server.ServeTCPListener(serveCtx, tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certs.Certificate}}))
	}()
	t.Cleanup(func() { cancel(); <-done })
	address := "https://" + listener.Addr().String()
	untrusted := &Client{Address: address, Credential: "machine-a-synthetic-credential"}
	if _, err := untrusted.Issue(ctx, "", nil); !errors.Is(err, ErrSocketUnavailable) || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a client without the CA: %v", err)
	}
	client := &Client{Address: address, Credential: "machine-a-synthetic-credential", roots: certs.Roots}
	grant, err := client.Issue(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if grant.ReverseProxy != "https://gateway.example.test:8789" {
		t.Fatalf("grant %+v", grant.Entrypoints)
	}
	var sessions int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions issued: %d %v", sessions, err)
	}
	if err := client.Revoke(ctx, grant.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, grant.Token, accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("after revocation: %v", err)
	}
}
