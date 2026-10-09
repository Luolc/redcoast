package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// hashOf is what a client machine prints and the operator registers: the SHA-256 of
// the credential in lowercase hex.
func hashOf(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

// TestMachines checks the machine registry and the sessions issued against it: a
// registered machine's credential issues sessions up to its limit; the wrong credential,
// a revoked machine and a machine at its limit are refused; revoking ends the machine's
// live sessions; a second registration of the same name or credential is refused.
func TestMachines(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	schedule(t, store, "sample-a", "pro")
	const credA, credB = "machine-a-synthetic-credential", "machine-b-synthetic-credential"
	if err := store.AddMachine(ctx, "client-a", hashOf(credA), 2); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMachine(ctx, "client-b", hashOf(credB), 0); err != nil {
		t.Fatal(err)
	}
	for _, dup := range []struct{ name, hash string }{{"client-a", hashOf("other")}, {"client-c", hashOf(credA)}} {
		if err := store.AddMachine(ctx, dup.name, dup.hash, 0); !errors.Is(err, ErrMachineExists) {
			t.Fatalf("duplicate %s: %v", dup.name, err)
		}
	}
	if err := store.AddMachine(ctx, "Bad Name", hashOf(credA), 0); !errors.Is(err, ErrMachineShape) {
		t.Fatalf("bad name: %v", err)
	}
	if err := store.AddMachine(ctx, "client-d", "nothex", 0); !errors.Is(err, ErrMachineShape) {
		t.Fatalf("bad hash: %v", err)
	}
	machines, err := store.Machines(ctx)
	if err != nil || len(machines) != 2 || machines[0].Name != "client-a" || machines[0].MaxSessions != 2 || machines[1].MaxSessions != DefaultMaxSessions || !machines[1].RevokedAt.IsZero() {
		t.Fatalf("machines: %+v %v", machines, err)
	}

	// Two sessions for A succeed; the third hits the limit; B's first is unaffected.
	first, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	var limit *MachineLimit
	if _, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil); !errors.As(err, &limit) || limit.Name != "client-a" || limit.MaxSessions != 2 {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := store.IssueForMachine(ctx, credB, "10.0.0.6", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueForMachine(ctx, "not-a-registered-credential", "10.0.0.7", nil); !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("wrong credential: %v", err)
	}
	// Raising the limit lets the third through.
	if err := store.SetMachineLimit(ctx, "client-a", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil); err != nil {
		t.Fatalf("after raising the limit: %v", err)
	}
	if err := store.SetMachineLimit(ctx, "client-z", 3); !errors.Is(err, ErrNoSuchMachine) {
		t.Fatalf("limit of unknown machine: %v", err)
	}

	// The session is bound from its source only.
	if _, err := store.BindFrom(ctx, first.Token, "10.0.0.5", accounts); err != nil {
		t.Fatalf("bind from the issuing address: %v", err)
	}
	for _, source := range []string{"10.0.0.6", LocalSource} {
		if _, err := store.BindFrom(ctx, first.Token, source, accounts); !errors.Is(err, ErrWrongSource) {
			t.Fatalf("bind from %s: %v", source, err)
		}
	}
	// The wrong source did not end the session (control arm).
	if _, err := store.BindFrom(ctx, first.Token, "10.0.0.5", accounts); err != nil {
		t.Fatalf("bind after a wrong-source attempt: %v", err)
	}

	// A slot held by a session that went idle is freed when the machine asks again: at
	// the limit of 3, advancing past the idle expiry lets a fourth through and ends the
	// idle one; without advancing (control arm) the limit holds.
	if _, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil); !errors.As(err, &limit) {
		t.Fatalf("at the limit before the idle expiry: %v", err)
	}
	c.Advance(store.cfg.IdleExpiry)
	fourth, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil)
	if err != nil {
		t.Fatalf("after the idle expiry: %v", err)
	}
	if _, err := store.BindFrom(ctx, first.Token, "10.0.0.5", accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("idle session after the machine's next request: %v", err)
	}
	var idle int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE client_machine = 'client-a' AND end_reason = 'idle'").Scan(&idle); err != nil || idle != 3 {
		t.Fatalf("idle-ended sessions: %d %v", idle, err)
	}
	// The first session had been bound, so it has history; ending it on this path
	// closes that history like every other end does, and the retention pruning then
	// removes the session with it. (Control arm: the fourth, still live, stays.)
	var openHistory int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM binding_history WHERE session_id = ? AND to_ts IS NULL", first.ID).Scan(&openHistory); err != nil || openHistory != 0 {
		t.Fatalf("open history rows of the idle-ended session: %d %v", openHistory, err)
	}
	c.Advance(DefaultSessionRetention + 24*time.Hour)
	if _, err := store.Prune(ctx, Retention{Traffic: DefaultTrafficRetention, Sessions: DefaultSessionRetention}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id IN (?, ?)", first.ID, fourth.ID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("sessions after pruning: %d %v (want only the live one)", remaining, err)
	}
	c.Advance(-(DefaultSessionRetention + 24*time.Hour))

	// Revoking A ends its live session and refuses its credential; B keeps going.
	ended, err := store.RevokeMachine(ctx, "client-a")
	if err != nil || ended != 1 {
		t.Fatalf("revoke: ended=%d err=%v", ended, err)
	}
	if _, err := store.BindFrom(ctx, fourth.Token, "10.0.0.5", accounts); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("bind after revocation: %v", err)
	}
	if _, err := store.IssueForMachine(ctx, credA, "10.0.0.5", nil); !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("issue after revocation: %v", err)
	}
	if _, err := store.IssueForMachine(ctx, credB, "10.0.0.6", nil); err != nil {
		t.Fatalf("B after A's revocation: %v", err)
	}
	if _, err := store.RevokeMachine(ctx, "client-a"); !errors.Is(err, ErrNoSuchMachine) {
		t.Fatal("a second revocation should find nothing to revoke")
	}
	var reason string
	if err := store.db.QueryRow("SELECT end_reason FROM sessions WHERE id = ?", fourth.ID).Scan(&reason); err != nil || reason != "machine_revoked" {
		t.Fatalf("end reason: %q %v", reason, err)
	}
	// The credential itself is nowhere in the file; its hash is (control arm).
	var hits int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM machines WHERE cred_hash = ?", hashOf(credA)).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("hash row: %d %v", hits, err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM machines WHERE cred_hash = ? OR name = ?", credA, credA).Scan(&hits); err != nil || hits != 0 {
		t.Fatalf("credential stored: %d %v", hits, err)
	}
}
