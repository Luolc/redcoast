package claude

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Luolc/redcoast/session"
)

// createBody is the body of POST /accounts. The token reference is not in it: it is
// the configured template with the alias.
type createBody struct {
	Alias  *string    `json:"alias"`
	Email  *string    `json:"email"`
	Access *string    `json:"access"`
	Plan   *string    `json:"plan"`
	Proxy  *proxyBody `json:"proxy"`

	DeviceID      *string `json:"device_id"`
	ReceiverEmail *string `json:"receiver_email"`
}

// patchBody is the body of PATCH /accounts/{alias}: the revision the edit is made
// against and the fields to change, the subscription plan among them. The alias, the access and the token reference are
// not editable, and a status is anything but active, which only activate sets.
type patchBody struct {
	Rev           *int64     `json:"rev"`
	Email         *string    `json:"email"`
	Status        *string    `json:"status"`
	DeviceID      *string    `json:"device_id"`
	AccountUUID   *string    `json:"account_uuid"`
	ReceiverEmail *string    `json:"receiver_email"`
	Plan          *string    `json:"plan"`
	Proxy         *proxyBody `json:"proxy"`
}

// revBody is the body of verify and activate.
type revBody struct {
	Rev *int64 `json:"rev"`
}

// accountRows reads the table.
func (w *AccountWriter) accountRows(ctx context.Context) ([]session.AccountRow, error) {
	rows, _, err := w.cfg.Store.Accounts(ctx)
	if err != nil {
		return nil, errStoreUnavailable
	}
	return rows, nil
}

// storeError maps an error of the store's account writes to the answer.
func storeError(err error) error {
	switch {
	case errors.Is(err, session.ErrAccountNotFound):
		return writeError{status: http.StatusNotFound, message: "no such account", code: errorUnknownAccount}
	case errors.Is(err, session.ErrRevConflict):
		return writeError{status: http.StatusConflict, message: "the account changed since it was read", field: "rev", code: errorRevStale}
	case errors.Is(err, session.ErrAccountExists):
		return writeError{status: http.StatusConflict, message: "the account exists", field: "alias", code: codeAccountExists}
	}
	var we writeError
	if errors.As(err, &we) {
		return we
	}
	return errStoreUnavailable
}

// create stores a new account as testing, with its plan, and serves nothing yet.
func (w *AccountWriter) create(r *http.Request, _ string, body *createBody, entry session.AuditEntry) (any, error) {
	ctx := r.Context()
	if w.cfg.TokenRefTemplate == "" {
		return nil, writeError{status: http.StatusConflict, message: "creating accounts needs dashboard_write.token_ref_template", field: "token_ref", code: codeNoTokenTemplate}
	}
	for _, field := range []struct {
		name string
		set  bool
	}{{"alias", body.Alias != nil}, {"email", body.Email != nil}, {"access", body.Access != nil}, {"plan", body.Plan != nil}, {"proxy", body.Proxy != nil}, {"device_id", body.DeviceID != nil}} {
		if !field.set {
			return nil, fieldProblem(field.name, "missing")
		}
	}
	if !accountAlias.MatchString(*body.Alias) {
		return nil, fieldProblem("alias", "not an account alias")
	}
	if !planShape.MatchString(*body.Plan) {
		return nil, fieldProblem("plan", "not a plan name")
	}
	if !deviceIDShape.MatchString(*body.DeviceID) {
		return nil, fieldProblem("device_id", "not 64 lower-case hex digits")
	}
	row := session.AccountRow{Alias: *body.Alias, Email: *body.Email, Access: *body.Access, Status: "testing", DeviceID: *body.DeviceID,
		TokenRef: strings.Replace(w.cfg.TokenRefTemplate, aliasPlaceholder, *body.Alias, 1)}
	if body.ReceiverEmail != nil {
		row.ReceiverEmail = *body.ReceiverEmail
	}
	body.Proxy.applyProxy(&row)
	rows, err := w.accountRows(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkRow(row, proxyRefOptions(rows), true); err != nil {
		return nil, err
	}
	if err := checkDeviceIDFree(row, rows); err != nil {
		return nil, err
	}
	if checkRowRefs(row) != nil {
		return nil, fieldProblem("alias", "gives a token reference that is not a credential reference")
	}
	now := w.now()
	if _, err := w.cfg.Store.PlanQuotaUnit(ctx, *body.Plan, now); err != nil {
		return nil, fieldProblem("plan", "no such plan")
	}
	created, err := w.cfg.Store.CreateAccount(ctx, row, entry)
	if err != nil {
		return nil, storeError(err)
	}
	if err := w.cfg.Store.SetPlanSchedule(ctx, session.PlanPeriod{Alias: row.Alias, Plan: *body.Plan, From: now, Source: "manual", Note: "set when the account was created on the dashboard"}); err != nil {
		return nil, writeError{status: http.StatusInternalServerError, message: "the account was created but its plan was not set; set it with a plan edit or the plan command", field: "plan", code: codePlanNotSet}
	}
	if err := w.cfg.Store.RecordAudit(ctx, row.Alias, entry, map[string]session.FieldChange{"plan": {Old: "", New: *body.Plan}}); err != nil {
		log.Printf("account write alias=%s: the plan was not audited", row.Alias)
	}
	logWrite(entry, row.Alias, session.AccountRow{}, created)
	config := configOf(created)
	return writeResponse{Account: &config}, nil
}

// checkDeviceIDFree refuses a row whose device ID another account has; an empty one is
// not a value to compare.
func checkDeviceIDFree(row session.AccountRow, rows []session.AccountRow) error {
	if row.DeviceID == "" {
		return nil
	}
	for _, other := range rows {
		if other.Alias != row.Alias && other.DeviceID == row.DeviceID {
			return writeError{status: http.StatusConflict, message: "another account has this device ID", field: "device_id", code: codeFieldInvalid}
		}
	}
	return nil
}

// patched returns before with the fields of body put in. A status of active is refused,
// since only activate sets it, and so is a status that is none.
func patched(before session.AccountRow, body *patchBody) (session.AccountRow, error) {
	after := before
	if body.Email != nil {
		after.Email = *body.Email
	}
	if body.Status != nil {
		switch *body.Status {
		case "testing", "paused", "suspended", "retired", "kyc":
			after.Status = *body.Status
		case "active":
			return after, fieldProblem("status", "use activate")
		default:
			return after, fieldProblem("status", "unknown value")
		}
	}
	if body.DeviceID != nil {
		after.DeviceID = *body.DeviceID
	}
	if body.AccountUUID != nil {
		after.AccountUUID = *body.AccountUUID
	}
	if body.ReceiverEmail != nil {
		after.ReceiverEmail = *body.ReceiverEmail
	}
	body.Proxy.applyProxy(&after)
	return after, nil
}

// update changes the fields of an account at the revision the request names, and
// reloads when the served set can differ. A row that would be served is first loaded
// with its references resolved, so that an edit the reload would refuse is not stored.
func (w *AccountWriter) update(r *http.Request, alias string, body *patchBody, entry session.AuditEntry) (any, error) {
	ctx := r.Context()
	if body.Rev == nil {
		return nil, fieldProblem("rev", "missing")
	}
	before, err := w.cfg.Store.Account(ctx, alias)
	if err != nil {
		return nil, storeError(err)
	}
	if before.Rev != *body.Rev {
		return nil, storeError(session.ErrRevConflict)
	}
	if body.Plan != nil {
		if !planShape.MatchString(*body.Plan) {
			return nil, fieldProblem("plan", "not a plan name")
		}
		if _, err := w.cfg.Store.PlanQuotaUnit(ctx, *body.Plan, w.now()); err != nil {
			return nil, fieldProblem("plan", "no such plan")
		}
	}
	after, err := patched(before, body)
	if err != nil {
		return nil, err
	}
	rows, err := w.accountRows(ctx)
	if err != nil {
		return nil, err
	}
	loginChanged := after.UsernameRef != before.UsernameRef || after.PasswordRef != before.PasswordRef
	if err := checkRow(after, proxyRefOptions(rows), loginChanged); err != nil {
		return nil, err
	}
	if err := checkDeviceIDFree(after, rows); err != nil {
		return nil, err
	}
	if served(after) && servedSettings(before) != servedSettings(after) {
		if err := w.dryLoad(ctx, after); err != nil {
			return nil, err
		}
	}
	saved, err := w.cfg.Store.UpdateAccount(ctx, alias, *body.Rev, entry, func(row *session.AccountRow) error { *row = after; return nil })
	if err != nil {
		return nil, storeError(err)
	}
	logWrite(entry, alias, before, saved)
	planChange, err := w.setPlan(ctx, alias, body.Plan, entry)
	if err != nil {
		return nil, err
	}
	answer, err := w.afterWrite(ctx, before, saved, nil)
	answer.PlanRefresh = planChange
	return answer, err
}

// currentPlan returns the plan of account alias in effect at now, or "".
func (w *AccountWriter) currentPlan(ctx context.Context, alias string, now time.Time) (string, error) {
	periods, err := w.cfg.Store.PlanSchedule(ctx, alias)
	if err != nil {
		return "", err
	}
	for _, p := range periods {
		if !p.From.After(now) && (p.To.IsZero() || p.To.After(now)) {
			return p.Plan, nil
		}
	}
	return "", nil
}

// setPlan puts the account on plan from now on, as the plan command does, through the
// same store write, and audits the change. When the quota unit is higher after it than
// before, the account's quota is read again once, as the plan command does; the word it
// returns says what became of that, empty when no change or no refresh was due. The
// account was saved already, so a failure here says that it was.
func (w *AccountWriter) setPlan(ctx context.Context, alias string, plan *string, entry session.AuditEntry) (string, error) {
	if plan == nil {
		return "", nil
	}
	now := w.now()
	failed := writeError{status: http.StatusInternalServerError, message: "the account was saved but its plan was not; set it with the plan command", field: "plan", code: codePlanNotSet}
	current, err := w.currentPlan(ctx, alias, now)
	if err != nil {
		return "", failed
	}
	if current == *plan {
		return "", nil
	}
	unitBefore, err := w.cfg.Store.QuotaUnit(ctx, alias, now)
	if err != nil && !errors.Is(err, session.ErrNoPlan) {
		return "", failed
	}
	if err := w.cfg.Store.SetPlanSchedule(ctx, session.PlanPeriod{Alias: alias, Plan: *plan, From: now, Source: "manual", Note: "changed on the dashboard"}); err != nil {
		return "", failed
	}
	if err := w.cfg.Store.RecordAudit(ctx, alias, entry, map[string]session.FieldChange{"plan": {Old: current, New: *plan}}); err != nil {
		log.Printf("account write alias=%s: the plan change was not audited", alias)
	}
	log.Printf("account write ip=%s action=%s alias=%s result=ok fields=plan", entry.SourceIP, entry.Action, alias)
	unitAfter, err := w.cfg.Store.QuotaUnit(ctx, alias, now)
	if err != nil || unitAfter <= unitBefore || w.cfg.Accounts.set.Load().lookup(alias) == nil {
		return "", nil
	}
	if _, err := w.cfg.Entrypoint.Refresh(ctx, alias); err != nil {
		log.Printf("account write alias=%s: quota refresh after a plan change: not done", alias)
		return "not refreshed", nil
	}
	return "refreshed", nil
}

// activatable checks the revision, the status and the path of an account that is to be
// activated; with onlyTesting a status other than testing is refused.
func activatable(account session.AccountRow, rev int64, onlyTesting bool) error {
	notReady := func(check, message string) error {
		return writeError{status: http.StatusConflict, message: message, check: check, code: codeNotReady}
	}
	switch {
	case account.Rev != rev:
		return storeError(session.ErrRevConflict)
	case account.Status == "active":
		return notReady("status", "the account is active already")
	case onlyTesting && account.Status != "testing":
		return notReady("status", "only a testing account is activated this way")
	case account.Access != "gateway":
		return notReady("access", "the account is not on the gateway path")
	case account.DeviceID == "":
		return notReady("device_id", "the account has no device ID")
	}
	return nil
}

// verify runs the three checks on an account and stores their result. A body with a
// revision is refused when the account is at another one.
func (w *AccountWriter) verify(r *http.Request, alias string, body *revBody, entry session.AuditEntry) (any, error) {
	ctx := r.Context()
	row, err := w.cfg.Store.Account(ctx, alias)
	if err != nil {
		return nil, storeError(err)
	}
	if body.Rev != nil && *body.Rev != row.Rev {
		return nil, storeError(session.ErrRevConflict)
	}
	result, report, err := w.runChecks(ctx, row)
	if err != nil {
		return nil, err
	}
	saved, err := w.cfg.Store.SetVerification(ctx, alias, row.Rev, result, entry)
	if err != nil {
		return nil, storeError(err)
	}
	logWrite(entry, alias, row, saved)
	config := configOf(saved)
	return writeResponse{Account: &config, Verify: &report}, nil
}

// activate serves an account: at the revision the request names it needs a plan in
// effect and an exit no other served account has, and the three checks, run again now,
// all pass; only then is the row stored as active and the set reloaded. Anything
// else is a 409 that names what failed and changes nothing.
func (w *AccountWriter) activate(r *http.Request, alias string, body *revBody, entry session.AuditEntry, onlyTesting bool) (any, error) {
	ctx := r.Context()
	if body.Rev == nil {
		return nil, fieldProblem("rev", "missing")
	}
	before, err := w.cfg.Store.Account(ctx, alias)
	if err != nil {
		return nil, storeError(err)
	}
	notReady := func(check, message string) error {
		return writeError{status: http.StatusConflict, message: message, check: check, code: codeNotReady}
	}
	if err := activatable(before, *body.Rev, onlyTesting); err != nil {
		return nil, err
	}
	if _, err := w.cfg.Store.QuotaUnit(ctx, alias, w.now()); err != nil {
		return nil, notReady("plan", "the account has no plan in effect")
	}
	rows, err := w.accountRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, other := range rows {
		if other.Alias != alias && served(other) && ((before.Direct && other.Direct) || before.ExpectedIP == other.ExpectedIP) {
			return nil, notReady("exit", "another served account has the same exit")
		}
	}
	result, report, err := w.runChecks(ctx, before)
	if err != nil {
		return nil, err
	}
	for _, check := range []struct {
		name   string
		ok     *bool
		result checkResult
	}{{"token_ok", result.TokenOK, report.TokenOK}, {"egress_ok", result.EgressOK, report.EgressOK}, {"probe_ok", result.ProbeOK, report.ProbeOK}} {
		if check.ok == nil || !*check.ok {
			// What was learned is stored, so that the page shows which check failed.
			if _, err := w.cfg.Store.SetVerification(ctx, alias, before.Rev, result, entry); err != nil {
				return nil, storeError(err)
			}
			return nil, notReady(check.name, fmt.Sprintf("%s did not pass: %s", check.name, check.result.Reason))
		}
	}
	saved, err := w.cfg.Store.UpdateAccount(ctx, alias, before.Rev, entry, func(row *session.AccountRow) error {
		row.Status, row.Verify = "active", result
		return nil
	})
	if err != nil {
		return nil, storeError(err)
	}
	logWrite(entry, alias, before, saved)
	return w.afterWrite(ctx, before, saved, &report)
}
