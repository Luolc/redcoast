// What the pages compute from dashboard.json: hourly series, totals, error
// rates and the states shown as a word with an icon. Nothing here makes up
// data the gateway did not send.
import type {
  Account,
  Dashboard,
  HistoryEntry,
  HourLatency,
  LiveSession,
  Quota,
  SourceCount,
  UsagePoint,
} from "./types";

export const HOUR_MS = 3_600_000;

/** How a state reads: its ink, and the word that always goes with it. */
export type Tone = "ok" | "warn" | "bad" | "info" | "off";

export interface State {
  tone: Tone;
  word: string;
}

export interface Counts {
  requests: number;
  errors: number;
  limited: number;
}

export interface HourCounts extends Counts {
  hour: string;
}

function add(into: Counts, p: Counts) {
  into.requests += p.requests;
  into.errors += p.errors;
  into.limited += p.limited;
}

/**
 * One entry per hour of the gateway's 24 h window, oldest first, zeros
 * included. The window starts 24 h before at, inside an hour, so it touches
 * 25 hours.
 */
function windowHours(at: string): number[] {
  const last = Math.floor(Date.parse(at) / HOUR_MS) * HOUR_MS;
  const hours: number[] = [];
  for (let t = last - 24 * HOUR_MS; t <= last; t += HOUR_MS) hours.push(t);
  return hours;
}

/** The start of each hour of the 24 h window, oldest first. */
export function windowHourStarts(at: string): string[] {
  return windowHours(at).map((t) => new Date(t).toISOString());
}

export function hourSeries(rows: UsagePoint[], at: string): HourCounts[] {
  const series: HourCounts[] = [];
  const index = new Map<number, HourCounts>();
  for (const t of windowHours(at)) {
    const h = {
      hour: new Date(t).toISOString(),
      requests: 0,
      errors: 0,
      limited: 0,
    };
    series.push(h);
    index.set(t, h);
  }
  for (const r of rows) {
    const h = index.get(Date.parse(r.hour));
    if (h !== undefined) add(h, r);
  }
  return series;
}

export interface HourDurations {
  hour: string;
  // Null for an hour with no answered inference request.
  p50: number | null;
  p95: number | null;
}

/** The duration percentiles of each hour of the window, oldest first. */
export function durationSeries(
  rows: HourLatency[],
  at: string,
): HourDurations[] {
  const byHour = new Map(rows.map((r) => [Date.parse(r.hour), r]));
  return windowHours(at).map((t) => {
    const r = byHour.get(t);
    return {
      hour: new Date(t).toISOString(),
      p50: r?.duration_p50_ms ?? null,
      p95: r?.duration_p95_ms ?? null,
    };
  });
}

export function total(rows: Counts[]): Counts {
  const sum = { requests: 0, errors: 0, limited: 0 };
  for (const r of rows) add(sum, r);
  return sum;
}

export interface MachineCounts extends Counts {
  machine: string; // empty for requests of no session
}

/** The counts of each client machine, most requests first. */
export function byMachine(rows: UsagePoint[]): MachineCounts[] {
  const machines = new Map<string, MachineCounts>();
  for (const r of rows) {
    let m = machines.get(r.client_machine);
    if (m === undefined) {
      m = { machine: r.client_machine, requests: 0, errors: 0, limited: 0 };
      machines.set(r.client_machine, m);
    }
    add(m, r);
  }
  return [...machines.values()].sort((a, b) =>
    b.requests !== a.requests
      ? b.requests - a.requests
      : a.machine.localeCompare(b.machine),
  );
}

/** Errors per request; 0 when there were no requests. */
export function errorRate(c: Counts): number {
  return c.requests === 0 ? 0 : c.errors / c.requests;
}

export function errorRateState(rate: number): State {
  if (rate >= 0.02) return { tone: "bad", word: "High" };
  if (rate >= 0.01) return { tone: "warn", word: "Elevated" };
  return { tone: "ok", word: "Normal" };
}

export interface ErrorKinds {
  upstream: number; // the upstream answered 4xx or 5xx
  limited: number; // the gateway's own limits
  other: number; // refused by the gateway, or broken off
}

export function errorKinds(sources: SourceCount[]): ErrorKinds {
  const kinds = { upstream: 0, limited: 0, other: 0 };
  for (const s of sources) {
    if (!s.error) continue;
    if (s.result === "limited") kinds.limited += s.count;
    else if (s.result === "ok") kinds.upstream += s.count;
    else kinds.other += s.count;
  }
  return kinds;
}

export interface Failure {
  kind: string;
  result: string;
  status: number;
  count: number;
}

/** The failed results summed over accounts and machines, most first. */
export function failures(sources: SourceCount[]): {
  answered: number;
  failed: Failure[];
} {
  let answered = 0;
  const failed = new Map<string, Failure>();
  for (const s of sources) {
    if (!s.error) {
      answered += s.count;
      continue;
    }
    const key = `${s.kind}\t${s.result}\t${String(s.status)}`;
    const f = failed.get(key);
    if (f === undefined)
      failed.set(key, {
        kind: s.kind,
        result: s.result,
        status: s.status,
        count: s.count,
      });
    else f.count += s.count;
  }
  return {
    answered,
    failed: [...failed.values()].sort((a, b) => b.count - a.count),
  };
}

const WINDOW_MS: Record<string, number> = {
  "5h": 5 * HOUR_MS,
  "7d": 7 * 24 * HOUR_MS,
};

/** The length of a quota window in ms; undefined for one the page does not know. */
export function windowSpan(window: string): number | undefined {
  return WINDOW_MS[window];
}

export interface Pace {
  elapsed: number; // the fraction of the window that has passed
  by: number; // points of the window used beyond an even pace; negative below it
}

/**
 * How far a window's use is from an even pace through its cycle, from its
 * utilization and reset time alone; null when the reset is unknown or has
 * passed, or the window's length is not known.
 */
export function pace(q: Quota, window: string, now: Date): Pace | null {
  const span = WINDOW_MS[window];
  if (span === undefined || q.reset_at === undefined) return null;
  const left = Date.parse(q.reset_at) - now.getTime();
  if (left <= 0) return null;
  const elapsed = Math.max(0, 1 - left / span);
  return { elapsed, by: Math.round((q.utilization - elapsed) * 100) };
}

// A pause with no end is stored as one in the year 9999.
const NO_END = Date.parse("9000-01-01T00:00:00Z");

/** Whether a pause lasts until someone resumes the account. */
export function pausedForGood(until: string): boolean {
  return Date.parse(until) >= NO_END;
}

const PAUSES: Record<string, State> = {
  egress_ip_mismatch: { tone: "bad", word: "Paused · egress mismatch" },
  needs_reimport: { tone: "bad", word: "Paused · needs re-import" },
  upstream_401: { tone: "bad", word: "Paused · login refused" },
  upstream_429: { tone: "warn", word: "Paused · rate-limited" },
};

/**
 * How selection reads one window (session/select.go): a reading whose reset
 * has passed counts as unread, and an upstream "rejected" as over the hard
 * limit, whatever the utilization.
 */
export type Tier = "unread" | "open" | "soft" | "exhausted";

export function windowTier(
  q: Quota,
  soft: number,
  hard: number,
  now: Date,
): Tier {
  if (q.reset_at !== undefined && Date.parse(q.reset_at) <= now.getTime())
    return "unread";
  if (q.utilization >= hard || q.status === "rejected") return "exhausted";
  if (q.utilization >= soft) return "soft";
  return "open";
}

const TIER_ORDER: Tier[] = ["unread", "open", "soft", "exhausted"];

export function accountState(
  a: Account,
  soft: number,
  hard: number,
  now: Date,
): State {
  if (a.paused_until !== undefined)
    return PAUSES[a.pause_reason ?? ""] ?? { tone: "off", word: "Paused" };
  if (a.plan === null) return { tone: "bad", word: "No plan" };
  const tiers = Object.values(a.quota).map((q) =>
    windowTier(q, soft, hard, now),
  );
  const worst =
    TIER_ORDER[Math.max(0, ...tiers.map((t) => TIER_ORDER.indexOf(t)))];
  if (worst === "exhausted") return { tone: "bad", word: "Over hard limit" };
  if (worst === "soft") return { tone: "warn", word: "Near limit" };
  return { tone: "ok", word: "Available" };
}

/** Accounts that selection may choose now: not paused, with a plan, under the hard limit. */
export function available(d: Dashboard, now: Date): Account[] {
  return d.accounts.filter((a) => {
    if (a.paused_until !== undefined) return false;
    const t = accountState(a, d.soft, d.hard, now).tone;
    return t === "ok" || t === "warn";
  });
}

export function healthState(d: Dashboard): State {
  return d.health.status === "ok"
    ? { tone: "ok", word: "Healthy" }
    : { tone: "warn", word: "Degraded" };
}

const QUIET_MS = 2 * 60_000;

export function sessionState(s: LiveSession, now: Date): State {
  return now.getTime() - Date.parse(s.last_seen) < QUIET_MS
    ? { tone: "ok", word: "Working" }
    : { tone: "off", word: "Quiet" };
}

/** Live sessions with no request for 24 h, as the gateway counts idle_sessions. */
export function idleSessions(sessions: LiveSession[], now: Date): number {
  return sessions.filter(
    (s) => now.getTime() - Date.parse(s.last_seen) >= 24 * HOUR_MS,
  ).length;
}

/** Bindings that started in the 24 h before now, and how many of them were rebinds. */
export function recentBindings(
  history: HistoryEntry[],
  now: Date,
): { all: number; rebound: number } {
  const recent = history.filter(
    (h) => now.getTime() - Date.parse(h.from) < 24 * HOUR_MS,
  );
  return {
    all: recent.length,
    rebound: recent.filter((h) => h.reason !== "new").length,
  };
}

export interface ClientRow {
  machine: string;
  client: string; // empty when the launcher did not name a known one
  versions: string[]; // of its live sessions, newest first
  live: number;
  sessions: number; // created in the last 7 days, live or not
  requests: number[]; // per hour of the 24 h window, oldest first
}

const byVersion = new Intl.Collator("en", { numeric: true });

/**
 * One row per client machine and client program, from the session counts
 * of the last 7 days and the hourly requests of the 24 h window, most
 * requests first.
 */
export function clientRows(
  d: Pick<Dashboard, "at" | "clients" | "client_hourly" | "sessions">,
): ClientRow[] {
  const hours = windowHours(d.at);
  const slot = new Map(hours.map((t, i) => [t, i]));
  const rows = new Map<string, ClientRow>();
  const row = (machine: string, client: string) => {
    const k = `${machine}\t${client}`;
    let r = rows.get(k);
    if (r === undefined) {
      r = {
        machine,
        client,
        versions: [],
        live: 0,
        sessions: 0,
        requests: hours.map(() => 0),
      };
      rows.set(k, r);
    }
    return r;
  };
  for (const c of d.clients ?? []) {
    const r = row(c.client_machine, c.client);
    r.live += c.live;
    r.sessions += c.sessions;
  }
  for (const h of d.client_hourly ?? []) {
    const i = slot.get(Date.parse(h.hour));
    if (i === undefined) continue;
    const r = row(h.client_machine, h.client);
    r.requests[i] = (r.requests[i] ?? 0) + h.requests;
  }
  for (const s of d.sessions) {
    const r = rows.get(`${s.client_machine}\t${s.client ?? ""}`);
    const v = s.client_version ?? "";
    if (r !== undefined && v !== "" && !r.versions.includes(v))
      r.versions.push(v);
  }
  const sum = (r: ClientRow) => r.requests.reduce((a, b) => a + b, 0);
  return [...rows.values()]
    .map((r) => ({
      ...r,
      versions: r.versions.sort((a, b) => byVersion.compare(b, a)),
    }))
    .sort((a, b) => {
      if (sum(a) !== sum(b)) return sum(b) - sum(a);
      if (a.machine !== b.machine) return a.machine.localeCompare(b.machine);
      return a.client.localeCompare(b.client);
    });
}

type ModelRow = NonNullable<Dashboard["model_usage"]>[number];

export interface TokenCounts {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

/** The tokens of rows summed; null when no row reports tokens (an older gateway). */
export function tokenTotal(
  rows: (UsagePoint | ModelRow)[],
): TokenCounts | null {
  let reported = false;
  const sum = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  for (const r of rows) {
    if (r.input_tokens === undefined) continue;
    reported = true;
    sum.input += r.input_tokens;
    sum.output += r.output_tokens ?? 0;
    sum.cacheRead += r.cache_read_input_tokens ?? 0;
    sum.cacheWrite += r.cache_creation_input_tokens ?? 0;
  }
  return reported ? sum : null;
}

export interface ModelTotal {
  model: string; // empty when the responses named none
  requests: number;
  tokens: TokenCounts;
}

/** Each model's requests and tokens over the window, most requests first. */
export function byModel(rows: ModelRow[]): ModelTotal[] {
  const models = new Map<string, ModelRow[]>();
  for (const r of rows)
    models.set(r.model, [...(models.get(r.model) ?? []), r]);
  return [...models.entries()]
    .map(([model, rs]) => ({
      model,
      requests: rs.reduce((n, r) => n + r.requests, 0),
      tokens: tokenTotal(rs) ?? {
        input: 0,
        output: 0,
        cacheRead: 0,
        cacheWrite: 0,
      },
    }))
    .sort((a, b) =>
      a.requests !== b.requests
        ? b.requests - a.requests
        : a.model.localeCompare(b.model),
    );
}

/**
 * The tokens of each hour of the 24 h window, oldest first, as hourSeries
 * places its counts; null throughout when no row reports tokens.
 */
export function hourTokens(
  rows: UsagePoint[],
  at: string,
): (TokenCounts | null)[] {
  return windowHours(at).map((t) =>
    tokenTotal(rows.filter((r) => Date.parse(r.hour) === t)),
  );
}
