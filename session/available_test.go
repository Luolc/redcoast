package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAvailable checks that the pre-issue check gives the same verdicts as Bind: nil
// with an account to choose, QuotaExhausted with the earliest recovery when every
// candidate is out for a quota reason, ErrNoAccount when one is out for another reason
// or there is none; and that it issues or binds nothing.
func TestAvailable(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	ctx := context.Background()
	if err := store.Available(ctx, accounts); err != nil {
		t.Fatalf("open accounts: %v", err)
	}
	if err := store.Available(ctx, nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("no candidates: %v", err)
	}
	reset := c.Now().Add(2 * time.Hour)
	read(t, store, "sample-a", Window7d, 0.95, "allowed_warning", c.Now().Add(3*time.Hour))
	pause(t, store, "sample-b", reset, QuotaPauseReason)
	var exhausted *QuotaExhausted
	if err := store.Available(ctx, accounts); !errors.As(err, &exhausted) || !exhausted.ResetAt.Equal(reset) {
		t.Fatalf("all out for quota: %v", err)
	}
	pause(t, store, "sample-b", c.Now().Add(24*time.Hour), "upstream_401")
	if err := store.Available(ctx, accounts); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("one out for a 401: %v", err)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions") + count(t, store, "SELECT COUNT(*) FROM bindings"); got != 0 {
		t.Fatalf("the check wrote %d rows", got)
	}
}

// TestIssueRefusedWhenUnavailable checks POST /sessions against an unavailable set of
// accounts: every account over hard gives 429 with a quota_exhausted body naming the
// gateway and the earliest recovery; an account refused by the upstream gives 503 with
// a no_account body; neither issues a session. Control arm: after Resume the same
// request is granted.
func TestIssueRefusedWhenUnavailable(t *testing.T) {
	c := newClock()
	store, _ := open(t, c)
	socket, _, _ := startUnix(t, NewServer(store, Entrypoints{ReverseProxy: "http://127.0.0.1:8789", ForwardProxy: "127.0.0.1:8791"}, func() []string { return accounts }))
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	post := func() (int, Refusal, string) {
		t.Helper()
		resp, err := client.Post("http://session/sessions", "application/json", strings.NewReader(`{"client_machine":"machine-a"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var refused Refusal
		_ = json.Unmarshal(body, &refused)
		return resp.StatusCode, refused, string(body)
	}
	reset := c.Now().Add(90 * time.Minute)
	read(t, store, "sample-a", Window5h, 0.9, "allowed_warning", reset)
	read(t, store, "sample-b", Window7d, 0.95, "allowed_warning", c.Now().Add(24*time.Hour))
	status, refused, body := post()
	if status != http.StatusTooManyRequests || refused.Code != "quota_exhausted" || !strings.HasPrefix(refused.Message, "redcoast: local gateway quota exhausted") || !refused.ResetAt.Equal(reset) || !strings.Contains(refused.Message, reset.Format(time.RFC3339)) {
		t.Fatalf("all over hard: status=%d body=%s", status, body)
	}
	pause(t, store, "sample-a", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "upstream_401")
	status, refused, body = post()
	if status != http.StatusServiceUnavailable || refused.Code != "no_account" || !strings.HasPrefix(refused.Message, "redcoast: no account available") || !refused.ResetAt.IsZero() {
		t.Fatalf("refused token: status=%d body=%s", status, body)
	}
	if got := count(t, store, "SELECT COUNT(*) FROM sessions"); got != 0 {
		t.Fatalf("%d sessions issued by refused requests", got)
	}
	// Control arm: A resumed and under hard again, B still over hard: granted on A.
	if err := store.Resume(context.Background(), "sample-a"); err != nil {
		t.Fatal(err)
	}
	read(t, store, "sample-a", Window5h, 0.5, "allowed", reset)
	status, _, body = post()
	if status != http.StatusCreated || !strings.Contains(body, `"session_id"`) {
		t.Fatalf("control arm: status=%d body=%s", status, body)
	}
}
