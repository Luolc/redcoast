package claude

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"

	"github.com/Luolc/redcoast/session"
)

// The fixed codes the management socket's account commands answer with, besides those of
// the setup commands.
const (
	codeNotReady            = "not_ready"
	codeDescribeFailed      = "describe_failed"
	codeResolverUnavailable = "resolver_unavailable"
	codeReferenceUnresolved = "reference_unresolved"
	codeAccountExists       = "account_exists"
	codeNoTokenTemplate     = "no_token_template"
	codePlanNotSet          = "plan_not_set"
	codeFieldInvalid        = "field_invalid"
	codeFieldMissing        = "field_missing"
	codeSourceMissing       = "source_missing"
	codeSourceInvalid       = "source_invalid"
	codeWriteUnavailable    = "account_write_unavailable"
)

// errStoreUnavailable is the answer when the session store cannot be read or written.
var errStoreUnavailable = writeError{status: http.StatusInternalServerError, message: "session store unavailable", code: errorStoreUnavailable}

// adminSourceIP is the source address the audit records for a request through the
// management socket.
const adminSourceIP = "admin-socket"

// maxHistory bounds the audit rows `account show` gives.
const maxHistory = 100

// The shapes of a Slack thread key and message ts: only the format is checked, not that
// the message exists, and the channel ID's first letter is not looked at.
var (
	sourceThreadShape = regexp.MustCompile(`^[A-Z0-9]{8,12}/[0-9]{10}\.[0-9]{6}$`)
	sourceTSShape     = regexp.MustCompile(`^[0-9]{10}\.[0-9]{6}$`)
)

// sourceBody is the source of a write through the management socket, as the command
// sends it. The kind is not a field of the request: the command fixes it.
type sourceBody struct {
	Kind   string `json:"kind"`
	Thread string `json:"thread"`
	TS     string `json:"ts"`
}

// adminCreateBody is the body of POST /accounts of the management socket. There is no
// access and no direct key: such an account is on the gateway path, behind a proxy.
type adminCreateBody struct {
	Alias  *string         `json:"alias"`
	Email  *string         `json:"email"`
	Plan   *string         `json:"plan"`
	Proxy  *adminProxyBody `json:"proxy"`
	Source *sourceBody     `json:"source"`
}

// adminProxyBody is the proxy of an adminCreateBody.
type adminProxyBody struct {
	Host        *string `json:"host"`
	Port        *int    `json:"port"`
	ExpectedIP  *string `json:"expected_egress_ip"`
	UsernameRef *string `json:"username_ref"`
	PasswordRef *string `json:"password_ref"`
}

// adminRevBody is the body of verify, activate and setup request of the management socket.
type adminRevBody struct {
	Rev    *int64      `json:"rev"`
	Source *sourceBody `json:"source"`
}

// checkSource validates the source of a request and returns it as the store records it.
func checkSource(body *sourceBody) (session.Source, error) {
	if body == nil {
		return session.Source{}, writeError{status: http.StatusBadRequest, message: "source missing", field: "source", code: codeSourceMissing}
	}
	if body.Kind != session.SourceSlack {
		return session.Source{}, writeError{status: http.StatusBadRequest, message: "source kind", field: "source.kind", code: codeSourceInvalid}
	}
	if !sourceThreadShape.MatchString(body.Thread) {
		return session.Source{}, writeError{status: http.StatusBadRequest, message: "source thread", field: "source.thread", code: codeSourceInvalid}
	}
	if !sourceTSShape.MatchString(body.TS) {
		return session.Source{}, writeError{status: http.StatusBadRequest, message: "source ts", field: "source.ts", code: codeSourceInvalid}
	}
	return session.Source{Kind: body.Kind, Thread: body.Thread, TS: body.TS}, nil
}

// handleAccountWrites registers the account commands of the management socket.
func (a *Admin) handleAccountWrites(mux *http.ServeMux) {
	mux.HandleFunc("POST /accounts", adminWrite(a, actionCreate, func(w *AccountWriter, r *http.Request, _ string, body *adminCreateBody, entry session.AuditEntry) (any, error) {
		return w.create(r, "", body.toCreate(), entry)
	}, func(b *adminCreateBody) *sourceBody { return b.Source }, false))
	mux.HandleFunc("POST /accounts/{alias}/verify", adminWrite(a, actionVerify, func(w *AccountWriter, r *http.Request, alias string, body *adminRevBody, entry session.AuditEntry) (any, error) {
		return w.verify(r, alias, &revBody{Rev: body.Rev}, entry)
	}, func(b *adminRevBody) *sourceBody { return b.Source }, false))
	mux.HandleFunc("POST /accounts/{alias}/activate", adminWrite(a, actionActivate, func(w *AccountWriter, r *http.Request, alias string, body *adminRevBody, entry session.AuditEntry) (any, error) {
		return w.activate(r, alias, &revBody{Rev: body.Rev}, entry, true)
	}, func(b *adminRevBody) *sourceBody { return b.Source }, false))
	mux.HandleFunc("POST /accounts/{alias}/setup/request", adminWrite(a, actionSetup, func(w *AccountWriter, r *http.Request, alias string, body *adminRevBody, entry session.AuditEntry) (any, error) {
		action := actionLoginAndToken
		answer, err := w.setupRequest(r, alias, &setupBody{Action: &action, Rev: body.Rev}, entry)
		if done, ok := answer.(accepted); ok {
			return done.body, err
		}
		return answer, err
	}, func(b *adminRevBody) *sourceBody { return b.Source }, true))
	mux.HandleFunc("GET /accounts/{alias}", a.serveAccountShow)
}

// toCreate returns the body of a dashboard create that b stands for: an account on the
// gateway path behind the proxy b names.
func (b *adminCreateBody) toCreate() *createBody {
	access := "gateway"
	out := &createBody{Alias: b.Alias, Email: b.Email, Plan: b.Plan, Access: &access}
	if b.Proxy != nil {
		direct := false
		out.Proxy = &proxyBody{Direct: &direct, Host: b.Proxy.Host, Port: b.Proxy.Port, ExpectedIP: b.Proxy.ExpectedIP,
			UsernameRef: b.Proxy.UsernameRef, PasswordRef: b.Proxy.PasswordRef}
	}
	return out
}

// adminWrite returns the handler of one write command of the management socket. The
// writer must exist and admin writes be on (501 account_write_unavailable, checked
// first), and for a setup request setup must be on (501 setup_unavailable). Then the body
// must be JSON of at most 8 KiB with no unknown or repeated key, and carry a source in
// the form of a Slack message. The write runs under the writer's lock, as the
// dashboard's does, and is audited with the source.
func adminWrite[B any](a *Admin, action string, handle func(w *AccountWriter, r *http.Request, alias string, body *B, entry session.AuditEntry) (any, error), sourceOf func(*B) *sourceBody, needsSetup bool) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		w := a.Writer
		switch {
		case w == nil || !w.cfg.AdminWrite:
			adminRefuse(rw, writeError{status: http.StatusNotImplemented, code: codeWriteUnavailable})
			return
		case needsSetup && w.Setup() == nil:
			adminRefuse(rw, writeError{status: http.StatusNotImplemented, code: errorSetupUnavailable})
			return
		}
		var body B
		data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxWriteBody))
		if err != nil || decodeStrict(data, &body) != nil {
			adminRefuse(rw, writeError{status: http.StatusBadRequest, code: errorBadRequest})
			return
		}
		alias := r.PathValue("alias")
		if r.PathValue("alias") != "" && !accountAlias.MatchString(alias) {
			adminRefuse(rw, writeError{status: http.StatusBadRequest, field: "alias", code: codeFieldInvalid})
			return
		}
		source, err := checkSource(sourceOf(&body))
		if err != nil {
			adminRefuse(rw, err)
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		answer, err := handle(w, r, alias, &body, session.AuditEntry{SourceIP: adminSourceIP, Action: action, Source: source})
		if err != nil {
			adminRefuse(rw, err)
			return
		}
		writeJSON(rw, http.StatusOK, answer)
	}
}

// adminRefuse answers a refused write as {"error": code, "field"?, "check"?}; an error
// that has no fixed code is a 500 store_unavailable. The answer holds no request content.
func adminRefuse(rw http.ResponseWriter, err error) {
	var we writeError
	if !errors.As(err, &we) || we.code == "" {
		we = errStoreUnavailable
	}
	answer := map[string]string{"error": we.code}
	if we.field != "" {
		answer["field"] = we.field
	}
	if we.check != "" {
		answer["check"] = we.check
	}
	if we.status >= 500 && we.status != http.StatusNotImplemented {
		log.Printf("account command refused code=%s status=%d", we.code, we.status)
	}
	writeJSON(rw, we.status, answer)
}

// accountShow is the answer to `account show`.
type accountShow struct {
	Account          accountConfig `json:"account"`
	Served           bool          `json:"served"`
	Setup            *setupBlock   `json:"setup"`
	Created          *historyRow   `json:"created"`
	History          []historyRow  `json:"history"`
	HistoryTruncated bool          `json:"history_truncated"`
}

// historyRow is one audit row of an account as `account show` gives it. The changes are
// references, settings and check results, none of them a credential value.
type historyRow struct {
	Time     string                         `json:"time"`
	SourceIP string                         `json:"source_ip"`
	Action   string                         `json:"action"`
	Source   taskSource                     `json:"source"`
	Changes  map[string]session.FieldChange `json:"changes"`
}

func historyOf(row session.AuditRow) historyRow {
	source := taskSource{Kind: row.Source.Kind, Thread: row.Source.Thread, TS: row.Source.TS}
	return historyRow{Time: row.Time.UTC().Format("2006-01-02T15:04:05Z"), SourceIP: row.SourceIP, Action: row.Action, Source: source, Changes: row.Changes}
}

// serveAccountShow gives an account's settings, its setup state and its audit rows. It
// reads only; it does not depend on the write switch.
func (a *Admin) serveAccountShow(rw http.ResponseWriter, r *http.Request) {
	serveRecord(rw, r, a.Writer)
}

// serveRecord answers a read of an account's record from w: the answer of `account show`
// and of GET /accounts/{alias}/record, which share this function.
func serveRecord(rw http.ResponseWriter, r *http.Request, w *AccountWriter) {
	if w == nil {
		adminRefuse(rw, writeError{status: http.StatusNotImplemented, code: codeWriteUnavailable})
		return
	}
	alias := r.PathValue("alias")
	if !accountAlias.MatchString(alias) {
		adminRefuse(rw, writeError{status: http.StatusBadRequest, field: "alias", code: codeFieldInvalid})
		return
	}
	out, err := w.record(r.Context(), alias)
	if err != nil {
		adminRefuse(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, out)
}

// record reads an account's settings, setup state, audit rows and creation record.
func (w *AccountWriter) record(ctx context.Context, alias string) (*accountShow, error) {
	row, err := w.cfg.Store.Account(ctx, alias)
	if err != nil {
		return nil, storeError(err)
	}
	out := &accountShow{Account: configOf(row), Served: w.cfg.Accounts.set.Load().lookup(alias) != nil}
	if setup := w.Setup(); setup != nil {
		rows, err := w.cfg.Store.SetupRows(ctx)
		if err != nil {
			return nil, errStoreUnavailable
		}
		out.Setup = setup.block(alias, rows[alias])
	}
	history, truncated, err := w.cfg.Store.AccountAuditLast(ctx, alias, maxHistory)
	if err != nil {
		return nil, errStoreUnavailable
	}
	out.History, out.HistoryTruncated = make([]historyRow, 0, len(history)), truncated
	for _, h := range history {
		out.History = append(out.History, historyOf(h))
	}
	created, err := w.cfg.Store.AccountCreated(ctx, alias)
	if err != nil {
		return nil, errStoreUnavailable
	}
	if created != nil {
		h := historyOf(*created)
		out.Created = &h
	}
	return out, nil
}
