import { formatAgo, formatSpan } from "./format";
import type { QuotaRefreshState } from "./types";

/** The header, with the value `refresh`, that the gateway asks of the POST. */
const ACTION_HEADER = "X-Redcoast-Action";

/** Whether the refresh button can be pressed now, and the note beside it. */
export function refreshOffer(
  state: QuotaRefreshState,
  now: Date,
): { enabled: boolean; note: string | null } {
  if (state.running)
    return { enabled: false, note: "Refreshing every account…" };
  if (
    state.cooldown_until !== undefined &&
    Date.parse(state.cooldown_until) > now.getTime()
  )
    return {
      enabled: false,
      note: `Again in ${formatSpan(Date.parse(state.cooldown_until) - now.getTime())}`,
    };
  return {
    enabled: true,
    note:
      state.last_finished === undefined
        ? null
        : `Refreshed ${formatAgo(state.last_finished, now)}`,
  };
}

/** Why the gateway refused a refresh, and the reading the page showed then. */
export interface Refusal {
  text: string;
  at: string; // the `at` of the dashboard.json shown when the answer came
}

/**
 * The refusal to show beside the button: only until the next reading of the
 * gateway, which gives the state the refusal came from, such as the wait
 * after a round.
 */
export function shownRefusal(
  refusal: Refusal | null,
  at: string,
): string | null {
  return refusal?.at === at ? refusal.text : null;
}

/**
 * Asks the gateway to refresh every account's quota. Resolves to null once
 * the gateway started a round or joined the one running, otherwise to why
 * not. The browser adds the page's Origin, which the gateway requires.
 */
export async function requestQuotaRefresh(
  post: typeof fetch = fetch,
): Promise<string | null> {
  try {
    const response = await post("./quota/refresh", {
      method: "POST",
      headers: { [ACTION_HEADER]: "refresh" },
      cache: "no-store",
    });
    if (response.status === 202) return null;
    const retryAfter = Number(response.headers.get("Retry-After"));
    if (response.status === 429 && retryAfter > 0)
      return `Refreshed recently; again in ${formatSpan(retryAfter * 1000)}`;
    return `Could not refresh: HTTP ${String(response.status)}`;
  } catch (e) {
    return `Could not refresh: ${String(e)}`;
  }
}
