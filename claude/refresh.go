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

// refreshBody is Claude's Probe body: the smallest request that the upstream answers with every
// anthropic-ratelimit-unified-{5h,7d}-* header: one output token of the small model.
// count_tokens is cheaper but its answer carries none of them, and the usage endpoint
// refuses a setup token for lack of the user:profile scope.
const refreshBody = `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"quota"}]}`

// refreshHeaders are the headers of Claude's Probe besides the account's token, those
// of Claude Code's own requests.
var refreshHeaders = map[string]string{
	"Anthropic-Version": "2023-06-01",
	"Anthropic-Beta":    "claude-code-20250219," + oauthBeta,
	"Content-Type":      "application/json",
	"User-Agent":        "claude-cli/2.1.296 (external, cli)",
	"X-App":             "cli",
}

// errNoSuchAccount is returned by a refresh of an alias outside the current set.
var errNoSuchAccount = errors.New("no such account in the current set")

// errNoProbe is returned by a refresh for a provider without a Probe.
var errNoProbe = errors.New("quota refresh not supported for this provider")

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

// refresh sends the provider's Probe upstream for account alias, regardless of its thresholds
// and of a quota pause, and handles the answer as recordUpstream handles any response, under the
// source refresh. A 2xx answer then ends the account's quota pause if the readings
// allow it, so that the account can be chosen again before its reset. An account
// paused for any other reason, such as a changed exit IP or an operator's pause, gets
// nothing sent with its token: the refresh fails with pausedError.
func (rt *router) refresh(ctx context.Context, alias string) (Refreshed, error) {
	probe := rt.provider.Probe
	if probe == nil {
		return Refreshed{}, errNoProbe
	}
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
	req, err := http.NewRequestWithContext(context.WithValue(ctx, accountKey{}, a), http.MethodPost, rt.upstream.JoinPath(probe.Path).String(), strings.NewReader(probe.Body))
	if err != nil {
		return Refreshed{}, err
	}
	for name, value := range probe.Header {
		req.Header.Set(name, value)
	}
	a.credential.Authorize(req)
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

// RunQuotaRechecks refreshes, every interval until ctx is canceled, each account whose
// stored readings went stale behind a threshold (see router.recheckQuota). A provider
// without a Probe runs nothing. The first round runs one interval after the start, so a
// process that takes over in a handoff only starts once the old one has stopped
// serving, and the two do not poll at the same time.
func (e *Entrypoint) RunQuotaRechecks(ctx context.Context, interval time.Duration) error {
	if e.router.provider.Probe == nil {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.router.recheckQuota(ctx)
		}
	}
}

// recheckQuota refreshes, one after another, each account of the current set that has
// a stored reading at or over the soft threshold, or rejected, whose reset has passed.
// Selection prefers other accounts to such an account, or never chooses it, so no
// response would read its quota again. A quota pause needs no rule of its own: it ends
// by itself at the earliest reset its 429 reported, and a rejected window is rechecked
// at its own reset. An account paused for another reason is skipped without a log
// line, since refresh sends it nothing; a failed refresh is logged and tried again
// next round. A round cut short by shutdown logs nothing for the accounts left.
func (rt *router) recheckQuota(ctx context.Context) {
	soft, _, err := rt.store.Thresholds(ctx)
	if err != nil {
		log.Printf("quota recheck: %v", err)
		return
	}
	for _, alias := range rt.accounts.Candidates() {
		if ctx.Err() != nil {
			return
		}
		readings, err := rt.store.LatestQuota(ctx, alias)
		if err != nil {
			log.Printf("quota recheck alias=%s: %v", alias, err)
			continue
		}
		if !staleBehindThreshold(readings, soft, rt.now()) {
			continue
		}
		refreshed, err := rt.refresh(ctx, alias)
		var paused pausedError
		switch {
		case errors.As(err, &paused):
		case err != nil:
			log.Printf("quota recheck alias=%s: %v", alias, err)
		default:
			log.Printf("quota recheck alias=%s upstream_status=%d pause_ended=%t", alias, refreshed.UpstreamStatus, refreshed.PauseEnded)
		}
	}
}

// staleBehindThreshold reports whether a window of readings is at or over soft, or
// rejected, and has a reset that is not after now.
func staleBehindThreshold(readings map[string]session.Quota, soft float64, now time.Time) bool {
	for _, q := range readings {
		if (q.Utilization >= soft || q.Status == "rejected") && !q.ResetAt.IsZero() && !q.ResetAt.After(now) {
			return true
		}
	}
	return false
}
