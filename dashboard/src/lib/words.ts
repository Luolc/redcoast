// The gateway's codes written as a sentence. The page shows the code beside
// the sentence, so a code missing here still reads as itself.
import type { State, Tone } from "./stats";

export interface Wording {
  text: string;
  tone: Tone;
}

const RESULTS: Record<string, Wording> = {
  limited: { text: "Gateway busy: concurrency limit", tone: "warn" },
  quota_exhausted: { text: "Every account over its hard limit", tone: "bad" },
  wrong_source: { text: "Session used from another address", tone: "bad" },
  auth_failed: { text: "Session credential missing or unknown", tone: "bad" },
  not_session: { text: "Not a gateway session", tone: "bad" },
  no_account: { text: "No account available", tone: "bad" },
  store_failed: { text: "Session store unavailable", tone: "bad" },
  forbidden: { text: "Request not on the allowlist", tone: "bad" },
  egress_failed: { text: "Exit failed", tone: "warn" },
  upstream_failed: { text: "Upstream unreachable", tone: "warn" },
  incomplete: { text: "Broken off before the end", tone: "warn" },
  idle_timeout: { text: "Cut by the idle timeout", tone: "warn" },
  max_duration: { text: "Cut at the maximum duration", tone: "warn" },
};

/** A failed result as a sentence; an ok result failed by the upstream's status. */
export function resultWording(
  kind: string,
  result: string,
  status: number,
): Wording {
  if (result === "refused")
    return kind === "connect"
      ? { text: "CONNECT target refused", tone: "bad" }
      : { text: "Refused before forwarding", tone: "bad" };
  if (result === "ok") {
    if (status === 429) return { text: "Upstream rate limit", tone: "warn" };
    if (status >= 500) return { text: "Upstream server error", tone: "warn" };
    return { text: "Upstream refused the request", tone: "bad" };
  }
  return RESULTS[result] ?? { text: result, tone: "bad" };
}

const BINDINGS: Record<string, string> = {
  new: "First binding",
  unavailable: "Account became unavailable",
  quota_hard: "Account over its hard limit",
  lease_expired: "Lease expired",
  reload: "Moved by a reload",
};

export function bindingReason(reason: string): string {
  return BINDINGS[reason] ?? reason;
}

const EGRESS: Record<string, Wording> = {
  ok: { text: "Matches", tone: "ok" },
  mismatch: { text: "Mismatch", tone: "bad" },
  unread: { text: "IP not read", tone: "warn" },
};

export function egressWording(outcome: string | undefined): Wording {
  if (outcome === undefined) return { text: "Not checked", tone: "off" };
  return EGRESS[outcome] ?? { text: outcome, tone: "warn" };
}

const PROVIDERS: Record<string, string> = { claude: "Claude", codex: "Codex" };

/** The provider's name; "Gateway" when the gateway does not say. */
export function providerName(provider: string | undefined): string {
  if (provider === undefined) return "Gateway";
  return PROVIDERS[provider] ?? provider;
}

/**
 * Whether the provider counts cache reads and writes apart from input:
 * Claude does; Codex counts cached tokens in input and reports no cache.
 */
export function cacheApart(provider: string | undefined): boolean {
  return provider !== "codex";
}

const ACCOUNT_STATUS: Record<string, State> = {
  testing: { tone: "info", word: "Testing" },
  active: { tone: "ok", word: "Active" },
  paused: { tone: "warn", word: "Paused" },
  suspended: { tone: "bad", word: "Suspended" },
  retired: { tone: "off", word: "Retired" },
};

/** An account's status as the accounts table keeps it. */
export function accountStatusWording(status: string): State {
  return ACCOUNT_STATUS[status] ?? { tone: "warn", word: status };
}

/** One of the three checks: null while it has not been run since the last change. */
export function checkWording(ok: boolean | null): State {
  if (ok === null) return { tone: "off", word: "Not verified" };
  return ok ? { tone: "ok", word: "Passed" } : { tone: "bad", word: "Failed" };
}

export const CHECK_NAMES = {
  token_ok: "Token reference resolves",
  egress_ok: "Exit reads the expected IP",
  probe_ok: "Quota probe answers",
} as const;

// What a refused activation or edit names: a check, a condition of activating,
// or a field of the request.
const REFUSED: Record<string, string> = {
  ...CHECK_NAMES,
  status: "Status",
  access: "Access path",
  plan: "Plan",
  exit: "Exit IP",
  rev: "Revision",
  alias: "Alias",
  email: "Email",
  receiver_email: "Receiver email",
  "proxy.provider": "Proxy provider",
  "proxy.type": "Proxy type",
  device_id: "Device ID",
  account_uuid: "Account UUID",
  token_ref: "Token reference",
  "proxy.host": "Proxy host",
  "proxy.port": "Proxy port",
  "proxy.expected_egress_ip": "Expected egress IP",
  "proxy.username_ref": "Proxy login",
  "proxy.password_ref": "Proxy login",
};

/** The name of the check or field a refusal concerns; the code itself if unknown. */
export function refusedName(code: string): string {
  return REFUSED[code] ?? code;
}

const BURN_REASONS: Record<string, string> = {
  no_reading: "no reading in this window",
  too_few_points: "too few readings since the last reset",
  span_too_short: "the readings span too short a time",
  window_not_given: "the upstream does not give this window",
  reset_passed: "the window has reset and has no reading since",
  flat: "use has not risen since the last reset",
};

/** Why a window has no estimate, as a phrase. */
export function burnReason(reason: string | undefined): string {
  if (reason === undefined) return "too few readings";
  return BURN_REASONS[reason] ?? reason;
}

const SETUP_STATES: Record<string, State> = {
  not_started: { tone: "off", word: "Not started" },
  requested: { tone: "info", word: "Waiting for a login machine" },
  running: { tone: "info", word: "Running" },
  needs_human: { tone: "warn", word: "Needs a person" },
  done: { tone: "ok", word: "Done" },
  failed: { tone: "bad", word: "Failed" },
};

/** The state of an account's latest setup task. */
export function setupStateWording(state: string): State {
  return SETUP_STATES[state] ?? { tone: "warn", word: state };
}

const SETUP_STEPS: Record<string, string> = {
  lock: "Taking the account's lock",
  browser: "Starting the browser",
  egress: "Checking the exit",
  cli: "Starting the Claude CLI",
  email_page: "On the email page",
  email_sent: "Email sent",
  awaiting_captcha: "Waiting for a person at the CAPTCHA page",
  magic_link: "Reading the sign-in link",
  verification_code: "Entering the verification code",
  authorize: "Authorizing the login",
  callback: "Waiting for the login callback",
  cli_exit: "Waiting for the CLI to finish",
  verify: "Checking the login",
  complete: "Complete",
  token_cli: "Starting the token command",
  token_authorize: "Authorizing the token",
  token_callback: "Waiting for the token callback",
  token_read: "Reading the token",
  handover: "Handing the token to the gateway",
  probe: "Testing the token with one request",
  write: "Writing the token to the secret manager",
  reload: "Reloading the gateway",
};

/** What a setup step code means, as a phrase. */
export function setupStepWording(step: string): string {
  return SETUP_STEPS[step] ?? step;
}

const SETUP_REASONS: Record<string, string> = {
  captcha_challenge: "The login page asked for a CAPTCHA",
  unknown_page: "The login page was not one the automation knows",
  email_rejected: "The login page rejected the email address",
  egress_ip_mismatch: "The exit's IP is not the expected one",
  egress_check_failed: "The exit's IP could not be read",
  account_busy: "The account is busy with another task",
  credentials_present: "The login machine already holds credentials",
  mail_timeout: "The sign-in email did not arrive in time",
  step_timeout: "A step took too long",
  interrupted: "The task was interrupted",
  timeout: "The task ran out of time",
  not_started: "The task never started",
  token_cli_failed: "The token command failed",
  handover_failed: "The token could not be handed over",
  token_shape_invalid: "What the login machine sent is not a token",
  probe_refused: "The upstream refused the token",
  probe_failed: "The test request with the token failed",
  write_target_not_allowed:
    "The token reference is not one the gateway may write",
  write_target_missing: "The secret manager has no item at the token reference",
  write_target_ambiguous: "The token reference matches more than one item",
  write_failed: "The secret manager did not take the token",
  readback_mismatch: "The stored token read back different",
  reload_failed: "The gateway could not reload",
  account_status: "The account is no longer testing or active",
  direct_account: "The account has a direct exit",
  unexpected_error: "The automation failed unexpectedly",
};

/** Why a setup failed or needs a person, as a sentence. */
export function setupReasonWording(reason: string): string {
  return SETUP_REASONS[reason] ?? reason;
}

const SETUP_REFUSALS: Record<string, string> = {
  setup_open: "A setup is already requested or running for this account.",
  account_status: "Setup is only for accounts that are testing or active.",
  direct_account: "Setup is not offered for an account with a direct exit.",
  setup_unavailable: "Setup is off on this gateway.",
  store_unavailable: "The gateway's store did not answer; try again shortly.",
};

/** A refused setup request as a sentence; unknown codes stay as they are. */
export function setupRefusalWording(code: string): string {
  return SETUP_REFUSALS[code] ?? code;
}

const AUDIT_ACTIONS: Record<string, string> = {
  "account-create": "Account created",
  "account-update": "Settings changed",
  "account-verify": "Checked",
  "account-activate": "Activation tried",
  "account-setup": "Setup requested",
  "account-setup-report": "Setup step",
  "account-setup-token": "Token handover",
};

/** What an audit action code means, as a phrase. */
export function auditActionWording(action: string): string {
  return AUDIT_ACTIONS[action] ?? action;
}

const RECORD_ERRORS: Record<string, string> = {
  account_write_unavailable:
    "This gateway keeps no record page: account writes are not on.",
  unknown_account: "The gateway has no such account.",
  store_unavailable: "The gateway's store did not answer; try again shortly.",
};

/** Why the record could not be read, as a sentence; unknown codes stay as they are. */
export function recordErrorWording(code: string): string {
  return RECORD_ERRORS[code] ?? code;
}

/** What the gateway found of the token's expiry when it last read the item. */
export function tokenExpiryWording(state: string): string {
  switch (state) {
    case "no_field":
      return "The token item has no expiry field";
    case "unreadable":
      return "The expiry could not be read from the secret manager";
    case "unavailable":
      return "Not read yet";
    default:
      return state;
  }
}
