// The gateway's codes written as a sentence. The page shows the code beside
// the sentence, so a code missing here still reads as itself.
import type { Tone } from "./stats";

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
