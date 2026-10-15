package claude

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// RefreshTimeout bounds one refresh: the request through the account's exit and the
// writes after it.
const RefreshTimeout = 30 * time.Second

// refreshBody is the smallest request that the upstream answers with every
// anthropic-ratelimit-unified-{5h,7d}-* header: one output token of the small model.
// count_tokens is cheaper but its answer carries none of them, and the usage endpoint
// refuses a setup token for lack of the user:profile scope.
const refreshBody = `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"quota"}]}`

// refreshHeaders are the headers of the refresh request besides the account's token,
// those of Claude Code's own requests.
var refreshHeaders = map[string]string{
	"Anthropic-Version": "2023-06-01",
	"Anthropic-Beta":    "claude-code-20250219," + oauthBeta,
	"Content-Type":      "application/json",
	"User-Agent":        "claude-cli/2.1.296 (external, cli)",
	"X-App":             "cli",
}

// errNoSuchAccount is returned by a refresh of an alias outside the current set.
var errNoSuchAccount = errors.New("no such account in the current set")

// pausedError is returned by a refresh of an account paused for a reason other than
// quota; nothing is sent.
type pausedError struct{ reason string }

func (e pausedError) Error() string { return "account paused: " + e.reason }

// Refreshed is the outcome of one refresh: the upstream's status, the account's stored
// readings after it and whether its quota pause ended.
type Refreshed struct {
	Alias          string                   `json:"alias"`
	UpstreamStatus int                      `json:"upstream_status"`
	Quota          map[string]session.Quota `json:"quota"`
	PauseEnded     bool                     `json:"pause_ended"`
}

// Refresh reads the quota of account alias again with one request through the
// account's exit, as for the reverse proxy. See router.refresh.
func (e *Entrypoint) Refresh(ctx context.Context, alias string) (Refreshed, error) {
	return e.router.refresh(ctx, alias)
}

// refresh sends refreshBody upstream for account alias, regardless of its thresholds
// and of a quota pause, and handles the answer as recordUpstream handles any response, under the
// source refresh. A 2xx answer then ends the account's quota pause if the readings
// allow it, so that the account can be chosen again before its reset. An account
// paused for any other reason, such as a changed exit IP or an operator's pause, gets
// nothing sent with its token: the refresh fails with pausedError.
func (rt *router) refresh(ctx context.Context, alias string) (Refreshed, error) {
	a := rt.accounts.set.Load().lookup(alias)
	if a == nil {
		return Refreshed{}, errNoSuchAccount
	}
	ctx, cancel := context.WithTimeout(ctx, RefreshTimeout)
	defer cancel()
	reason, err := rt.store.PauseReason(ctx, alias)
	switch {
	case err != nil:
		return Refreshed{}, errors.New("session store unavailable")
	case reason != "" && reason != session.QuotaPauseReason:
		return Refreshed{}, pausedError{reason}
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, accountKey{}, a), http.MethodPost, rt.upstream.JoinPath("v1", "messages").String(), strings.NewReader(refreshBody))
	if err != nil {
		return Refreshed{}, err
	}
	for name, value := range refreshHeaders {
		req.Header.Set(name, value)
	}
	a.replaceClientAuthentication(req.Header)
	res, err := accountTransport{}.RoundTrip(req)
	if err != nil {
		// The error can name the exit; only the alias is logged.
		log.Printf("quota refresh failed alias=%s", alias)
		return Refreshed{}, errors.New("upstream not reached through the account's exit")
	}
	defer func() { _ = res.Body.Close() }()
	rt.recordUpstream(res, "refresh")
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxErrorBody))
	out := Refreshed{Alias: alias, UpstreamStatus: res.StatusCode}
	if res.StatusCode/100 == 2 {
		if out.PauseEnded, err = rt.store.EndQuotaPause(ctx, alias); err != nil {
			return Refreshed{}, errors.New("session store unavailable")
		}
	}
	if out.Quota, err = rt.store.LatestQuota(ctx, alias); err != nil {
		return Refreshed{}, errors.New("session store unavailable")
	}
	return out, nil
}
