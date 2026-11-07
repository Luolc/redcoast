package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// probeWait is how long the probe of a check waits for a quota round in progress, which
// holds the lock that lets one probing round run at a time.
var probeWait = 30 * time.Second

// verifyTimeout bounds the three checks of one account together.
const verifyTimeout = 2 * time.Minute

// checkResult is the outcome of one check: pass, fail, or skipped when an earlier
// check failed. Reason is fixed words, never what the resolver or the exit said.
type checkResult struct {
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// verifyReport is what a verify answers: each check's result.
type verifyReport struct {
	TokenOK  checkResult `json:"token_ok"`
	EgressOK checkResult `json:"egress_ok"`
	ProbeOK  checkResult `json:"probe_ok"`
}

func pass() checkResult                 { return checkResult{Result: "pass"} }
func fail(reason string) checkResult    { return checkResult{Result: "fail", Reason: reason} }
func skipped(reason string) checkResult { return checkResult{Result: "skipped", Reason: reason} }

// runChecks builds the account of row in memory, outside the served set, and runs the
// three checks in order: its token reference resolves and gives a header value, its exit
// reads the expected IP and reaches the upstream (the egress check, through the
// account's own exit), and the quota probe the refresh sends gets a 2xx answer through
// that exit. A check after a failed one is skipped. One check of an alias runs at a
// time (409 otherwise), and the probe takes the lock of the quota rounds.
func (w *AccountWriter) runChecks(ctx context.Context, row session.AccountRow) (session.Verification, verifyReport, error) {
	if row.Access != "gateway" {
		return session.Verification{}, verifyReport{}, writeError{status: http.StatusConflict, message: "the account is not on the gateway path", check: "access"}
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	var report verifyReport
	var v session.Verification
	set := func(ok *bool, target *checkResult, result checkResult) {
		*ok, *target = result.Result == "pass", result
	}
	var tokenOK, egressOK, probeOK bool
	file, data, err := rowDocument(row)
	if err != nil {
		return v, report, writeError{status: http.StatusInternalServerError, message: "the account cannot be described"}
	}
	entry, err := servedAccount(row.Alias, file, data, Claude)
	if err != nil {
		// A row that is no valid account fails every check by the field that is wrong.
		reason := "the account's settings are not valid: " + strings.Join(fieldsOf(err), ", ")
		set(&tokenOK, &report.TokenOK, fail(reason))
		report.EgressOK, report.ProbeOK = skipped(reason), skipped(reason)
		v.TokenOK, v.At = &tokenOK, w.now().UTC()
		return v, report, nil
	}
	resolve, err := w.cfg.Resolve(ctx)
	if err != nil {
		return v, report, writeError{status: http.StatusBadGateway, message: "the credential resolver is not available"}
	}
	candidate, tokenResult, exitResult := w.candidate(ctx, entry, resolve)
	set(&tokenOK, &report.TokenOK, tokenResult)
	v.TokenOK = &tokenOK
	if exitResult.Result == "fail" {
		set(&egressOK, &report.EgressOK, exitResult)
	} else {
		set(&egressOK, &report.EgressOK, w.egressResult(ctx, candidate))
	}
	v.EgressOK = &egressOK
	if !tokenOK || !egressOK {
		reason := "an earlier check failed"
		report.ProbeOK = skipped(reason)
	} else {
		set(&probeOK, &report.ProbeOK, w.probeResult(ctx, candidate))
		v.ProbeOK = &probeOK
	}
	v.At = w.now().UTC()
	return v, report, nil
}

// candidate builds the account of entry outside the served set. tokenResult says
// whether its token reference resolved; exitResult is a failure when the exit's
// references did not, and the account is nil then.
func (w *AccountWriter) candidate(ctx context.Context, entry inventoryAccount, resolve Resolver) (a *account, tokenResult, exitResult checkResult) {
	in := accountInput{Email: entry.email, Direct: entry.direct, ProxyURL: entry.exit, ExpectedIP: entry.expectedIP}
	source, err := entry.credential(ctx, resolve)
	tokenResult = pass()
	if err != nil {
		tokenResult = fail("the token reference does not resolve to a usable token")
	}
	in.Credential = source
	if !entry.direct {
		for _, ref := range []struct {
			reference string
			value     *string
		}{{entry.usernameRef, &in.ProxyUsername}, {entry.passwordRef, &in.ProxyPassword}} {
			value, err := resolve(ctx, ref.reference)
			if err != nil {
				return nil, tokenResult, fail("a proxy login reference does not resolve")
			}
			*ref.value = value
		}
	}
	a, err = newAccount(entry.alias, in, w.cfg.Accounts.set.Load().concurrency)
	if err != nil {
		return nil, tokenResult, fail("the exit cannot be built from the proxy settings")
	}
	return a, tokenResult, pass()
}

// egressResult runs the egress check on the candidate and words the outcome.
func (w *AccountWriter) egressResult(ctx context.Context, a *account) checkResult {
	if a == nil {
		return fail("the exit cannot be built")
	}
	defer w.cfg.Egress.forget(a)
	err := w.cfg.Egress.Check(ctx, a)
	last := w.cfg.Egress.lastCheck(a)
	switch {
	case errors.Is(err, errEgressMismatch):
		return fail("the exit IP differs from the expected one")
	case err != nil:
		return fail("the exit IP could not be read")
	case last.Upstream == "refused":
		return fail(fmt.Sprintf("the exit cannot reach the upstream (status %d)", last.UpstreamStatus))
	case last.Upstream != "ok":
		return fail("the exit did not answer for the upstream")
	}
	return pass()
}

// probeResult sends the quota probe through the candidate's exit and words the outcome.
func (w *AccountWriter) probeResult(ctx context.Context, a *account) checkResult {
	status, err := w.cfg.Entrypoint.probeCandidate(ctx, a)
	switch {
	case errors.Is(err, errProbeBusy):
		return fail("a quota refresh was running; try again")
	case err != nil:
		return fail("the upstream was not reached through the account's exit")
	case status/100 != 2:
		return fail(fmt.Sprintf("the upstream answered %d", status))
	}
	return pass()
}

// errProbeBusy is returned when the lock of the quota rounds is not free in time.
var errProbeBusy = errors.New("quota round in progress")

// probeCandidate sends the provider's Probe for an account outside the served set,
// through that account's exit, and returns the upstream's status. It holds the lock of
// the quota rounds, so no refresh round probes at the same moment, and records no
// reading, pause or traffic row: the account is not served yet.
func (e *Entrypoint) probeCandidate(ctx context.Context, a *account) (int, error) {
	probe := e.router.provider.Probe
	if probe == nil {
		return 0, errNoProbe
	}
	waitCtx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	if !e.router.rounds.acquire(waitCtx) {
		return 0, errProbeBusy
	}
	defer e.router.rounds.release()
	ctx, cancel = context.WithTimeout(ctx, RefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(withAccount(ctx, a, e.router.now()), http.MethodPost, e.router.upstream.JoinPath(probe.Path).String(), strings.NewReader(probe.Body))
	if err != nil {
		return 0, err
	}
	for name, value := range probe.Header {
		req.Header.Set(name, value)
	}
	a.credential.Authorize(req)
	res, err := accountTransport{}.RoundTrip(req)
	if err != nil {
		return 0, errors.New("upstream not reached through the account's exit")
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxErrorBody))
	return res.StatusCode, nil
}
