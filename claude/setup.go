package claude

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// actionSetup is the action name the setup request of the page carries.
const actionSetup = "account-setup"

// actionLoginAndToken is the one action of a setup task: log in, run `claude setup-token`
// and hand the token over.
const actionLoginAndToken = "login_and_token"

// maxSetupReport bounds the body of a report; maxSetupToken the token of a handover.
const (
	maxSetupReport = 4 << 10
	maxSetupToken  = 4 << 10
)

// handoverTimeout bounds a handover's own work; the store ends a claimed task after
// session.SetupHandoverTimeout, which is longer, so the work stops first.
const handoverTimeout = 4 * time.Minute

// The shapes of the IDs and of a setup token. A token is a line of the characters the
// token alphabet has, so it cannot break a header or a log line.
var (
	requestIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)
	runIDShape     = regexp.MustCompile(`^[0-9]{8}T[0-9]{9}Z$`)
	setupTokenForm = regexp.MustCompile(`^sk-ant-oat01-[A-Za-z0-9_-]+$`)
)

// setupSteps are the steps a login machine may report: those of the login, of
// `claude setup-token` and of the handover. The gateway's own steps (probe, write,
// reload) are not among them.
var setupSteps = []string{
	"lock", "browser", "egress", "cli", "email_page", "email_sent", "magic_link", "verification_code", "authorize", "callback", "cli_exit", "verify", "complete",
	"token_cli", "token_authorize", "token_callback", "token_read", "handover",
}

// The steps the gateway records itself during a handover.
const (
	stepProbe    = "probe"
	stepWrite    = "write"
	stepReload   = "reload"
	stepComplete = "complete"
)

// setupReasons are the reasons a login machine may report with failed.
var setupReasons = []string{
	"config_invalid", "account_busy", "lock_file_invalid", "credentials_present", "account_dir_symlink", "account_dir_not_private", "claude_dir_symlink", "claude_dir_not_private",
	"browser_pages_unexpected", "browser_version_unreadable", "browser_action_failed", "browser_identity_mismatch", "profile_symlink", "profile_not_private", "page_closed", "page_changed",
	"egress_ip_mismatch", "egress_check_failed", "proxy_reference_invalid", "proxy_token_missing", "proxy_read_timeout", "proxy_read_failed", "proxy_value_invalid",
	"captcha_challenge", "unknown_page", "email_rejected", "email_value_not_retained", "code_value_not_retained",
	"mail_timeout", "mail_command_failed", "mail_response_invalid", "mail_recipient_conflict", "mail_mime_invalid", "ambiguous_magic_link",
	"cli_missing", "cli_start_failed", "cli_environment_unreadable", "cli_output_failed", "cli_output_limit", "cli_url_timeout", "cli_exited_before_url", "ambiguous_auth_url",
	"cli_exited_before_authorize", "cli_config_dir_mismatch", "cli_exited_before_code", "code_shape_unexpected", "cli_input_failed", "cli_exit_timeout", "cli_login_failed",
	"cli_cleanup_timeout", "cli_status_timeout", "cli_status_failed", "cli_status_invalid",
	"login_not_verified", "credentials_expiry_unreadable",
	"step_timeout", "step_not_forward", "interrupted", "not_started", "not_implemented", "unexpected_error",
	"token_cli_failed", "token_url_timeout", "token_output_unreadable", "handover_failed",
}

// humanReasons are the only reasons that go with needs_human.
var humanReasons = []string{"captcha_challenge", "unknown_page", "email_rejected"}

// The reasons the gateway gives itself.
const (
	reasonComplete          = "complete"
	reasonTokenShape        = "token_shape_invalid"
	reasonProbeRefused      = "probe_refused"
	reasonProbeFailed       = "probe_failed"
	reasonTargetNotAllowed  = "write_target_not_allowed"
	reasonTargetMissing     = "write_target_missing"
	reasonTargetAmbiguous   = "write_target_ambiguous"
	reasonWriteFailed       = "write_failed"
	reasonReadbackMismatch  = "readback_mismatch"
	reasonReloadFailed      = "reload_failed"
	errorSetupUnavailable   = "setup_unavailable"
	errorBadRequest         = "bad_request"
	errorUnknownAccount     = "unknown_account"
	errorNoOpenTask         = "no_open_task"
	errorIDsMismatch        = "ids_mismatch"
	errorStateInvalid       = "state_invalid"
	errorStepInvalid        = "step_invalid"
	errorReasonInvalid      = "reason_invalid"
	errorSetupOpen          = "setup_open"
	errorRevStale           = "rev_stale"
	errorAccountStatus      = "account_status"
	errorDirectAccount      = "direct_account"
	errorSetupStoreNotReady = "session store unavailable"
)

// setupError is a refusal with its status and fixed code; written says, for a handover,
// that the token reached the secret store before the failure.
type setupError struct {
	status  int
	code    string
	written bool
}

func (e setupError) Error() string { return e.code }

// Setup runs the gateway's side of setting an account's token: the tasks the page asks
// for, handed to a login machine through the admin socket, and the handover that ends in
// the secret store. It also reads the expiry date of each account's token item. It is nil
// while setup is off.
type Setup struct {
	w         *AccountWriter
	lastAsked atomic.Pointer[time.Time]

	mu       sync.Mutex
	expiries map[string]tokenExpiry
	// reread asks Run to read the expiry dates again.
	reread chan struct{}
}

// Setup returns the gateway's setup, nil when it is off or w is nil.
func (w *AccountWriter) Setup() *Setup {
	if w == nil {
		return nil
	}
	return w.setup
}

// newRequestID returns 16 random bytes as 32 lower-case hex digits.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("claude: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// setupBlock is the state of an account's setup as /dashboard.json gives it. Every field
// is a state, an ID, a fixed code or a time; identity is null until the gateway can check
// that a token is the account's own.
type setupBlock struct {
	State       string      `json:"state"`
	RequestID   *string     `json:"request_id"`
	RunID       *string     `json:"run_id"`
	Step        *string     `json:"step"`
	Reason      *string     `json:"reason"`
	UpdatedAt   *time.Time  `json:"updated_at"`
	TokenSetAt  *time.Time  `json:"token_set_at"`
	Action      *string     `json:"action"`
	Identity    *string     `json:"identity"`
	TokenExpiry tokenExpiry `json:"token_expiry"`
}

// The states of a token's expiry date as the gateway read it from the secret store item.
const (
	expiryRead        = "read"
	expiryNoField     = "no_field"
	expiryUnreadable  = "unreadable"
	expiryUnavailable = "unavailable"
)

// tokenExpiry is the expiry date of the item at an account's token reference, as read:
// state is read (ExpiresAt is set), no_field (the item has no expiry date), unreadable
// (the item or its date could not be read) or unavailable (not read yet).
type tokenExpiry struct {
	State     string     `json:"state"`
	ExpiresAt *time.Time `json:"expires_at"`
	ReadAt    *time.Time `json:"read_at"`
}

func stringPointer(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// blockOf returns row as the dashboard shows it; the zero row is not_started. The expiry
// is not read yet.
func blockOf(row session.SetupRow) *setupBlock {
	state := row.State
	if state == "" {
		state = session.SetupNotStarted
	}
	return &setupBlock{State: state, RequestID: stringPointer(row.RequestID), RunID: stringPointer(row.RunID), Step: stringPointer(row.Step),
		Reason: stringPointer(row.Reason), UpdatedAt: timePointer(row.UpdatedAt), TokenSetAt: timePointer(row.TokenSetAt),
		Action: stringPointer(row.Action), TokenExpiry: tokenExpiry{State: expiryUnavailable}}
}

// block is blockOf with the expiry date last read for the account alias, whose row may
// be the zero row.
func (s *Setup) block(alias string, row session.SetupRow) *setupBlock {
	b := blockOf(row)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.expiries[alias]; ok {
		b.TokenExpiry = e
	}
	return b
}

// setupGateway is gateway.setup of /dashboard.json.
type setupGateway struct {
	Enabled     bool       `json:"enabled"`
	Queued      int        `json:"queued"`
	LastAskedAt *time.Time `json:"last_asked_at"`
}

// decorate puts each account's setup block into the views and returns the gateway's.
func (s *Setup) decorate(rows map[string]session.SetupRow, configs []*accountConfig) *setupGateway {
	for _, c := range configs {
		c.Setup = s.block(c.Alias, rows[c.Alias])
	}
	g := &setupGateway{Enabled: true, LastAskedAt: s.lastAskedAt()}
	for _, row := range rows {
		if row.State == session.SetupRequested {
			g.Queued++
		}
	}
	return g
}

func (s *Setup) lastAskedAt() *time.Time {
	if at := s.lastAsked.Load(); at != nil {
		return timePointer(*at)
	}
	return nil
}

// setupBody is the body of POST /accounts/{alias}/setup.
type setupBody struct {
	Action *string `json:"action"`
	Rev    *int64  `json:"rev"`
}

// accepted is an answer of status 202.
type accepted struct{ body any }

// request records a task for the page's button. The account's setup answers 409 when it
// has a requested or running task, when rev is not the account's and when the account is
// retired.
func (w *AccountWriter) setupRequest(r *http.Request, alias string, body *setupBody, entry session.AuditEntry) (any, error) {
	if body.Action == nil || *body.Action != actionLoginAndToken {
		return nil, fieldProblem("action", "must be login_and_token")
	}
	if body.Rev == nil {
		return nil, fieldProblem("rev", "missing")
	}
	row, before, err := w.cfg.Store.RequestSetup(r.Context(), alias, *body.Action, body.Rev, newRequestID())
	switch {
	case errors.Is(err, session.ErrRevConflict):
		return nil, writeError{status: http.StatusConflict, message: errorRevStale, field: "rev"}
	case errors.Is(err, session.ErrSetupOpen):
		return nil, writeError{status: http.StatusConflict, message: errorSetupOpen}
	case errors.Is(err, session.ErrSetupStatus):
		return nil, writeError{status: http.StatusConflict, message: errorAccountStatus}
	case errors.Is(err, session.ErrSetupDirect):
		return nil, writeError{status: http.StatusConflict, message: errorDirectAccount}
	case err != nil:
		return nil, storeError(err)
	}
	if err := w.cfg.Store.RecordAudit(r.Context(), alias, entry, map[string]session.FieldChange{"setup": {Old: before, New: session.SetupRequested + ":" + *body.Action}}); err != nil {
		log.Printf("account write alias=%s: the setup request was not audited", alias)
	}
	log.Printf("account write ip=%s action=%s alias=%s result=ok setup_action=%s", entry.SourceIP, entry.Action, alias, *body.Action)
	return accepted{body: map[string]any{"setup": w.Setup().block(alias, row)}}, nil
}

// nextTask is the task `setup next` answers: what a login machine needs of the account,
// as references and addresses, never the values of the credentials.
type nextTask struct {
	Alias       string        `json:"alias"`
	Action      string        `json:"action"`
	RequestID   string        `json:"request_id"`
	RequestedAt time.Time     `json:"requested_at"`
	Status      string        `json:"status"`
	Email       string        `json:"email"`
	DeviceID    string        `json:"device_id"`
	Proxy       taskProxyView `json:"proxy"`
}

type taskProxyView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	UsernameRef string `json:"username_ref"`
	PasswordRef string `json:"password_ref"`
	ExpectedIP  string `json:"expected_egress_ip"`
	Direct      bool   `json:"direct"`
}

// Next hands out the next task, or nil. It notes that a login machine asked.
func (s *Setup) Next(ctx context.Context) (*nextTask, error) {
	now := s.w.now().UTC()
	s.lastAsked.Store(&now)
	task, err := s.w.cfg.Store.NextSetup(ctx)
	if err != nil {
		return nil, setupError{status: http.StatusInternalServerError, code: errorSetupStoreNotReady}
	}
	if task == nil {
		return nil, nil
	}
	a := task.Account
	log.Printf("setup alias=%s request_id=%s result=handed_out action=%s", task.Alias, task.RequestID, task.Action)
	return &nextTask{Alias: task.Alias, Action: task.Action, RequestID: task.RequestID, RequestedAt: task.RequestedAt, Status: a.Status, Email: a.Email, DeviceID: a.DeviceID,
		Proxy: taskProxyView{Host: a.ProxyHost, Port: a.ProxyPort, UsernameRef: a.UsernameRef, PasswordRef: a.PasswordRef, ExpectedIP: a.ExpectedIP, Direct: a.Direct}}, nil
}

// reportBody is the body of a setup report.
type reportBody struct {
	RequestID string  `json:"request_id"`
	RunID     string  `json:"run_id"`
	State     string  `json:"state"`
	Step      string  `json:"step"`
	Reason    *string `json:"reason"`
}

// Report records one step of the running task. The state is running, needs_human or
// failed; done is the gateway's alone.
func (s *Setup) Report(ctx context.Context, alias string, body reportBody) (*setupBlock, error) {
	if !requestIDShape.MatchString(body.RequestID) || !runIDShape.MatchString(body.RunID) {
		return nil, setupError{status: http.StatusBadRequest, code: errorBadRequest}
	}
	if _, err := s.w.cfg.Store.Account(ctx, alias); err != nil {
		if errors.Is(err, session.ErrAccountNotFound) {
			return nil, setupError{status: http.StatusNotFound, code: errorUnknownAccount}
		}
		return nil, setupError{status: http.StatusInternalServerError, code: errorSetupStoreNotReady}
	}
	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}
	switch body.State {
	case session.SetupRunning:
		if reason != "" {
			return nil, setupError{status: http.StatusBadRequest, code: errorReasonInvalid}
		}
	case session.SetupNeedsHuman:
		if !slices.Contains(humanReasons, reason) {
			return nil, setupError{status: http.StatusBadRequest, code: errorReasonInvalid}
		}
	case session.SetupFailed:
		if !slices.Contains(setupReasons, reason) || slices.Contains(humanReasons, reason) {
			return nil, setupError{status: http.StatusBadRequest, code: errorReasonInvalid}
		}
	default:
		return nil, setupError{status: http.StatusBadRequest, code: errorStateInvalid}
	}
	if !slices.Contains(setupSteps, body.Step) {
		return nil, setupError{status: http.StatusBadRequest, code: errorStepInvalid}
	}
	row, err := s.w.cfg.Store.ReportSetup(ctx, alias, body.RequestID, body.RunID, body.State, body.Step, reason)
	if err := taskError(err); err != nil {
		return nil, err
	}
	log.Printf("setup alias=%s request_id=%s state=%s step=%s reason=%s", alias, body.RequestID, body.State, body.Step, reason)
	return s.block(alias, row), nil
}

// taskError maps the store's task errors to refusals.
func taskError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrSetupNoOpenTask):
		return setupError{status: http.StatusConflict, code: errorNoOpenTask}
	case errors.Is(err, session.ErrSetupIDsMismatch):
		return setupError{status: http.StatusConflict, code: errorIDsMismatch}
	case errors.Is(err, session.ErrAccountNotFound):
		return setupError{status: http.StatusNotFound, code: errorUnknownAccount}
	}
	return setupError{status: http.StatusInternalServerError, code: errorSetupStoreNotReady}
}

// handoverResult is the answer to a handover that succeeded.
type handoverResult struct {
	Setup   *setupBlock `json:"setup"`
	Probe   string      `json:"probe"`
	Written bool        `json:"written"`
	Reload  string      `json:"reload"`
}

// Token takes the setup token of a running task. It refuses, writing nothing and
// claiming nothing, a task that is not running with this request ID and the run ID a
// report set, and a token of the wrong shape. Otherwise it claims the task and, only
// when every step before has passed, sends one quota request with the token through the
// account's own exit, writes the token to the secret store at the account's token
// reference, reads it back and reloads. Any failure ends the task as failed, drops the
// token and says whether the secret store had it. The token is in no log line, audit
// row, answer or table.
func (s *Setup) Token(ctx context.Context, alias, requestID, runID string, token []byte) (*handoverResult, error) {
	if !accountAlias.MatchString(alias) || !requestIDShape.MatchString(requestID) || !runIDShape.MatchString(runID) {
		return nil, setupError{status: http.StatusBadRequest, code: errorBadRequest}
	}
	if _, err := s.w.cfg.Store.Account(ctx, alias); err != nil {
		return nil, taskError(err)
	}
	if err := taskError(s.w.cfg.Store.CheckHandover(ctx, alias, requestID, runID)); err != nil {
		return nil, err
	}
	if len(token) > maxSetupToken || !setupTokenForm.Match(token) {
		return nil, setupError{status: http.StatusUnprocessableEntity, code: reasonTokenShape}
	}
	task, err := s.w.cfg.Store.ClaimHandover(ctx, alias, requestID, runID)
	if err := taskError(err); err != nil {
		return nil, err
	}
	// The work outlives the connection of the login machine, so a dropped ssh session
	// does not stop it between the write and the reload.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handoverTimeout)
	defer cancel()
	value := string(token)
	return s.handover(ctx, task, value)
}

// recordCtx is a context for recording an outcome that the handover's own time limit
// does not cut short.
func recordCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// fail ends the claimed task as failed with code at step and returns the refusal.
func (s *Setup) fail(ctx context.Context, task session.SetupTask, step, code string, status int, written bool) error {
	ctx, cancel := recordCtx(ctx)
	defer cancel()
	_, err := s.w.cfg.Store.FinishSetup(ctx, task.Alias, task.RequestID, session.SetupFailed, step, code)
	if err != nil {
		log.Printf("setup alias=%s request_id=%s: the failure was not recorded", task.Alias, task.RequestID)
	}
	s.audit(ctx, task, session.SetupFailed+":"+code)
	log.Printf("setup alias=%s request_id=%s step=%s result=failed reason=%s written=%t", task.Alias, task.RequestID, step, code, written)
	return setupError{status: status, code: code, written: written}
}

func (s *Setup) audit(ctx context.Context, task session.SetupTask, outcome string) {
	entry := session.AuditEntry{SourceIP: "admin-socket", Action: "account-setup-token"}
	if err := s.w.cfg.Store.RecordAudit(ctx, task.Alias, entry, map[string]session.FieldChange{"setup": {Old: session.SetupRunning, New: outcome}}); err != nil {
		log.Printf("setup alias=%s: the outcome was not audited", task.Alias)
	}
}

// step records the step of the handover. It fails when the task is no longer the
// claimed, running one, because a timeout or a restart ended it; the handover then goes
// no further.
func (s *Setup) step(ctx context.Context, task session.SetupTask, step string) error {
	if err := s.w.cfg.Store.SetupStep(ctx, task.Alias, task.RequestID, step); err != nil {
		log.Printf("setup alias=%s request_id=%s: the task ended before step %s", task.Alias, task.RequestID, step)
		return setupError{status: http.StatusConflict, code: errorNoOpenTask}
	}
	return nil
}

// handover runs the steps of a claimed task.
func (s *Setup) handover(ctx context.Context, task session.SetupTask, token string) (*handoverResult, error) {
	w := s.w
	account := task.Account
	// The gateway writes only to the token reference it built from the template and the
	// alias; any other reference, and any account without a template, is not written.
	if w.cfg.TokenRefTemplate == "" || account.TokenRef != strings.Replace(w.cfg.TokenRefTemplate, aliasPlaceholder, account.Alias, 1) {
		return nil, s.fail(ctx, task, stepWrite, reasonTargetNotAllowed, http.StatusUnprocessableEntity, false)
	}
	if err := s.step(ctx, task, stepProbe); err != nil {
		return nil, err
	}
	status, err := s.probe(ctx, account, token)
	switch {
	case err != nil:
		return nil, s.fail(ctx, task, stepProbe, reasonProbeFailed, http.StatusBadGateway, false)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, s.fail(ctx, task, stepProbe, reasonProbeRefused, http.StatusBadGateway, false)
	case status/100 != 2:
		return nil, s.fail(ctx, task, stepProbe, reasonProbeFailed, http.StatusBadGateway, false)
	}
	if w.cfg.SetupSecrets == nil {
		return nil, s.fail(ctx, task, stepWrite, reasonWriteFailed, http.StatusBadGateway, false)
	}
	secrets, err := w.cfg.SetupSecrets(ctx)
	if err != nil {
		return nil, s.fail(ctx, task, stepWrite, reasonWriteFailed, http.StatusBadGateway, false)
	}
	// The last look at the task before the write: a task that was ended meanwhile gets no
	// write.
	if err := s.step(ctx, task, stepWrite); err != nil {
		return nil, err
	}
	expires := w.now().Add(session.SetupTokenLife)
	if _, err := credential.Write(ctx, secrets, account.TokenRef, token, expires, true); err != nil {
		switch {
		case errors.Is(err, credential.ErrTargetMissing):
			return nil, s.fail(ctx, task, stepWrite, reasonTargetMissing, http.StatusInternalServerError, false)
		case errors.Is(err, credential.ErrTargetAmbiguous):
			return nil, s.fail(ctx, task, stepWrite, reasonTargetAmbiguous, http.StatusInternalServerError, false)
		case errors.Is(err, credential.ErrTargetRefused):
			return nil, s.fail(ctx, task, stepWrite, reasonTargetNotAllowed, http.StatusUnprocessableEntity, false)
		}
		return nil, s.fail(ctx, task, stepWrite, reasonWriteFailed, http.StatusBadGateway, false)
	}
	if err := w.cfg.Store.NoteTokenWritten(ctx, task.Alias, task.RequestID); err != nil {
		log.Printf("setup alias=%s request_id=%s: the task ended after the write", task.Alias, task.RequestID)
		return nil, setupError{status: http.StatusConflict, code: errorNoOpenTask, written: true}
	}
	if !s.readBack(ctx, account.TokenRef, token) {
		return nil, s.fail(ctx, task, stepWrite, reasonReadbackMismatch, http.StatusInternalServerError, true)
	}
	if err := s.step(ctx, task, stepReload); err != nil {
		return nil, setupError{status: http.StatusConflict, code: errorNoOpenTask, written: true}
	}
	if _, err := w.cfg.Reload(ctx); err != nil {
		return nil, s.fail(ctx, task, stepReload, reasonReloadFailed, http.StatusInternalServerError, true)
	}
	rec, cancel := recordCtx(ctx)
	defer cancel()
	row, err := w.cfg.Store.FinishSetup(rec, task.Alias, task.RequestID, session.SetupDone, stepComplete, reasonComplete)
	if err != nil {
		log.Printf("setup alias=%s request_id=%s: the task ended before it could be closed", task.Alias, task.RequestID)
		return nil, setupError{status: http.StatusConflict, code: errorNoOpenTask, written: true}
	}
	s.audit(rec, task, session.SetupDone)
	s.readExpiry(rec, secrets, task.Alias, account.TokenRef)
	log.Printf("setup alias=%s request_id=%s step=%s result=done written=true", task.Alias, task.RequestID, stepComplete)
	return &handoverResult{Setup: s.block(task.Alias, row), Probe: "ok", Written: true, Reload: "ok"}, nil
}

// probe sends the quota request with token as the account's token, through the account's
// own exit and under the lock of the quota rounds, and returns the upstream's status.
// The account is built outside the served set, so the request records no reading, pause
// or traffic row.
func (s *Setup) probe(ctx context.Context, row session.AccountRow, token string) (int, error) {
	file, data, err := rowDocument(row)
	if err != nil {
		return 0, err
	}
	entry, err := servedAccount(row.Alias, file, data, Claude)
	if err != nil {
		return 0, errors.New("the account's settings are not valid")
	}
	resolve, err := s.w.cfg.Resolve(ctx)
	if err != nil {
		return 0, err
	}
	own := func(ctx context.Context, reference string) (string, error) {
		if reference == row.TokenRef {
			return token, nil
		}
		return resolve(ctx, reference)
	}
	a, err := loadAccount(ctx, entry, own, s.w.cfg.Accounts.set.Load().concurrency)
	if err != nil {
		return 0, errors.New("the account cannot be built")
	}
	defer a.transport.CloseIdleConnections()
	return s.w.cfg.Entrypoint.probeCandidate(ctx, a)
}

// readBack resolves the reference again and reports whether it gives token.
func (s *Setup) readBack(ctx context.Context, reference, token string) bool {
	resolve, err := s.w.cfg.Resolve(ctx)
	if err != nil {
		return false
	}
	got, err := resolve(ctx, reference)
	return err == nil && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Start ends the tasks whose handover was under way when the previous process stopped.
func (s *Setup) Start(ctx context.Context) error {
	n, err := s.w.cfg.Store.InterruptSetups(ctx)
	if err != nil {
		return fmt.Errorf("setup: %v", err)
	}
	if n > 0 {
		log.Printf("setup: %d handover(s) were under way when the last process stopped; ended as interrupted", n)
	}
	return nil
}

// Run applies the timeouts every minute and reads the expiry dates at start, once an
// hour and whenever it is asked to, until ctx is canceled. On a nil Setup it only waits
// for ctx.
func (s *Setup) Run(ctx context.Context) error {
	if s == nil {
		<-ctx.Done()
		return nil
	}
	const readEvery = time.Hour
	last := time.Time{}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if err := s.w.cfg.Store.ExpireSetups(ctx); err != nil && ctx.Err() == nil {
			log.Printf("setup: timeouts not applied: session store unavailable")
		}
		if now := s.w.now(); now.Sub(last) >= readEvery {
			last = now
			s.readExpiries(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-s.reread:
			s.readExpiries(ctx)
		}
	}
}

// RereadExpiries asks Run to read the expiry dates again, without waiting for it.
func (s *Setup) RereadExpiries() {
	if s == nil {
		return
	}
	select {
	case s.reread <- struct{}{}:
	default:
	}
}

// readExpiries reads the expiry date of the item at each account's token reference,
// whatever the account's status. A failure is noted for that account and changes nothing
// else.
func (s *Setup) readExpiries(ctx context.Context) {
	rows, _, err := s.w.cfg.Store.Accounts(ctx)
	if err != nil {
		return
	}
	secrets := s.secrets(ctx)
	for _, row := range rows {
		s.readExpiry(ctx, secrets, row.Alias, row.TokenRef)
	}
}

// secrets opens the secret store, nil when it cannot be opened.
func (s *Setup) secrets(ctx context.Context) credential.Store {
	if s.w.cfg.SetupSecrets == nil {
		return nil
	}
	secrets, err := s.w.cfg.SetupSecrets(ctx)
	if err != nil {
		return nil
	}
	return secrets
}

// readExpiry reads the expiry date at reference and keeps it for alias. It reads no
// token, and logs only the alias and the state.
func (s *Setup) readExpiry(ctx context.Context, secrets credential.Store, alias, reference string) {
	now := s.w.now().UTC()
	e := tokenExpiry{State: expiryUnreadable, ReadAt: &now}
	if secrets != nil {
		at, err := credential.Expires(ctx, secrets, reference)
		switch {
		case err == nil:
			e.State, e.ExpiresAt = expiryRead, &at
		case errors.Is(err, credential.ErrExpiresMissing):
			e.State = expiryNoField
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.expiries[alias]; !ok || old.State != e.State {
		log.Printf("setup alias=%s token_expiry=%s", alias, e.State)
	}
	s.expiries[alias] = e
}
