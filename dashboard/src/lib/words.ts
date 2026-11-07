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
