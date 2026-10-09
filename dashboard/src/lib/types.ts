// The shape of GET /dashboard.json, written from the Go structs with json
// tags in claude/dashboard.go and session/. Times
// are RFC 3339 in UTC. A key marked optional is omitted by the Go side when
// it is zero.
import {
  arrayOf,
  isBoolean,
  isNumber,
  isString,
  nullable,
  object,
  optional,
  recordOf,
  type Guard,
} from "./guard";

interface Health {
  status: string; // ok or degraded
  reasons?: string[] | undefined;
}

interface RetentionRun {
  at?: string | undefined; // absent before the first run since the start
  pruned: { traffic: number; binding_history: number; sessions: number };
  vacuumed: boolean;
  error?: string | undefined;
}

interface Gateway {
  version?: string | undefined; // absent from a gateway older than this page
  inventory_commit: string;
  started: string;
  wal_bytes: number;
  retention: RetentionRun;
  backup: {
    last_success?: string | undefined;
    last_failure?: string | undefined;
  } | null; // null when no backups are configured
  open_tunnels: number;
  tunnel_limit: number;
}

export interface Quota {
  utilization: number; // non-negative fraction, 1 at the quota; may exceed 1
  status: string;
  reset_at?: string | undefined;
  observed_at: string;
  source: string;
}

interface Plan {
  name: string;
  effective_from: string;
  effective_to: string | null; // null when open-ended
}

export interface Account {
  alias: string;
  email: string;
  expected_egress_ip: string;
  egress_check?: { at: string; outcome: string } | undefined; // ok, mismatch or unread
  paused_until?: string | undefined;
  pause_reason?: string | undefined;
  bound_sessions: number;
  active_sessions: number;
  quota: Record<string, Quota>; // by window, 5h and 7d
  plan: Plan | null;
  next_plan: Plan | null;
  in_flight: number;
  concurrency: number;
}

interface Machine {
  name: string;
  created_at: string;
  revoked_at?: string | undefined;
  max_sessions: number;
  live_sessions: number;
  last_seen?: string | undefined;
}

interface LiveSession {
  id: string; // the first 8 characters
  client_machine: string;
  alias: string; // empty when unbound
  created_at: string;
  last_seen: string;
}

export interface HourTraffic {
  hour: string;
  alias: string;
  kind: string;
  requests: number;
  bytes_up: number;
  bytes_down: number;
}

interface ResultCount {
  kind: string;
  result: string;
  status: number; // 0 when there was none
  count: number;
}

interface Destination {
  host: string;
  port: number;
  alias: string;
  route: string; // direct, account, or "" for an invalid target
  tunnels: number;
  ok: number;
  bytes_up: number;
  bytes_down: number;
  last: string;
}

interface Day {
  day: string;
  limited_inference: number;
  limited_connect: number;
  tunnel_peak: number;
  session_peak: number;
  leaked: number;
}

interface HistoryEntry {
  session: string;
  alias: string;
  from: string;
  to?: string | undefined;
  reason: string;
}

export interface Dashboard {
  at: string;
  soft: number; // quota thresholds, fractions
  hard: number;
  health: Health;
  gateway: Gateway;
  db_bytes: number;
  accounts: Account[];
  machines: Machine[];
  sessions: LiveSession[];
  idle_sessions: number;
  hourly: HourTraffic[];
  results: ResultCount[];
  destinations: Destination[];
  days: Day[];
  history: HistoryEntry[];
}

const isPlan: Guard<Plan> = object<Plan>({
  name: isString,
  effective_from: isString,
  effective_to: nullable(isString),
});

const isDashboard: Guard<Dashboard> = object<Dashboard>({
  at: isString,
  soft: isNumber,
  hard: isNumber,
  health: object<Health>({
    status: isString,
    reasons: optional(arrayOf(isString)),
  }),
  gateway: object<Gateway>({
    version: optional(isString),
    inventory_commit: isString,
    started: isString,
    wal_bytes: isNumber,
    retention: object<RetentionRun>({
      at: optional(isString),
      pruned: object({
        traffic: isNumber,
        binding_history: isNumber,
        sessions: isNumber,
      }),
      vacuumed: isBoolean,
      error: optional(isString),
    }),
    backup: nullable(
      object({
        last_success: optional(isString),
        last_failure: optional(isString),
      }),
    ),
    open_tunnels: isNumber,
    tunnel_limit: isNumber,
  }),
  db_bytes: isNumber,
  accounts: arrayOf(
    object<Account>({
      alias: isString,
      email: isString,
      expected_egress_ip: isString,
      egress_check: optional(object({ at: isString, outcome: isString })),
      paused_until: optional(isString),
      pause_reason: optional(isString),
      bound_sessions: isNumber,
      active_sessions: isNumber,
      quota: recordOf(
        object<Quota>({
          utilization: isNumber,
          status: isString,
          reset_at: optional(isString),
          observed_at: isString,
          source: isString,
        }),
      ),
      plan: nullable(isPlan),
      next_plan: nullable(isPlan),
      in_flight: isNumber,
      concurrency: isNumber,
    }),
  ),
  machines: arrayOf(
    object<Machine>({
      name: isString,
      created_at: isString,
      revoked_at: optional(isString),
      max_sessions: isNumber,
      live_sessions: isNumber,
      last_seen: optional(isString),
    }),
  ),
  sessions: arrayOf(
    object<LiveSession>({
      id: isString,
      client_machine: isString,
      alias: isString,
      created_at: isString,
      last_seen: isString,
    }),
  ),
  idle_sessions: isNumber,
  hourly: arrayOf(
    object<HourTraffic>({
      hour: isString,
      alias: isString,
      kind: isString,
      requests: isNumber,
      bytes_up: isNumber,
      bytes_down: isNumber,
    }),
  ),
  results: arrayOf(
    object<ResultCount>({
      kind: isString,
      result: isString,
      status: isNumber,
      count: isNumber,
    }),
  ),
  destinations: arrayOf(
    object<Destination>({
      host: isString,
      port: isNumber,
      alias: isString,
      route: isString,
      tunnels: isNumber,
      ok: isNumber,
      bytes_up: isNumber,
      bytes_down: isNumber,
      last: isString,
    }),
  ),
  days: arrayOf(
    object<Day>({
      day: isString,
      limited_inference: isNumber,
      limited_connect: isNumber,
      tunnel_peak: isNumber,
      session_peak: isNumber,
      leaked: isNumber,
    }),
  ),
  history: arrayOf(
    object<HistoryEntry>({
      session: isString,
      alias: isString,
      from: isString,
      to: optional(isString),
      reason: isString,
    }),
  ),
});

/** Parses the endpoint's text; throws when it is not the expected shape. */
export function parseDashboard(text: string): Dashboard {
  const value: unknown = JSON.parse(text);
  if (!isDashboard(value)) throw new Error("dashboard.json 的结构与页面不符");
  return value;
}
