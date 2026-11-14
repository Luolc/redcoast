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
  pruned: {
    traffic: number;
    binding_history: number;
    sessions: number;
    quota_history?: number | undefined; // absent from a gateway older than this page
  };
  vacuumed: boolean;
  error?: string | undefined;
}

// Another provider's gateway's dashboard. The page only links to it, and
// reads nothing from it: its Host and Origin checks refuse this page.
interface DashboardLink {
  provider: string; // claude or codex
  url: string; // absolute, http or https
}

interface Gateway {
  provider?: string | undefined; // claude or codex; absent from a gateway older than this page
  dashboards?: DashboardLink[] | undefined; // the other providers'; absent from an older gateway
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
  // null at a gateway that cannot refresh quota (Codex); absent from an older one.
  quota_refresh?: QuotaRefreshState | null | undefined;
  // null while setup is off (the gateway has no setup, or it is switched off);
  // absent from an older gateway.
  setup?: GatewaySetup | null | undefined;
}

// The gateway's setup as a whole; present only while setup is on.
interface GatewaySetup {
  enabled: boolean;
  queued: number; // requested tasks no login machine has taken yet
  last_asked_at: string | null; // when a login machine last asked for a task
}

// What the gateway read of the token item's expiry: read (expires_at is that
// day's start), no_field (the item has none), unreadable (the secret manager
// did not answer) or unavailable (not read yet).
interface TokenExpiry {
  state: string;
  expires_at: string | null;
  read_at: string | null;
}

// Where a task or a change came from: the dashboard (no thread), or a Slack
// message with its thread key and ts.
export interface TaskSource {
  kind: string;
  thread?: string | undefined;
  ts?: string | undefined;
}

export const isTaskSource = object<TaskSource>({
  kind: isString,
  thread: optional(isString),
  ts: optional(isString),
});

// One account's latest setup task. Every field is a state, an ID, a fixed
// code or a time; the gateway never sends a token.
export interface SetupBlock {
  state: string; // not_started, requested, running, needs_human, done or failed
  request_id: string | null;
  run_id: string | null;
  step: string | null;
  reason: string | null;
  updated_at: string | null;
  token_set_at: string | null; // null when the gateway did not write the token
  action: string | null; // login_and_token or token_only
  identity: string | null;
  // When a login program said it would stop waiting for a person at a CAPTCHA
  // page; absent from a gateway older than this page.
  wait_until?: string | null | undefined;
  // Where the task was requested from: a record only, never an approval.
  source?: TaskSource | undefined;
  // The expiry the token's secret manager item carries; absent from a gateway
  // older than this page.
  token_expiry?: TokenExpiry | undefined;
}

// The dashboard's refresh of every account's quota.
export interface QuotaRefreshState {
  running: boolean; // a round is waiting or running
  last_started?: string | undefined;
  last_finished?: string | undefined;
  cooldown_until?: string | undefined; // set while a new round is refused
}

export interface Quota {
  utilization: number; // non-negative fraction, 1 at the quota; may exceed 1
  status: string;
  reset_at?: string | undefined;
  observed_at: string;
  source: string;
}

// One sampled reading of an account's quota history.
interface QuotaPoint {
  at: string;
  utilization: number;
}

// An account's estimate for one quota window from its readings since the last
// reset. The numbers are null, with a reason, when the readings do not allow one.
export interface WindowBurn {
  rate_per_hour: number | null; // utilization per hour; 1 is the whole quota
  exhausts_at: string | null;
  resets_at: string | null;
  before_reset: boolean | null;
  points: number;
  span_minutes: number;
  reason?: string | undefined;
}

// A client machine's share of an account's rate in one window, an estimate.
interface MachineBurn {
  window: string;
  machine: string;
  share: number;
  rate_per_hour: number;
  estimate: boolean;
}

interface Burn {
  "5h": WindowBurn;
  "7d": WindowBurn;
  by_machine: MachineBurn[];
}

interface WindowTotals {
  requests: number;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
}

interface Totals {
  "24h": WindowTotals;
  "7d": WindowTotals;
}

interface Plan {
  name: string;
  effective_from: string;
  effective_to: string | null; // null when open-ended
}

// The proxy block of an account's row in the accounts table. The references
// are names in the secret manager, never values.
interface ProxyConfig {
  provider: string; // empty when not set
  type: string; // empty when not set
  direct: boolean;
  host: string;
  port: number;
  expected_egress_ip: string;
  username_ref: string;
  password_ref: string;
}

// The last run of the three checks: null while not run, voided by a change of
// exit or reference.
export interface Verification {
  token_ok: boolean | null;
  egress_ok: boolean | null;
  probe_ok: boolean | null;
  at?: string | undefined;
}

// One row of the accounts table, as the gateway holds it.
export interface AccountConfig {
  alias: string;
  email: string;
  receiver_email: string; // empty when not set
  access: string; // direct or gateway
  status: string; // testing, active, paused, suspended, retired or kyc
  token_ref: string;
  proxy: ProxyConfig;
  device_id: string; // empty until set
  account_uuid: string;
  verify: Verification;
  rev: number; // the version an edit is made against
  created_at: string;
  updated_at: string;
  // Absent while setup is off.
  setup?: SetupBlock | undefined;
}

// A proxy login that accounts already use; a write can only choose among these.
export interface ProxyRefOption {
  username_ref: string;
  password_ref: string;
}

// Whether this request's source may write accounts; reason says why not.
interface WriteState {
  enabled: boolean;
  reason?: string | undefined;
  can_create: boolean; // a new account can be created
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
  // By window, oldest first, over the window's length (5 hours, 7 days); a
  // window without readings has no key. Absent from a gateway older than this
  // page.
  quota_history?: Record<string, QuotaPoint[]> | undefined;
  // Absent from a gateway older than this page.
  burn?: Burn | undefined;
  totals?: Totals | undefined;
  plan: Plan | null;
  next_plan: Plan | null;
  in_flight: number;
  concurrency: number;
  // The account's row; absent from a gateway without an accounts table.
  config?: AccountConfig | undefined;
}

interface Machine {
  name: string;
  created_at: string;
  revoked_at?: string | undefined;
  max_sessions: number;
  live_sessions: number;
  last_seen?: string | undefined;
}

export interface LiveSession {
  id: string; // the first 8 characters
  client_machine: string;
  alias: string; // empty when unbound
  created_at: string;
  last_seen: string;
  // claude-code, pi or codex-cli, and its version; empty when the launcher did
  // not report a valid one, absent from a gateway older than this page.
  client?: string | undefined;
  client_version?: string | undefined;
}

// Sessions of one client machine and client program; client is empty when
// unknown.
interface ClientCount {
  client_machine: string;
  client: string;
  live: number; // not ended
  sessions: number; // created in the 7-day window, live or not
}

// One hour's requests of the sessions of one client machine and client
// program; requests of no session are not counted.
interface ClientHour {
  hour: string;
  client_machine: string;
  client: string;
  requests: number;
}

interface ResultCount {
  kind: string;
  result: string;
  status: number; // 0 when there was none
  count: number;
}

// Sums of the token counts the upstream's responses reported, as the provider
// counts them: Claude's input tokens leave out the cache reads and writes,
// which it counts apart; Codex counts no cache tokens apart and reports them as
// 0, its input tokens including the cached ones.
interface Tokens {
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
}

// Errors are rows with a result other than ok, or with a status of 400 or more;
// a CONNECT tunnel the limits closed (idle_timeout, max_duration) is no error.
// Limited rows are among the errors. The tokens, absent from a gateway older
// than this page, count errors too.
export interface UsagePoint {
  hour: string;
  alias: string; // empty for requests carried on no account
  client_machine: string; // empty for requests of no session
  kind: string;
  requests: number;
  bytes_up: number;
  bytes_down: number;
  errors: number;
  limited: number;
  input_tokens?: number | undefined;
  output_tokens?: number | undefined;
  cache_read_input_tokens?: number | undefined;
  cache_creation_input_tokens?: number | undefined;
}

// One hour's tokens of one account and model, over the requests whose
// response reported usage; model is empty when the response named none.
interface ModelUsage extends Tokens {
  hour: string;
  alias: string;
  model: string;
  requests: number;
}

export interface SourceCount extends ResultCount {
  alias: string;
  client_machine: string;
  error: boolean; // whether this result and status count as an error
}

// Nearest-rank percentiles, in ms, of the inference requests that are no error.
// A duration is the whole request until its response finished streaming, not
// the time to its first byte; CONNECT tunnels are left out.
interface Latency {
  requests: number;
  duration_p50_ms: number;
  duration_p95_ms: number;
}

export type HourLatency = Latency & { hour: string };

interface LatencyView {
  hourly: HourLatency[];
  accounts: (Latency & { alias: string })[]; // "" for no account
  machines: (Latency & { client_machine: string })[]; // "" for no session
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

export interface HistoryEntry {
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
  // Rows of the accounts table that the gateway does not serve; absent from a
  // gateway older than the table.
  unserved_accounts?: AccountConfig[] | undefined;
  accounts_editable?: boolean | undefined;
  write?: WriteState | undefined;
  proxy_ref_options?: ProxyRefOption[] | undefined;
  machines: Machine[];
  sessions: LiveSession[];
  idle_sessions: number;
  // By account and client machine; absent from a gateway older than this page.
  usage?: UsagePoint[] | undefined;
  result_sources?: SourceCount[] | undefined;
  model_usage?: ModelUsage[] | undefined; // absent from a gateway older than this page
  // Absent from a gateway older than this page.
  clients?: ClientCount[] | undefined;
  client_hourly?: ClientHour[] | undefined;
  destinations: Destination[];
  days: Day[];
  history: HistoryEntry[];
  latency?: LatencyView | undefined; // absent from a gateway older than this page
}

const isPlan: Guard<Plan> = object<Plan>({
  name: isString,
  effective_from: isString,
  effective_to: nullable(isString),
});

const isWindowBurn = object<WindowBurn>({
  rate_per_hour: nullable(isNumber),
  exhausts_at: nullable(isString),
  resets_at: nullable(isString),
  before_reset: nullable(isBoolean),
  points: isNumber,
  span_minutes: isNumber,
  reason: optional(isString),
});

const isWindowTotals = object<WindowTotals>({
  requests: isNumber,
  input: isNumber,
  output: isNumber,
  cache_read: isNumber,
  cache_creation: isNumber,
});

const isTokenExpiry = object<TokenExpiry>({
  state: isString,
  expires_at: nullable(isString),
  read_at: nullable(isString),
});

const isSetupBlock = object<SetupBlock>({
  state: isString,
  request_id: nullable(isString),
  run_id: nullable(isString),
  step: nullable(isString),
  reason: nullable(isString),
  updated_at: nullable(isString),
  token_set_at: nullable(isString),
  action: nullable(isString),
  identity: nullable(isString),
  wait_until: optional(nullable(isString)),
  source: optional(isTaskSource),
  token_expiry: optional(isTokenExpiry),
});

const isAccountConfig: Guard<AccountConfig> = object<AccountConfig>({
  alias: isString,
  email: isString,
  receiver_email: isString,
  access: isString,
  status: isString,
  token_ref: isString,
  proxy: object<ProxyConfig>({
    provider: isString,
    type: isString,
    direct: isBoolean,
    host: isString,
    port: isNumber,
    expected_egress_ip: isString,
    username_ref: isString,
    password_ref: isString,
  }),
  device_id: isString,
  account_uuid: isString,
  verify: object<Verification>({
    token_ok: nullable(isBoolean),
    egress_ok: nullable(isBoolean),
    probe_ok: nullable(isBoolean),
    at: optional(isString),
  }),
  rev: isNumber,
  created_at: isString,
  updated_at: isString,
  setup: optional(isSetupBlock),
});

const latency = {
  requests: isNumber,
  duration_p50_ms: isNumber,
  duration_p95_ms: isNumber,
};

const tokens = {
  input_tokens: isNumber,
  output_tokens: isNumber,
  cache_read_input_tokens: isNumber,
  cache_creation_input_tokens: isNumber,
};

const isDashboard: Guard<Dashboard> = object<Dashboard>({
  at: isString,
  soft: isNumber,
  hard: isNumber,
  health: object<Health>({
    status: isString,
    reasons: optional(arrayOf(isString)),
  }),
  gateway: object<Gateway>({
    provider: optional(isString),
    dashboards: optional(
      arrayOf(object<DashboardLink>({ provider: isString, url: isString })),
    ),
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
        quota_history: optional(isNumber),
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
    quota_refresh: optional(
      nullable(
        object<QuotaRefreshState>({
          running: isBoolean,
          last_started: optional(isString),
          last_finished: optional(isString),
          cooldown_until: optional(isString),
        }),
      ),
    ),
    setup: optional(
      nullable(
        object<GatewaySetup>({
          enabled: isBoolean,
          queued: isNumber,
          last_asked_at: nullable(isString),
        }),
      ),
    ),
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
      quota_history: optional(
        recordOf(
          arrayOf(object<QuotaPoint>({ at: isString, utilization: isNumber })),
        ),
      ),
      burn: optional(
        object<Burn>({
          "5h": isWindowBurn,
          "7d": isWindowBurn,
          by_machine: arrayOf(
            object<MachineBurn>({
              window: isString,
              machine: isString,
              share: isNumber,
              rate_per_hour: isNumber,
              estimate: isBoolean,
            }),
          ),
        }),
      ),
      totals: optional(
        object<Totals>({ "24h": isWindowTotals, "7d": isWindowTotals }),
      ),
      plan: nullable(isPlan),
      next_plan: nullable(isPlan),
      in_flight: isNumber,
      concurrency: isNumber,
      config: optional(isAccountConfig),
    }),
  ),
  unserved_accounts: optional(arrayOf(isAccountConfig)),
  accounts_editable: optional(isBoolean),
  write: optional(
    object<WriteState>({
      enabled: isBoolean,
      reason: optional(isString),
      can_create: isBoolean,
    }),
  ),
  proxy_ref_options: optional(
    arrayOf(
      object<ProxyRefOption>({
        username_ref: isString,
        password_ref: isString,
      }),
    ),
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
      client: optional(isString),
      client_version: optional(isString),
    }),
  ),
  idle_sessions: isNumber,
  usage: optional(
    arrayOf(
      object<UsagePoint>({
        hour: isString,
        alias: isString,
        client_machine: isString,
        kind: isString,
        requests: isNumber,
        bytes_up: isNumber,
        bytes_down: isNumber,
        errors: isNumber,
        limited: isNumber,
        input_tokens: optional(isNumber),
        output_tokens: optional(isNumber),
        cache_read_input_tokens: optional(isNumber),
        cache_creation_input_tokens: optional(isNumber),
      }),
    ),
  ),
  result_sources: optional(
    arrayOf(
      object<SourceCount>({
        kind: isString,
        result: isString,
        status: isNumber,
        count: isNumber,
        alias: isString,
        client_machine: isString,
        error: isBoolean,
      }),
    ),
  ),
  model_usage: optional(
    arrayOf(
      object<ModelUsage>({
        hour: isString,
        alias: isString,
        model: isString,
        requests: isNumber,
        ...tokens,
      }),
    ),
  ),
  clients: optional(
    arrayOf(
      object<ClientCount>({
        client_machine: isString,
        client: isString,
        live: isNumber,
        sessions: isNumber,
      }),
    ),
  ),
  client_hourly: optional(
    arrayOf(
      object<ClientHour>({
        hour: isString,
        client_machine: isString,
        client: isString,
        requests: isNumber,
      }),
    ),
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
  latency: optional(
    object<LatencyView>({
      hourly: arrayOf(object({ hour: isString, ...latency })),
      accounts: arrayOf(object({ alias: isString, ...latency })),
      machines: arrayOf(object({ client_machine: isString, ...latency })),
    }),
  ),
});

/** Parses the endpoint's text; throws when it is not the expected shape. */
export function parseDashboard(text: string): Dashboard {
  const value: unknown = JSON.parse(text);
  if (!isDashboard(value))
    throw new Error("dashboard.json does not have the shape this page reads");
  return value;
}
