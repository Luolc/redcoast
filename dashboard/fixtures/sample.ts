// Made-up data in the shape of GET /dashboard.json, for `pnpm dev` and the
// tests. Times are relative to now so ages and countdowns look live.
import type { Dashboard } from "../src/lib/types.ts";

export function sampleDashboard(now: Date): Dashboard {
  const at = (minutes: number) =>
    new Date(now.getTime() + minutes * 60_000).toISOString();
  const day = (back: number) => {
    const d = new Date(now);
    d.setUTCHours(0, 0, 0, 0);
    d.setUTCDate(d.getUTCDate() - back);
    return d.toISOString();
  };
  const counted = traffic(now);
  return {
    at: at(0),
    soft: 0.9,
    hard: 0.95,
    health: {
      status: "degraded",
      reasons: [
        "sample-ac: paused (upstream_429)",
        "sample-ad: egress IP not read",
      ],
    },
    gateway: {
      provider: "claude",
      dashboards: [{ provider: "codex", url: "http://127.0.0.1:7807/" }],
      version: "0.1.0",
      inventory_commit: "fedcba9876543210fedcba9876543210fedcba98",
      started: at(-26 * 60),
      wal_bytes: 4_194_304,
      retention: {
        at: at(-25 * 60),
        pruned: {
          traffic: 0,
          binding_history: 0,
          sessions: 0,
          quota_history: 0,
        },
        vacuumed: false,
      },
      backup: { last_success: at(-17) },
      open_tunnels: 7,
      tunnel_limit: 256,
    },
    db_bytes: 12_582_912,
    accounts: [
      {
        alias: "sample-ab",
        email: "ab@example.test",
        expected_egress_ip: "192.0.2.1",
        egress_check: { at: at(-4), outcome: "ok" },
        bound_sessions: 9,
        active_sessions: 6,
        quota: {
          "5h": {
            utilization: 0.42,
            status: "allowed",
            reset_at: at(130),
            observed_at: at(-1),
            source: "response",
          },
          "7d": {
            utilization: 0.92,
            status: "allowed_warning",
            reset_at: at(3 * 24 * 60),
            observed_at: at(-1),
            source: "response",
          },
        },
        quota_history: {
          "5h": quotaHistory(now, 5 * 60, 15, 0.42, 0.02),
          "7d": quotaHistory(now, 7 * 24 * 60, 60, 0.92, 0.005),
        },
        plan: {
          name: "max-20x",
          effective_from: "1970-01-01T00:00:00Z",
          effective_to: null,
        },
        next_plan: null,
        in_flight: 3,
        concurrency: 32,
      },
      {
        alias: "sample-ac",
        email: "ac@example.test",
        expected_egress_ip: "192.0.2.2",
        egress_check: { at: at(-4), outcome: "ok" },
        paused_until: at(95),
        pause_reason: "upstream_429",
        bound_sessions: 0,
        active_sessions: 0,
        quota: {
          "5h": {
            utilization: 1,
            status: "rejected",
            reset_at: at(95),
            observed_at: at(-150),
            source: "429",
          },
        },
        plan: {
          name: "max-5x",
          effective_from: "1970-01-01T00:00:00Z",
          effective_to: day(-3),
        },
        next_plan: {
          name: "max-20x",
          effective_from: day(-3),
          effective_to: null,
        },
        in_flight: 0,
        concurrency: 32,
      },
      {
        alias: "sample-ad",
        email: "ad@example.test",
        expected_egress_ip: "192.0.2.3",
        egress_check: { at: at(-4), outcome: "unread" },
        bound_sessions: 2,
        active_sessions: 2,
        quota: {
          "5h": {
            utilization: 0.31,
            status: "allowed",
            reset_at: at(200),
            observed_at: at(-6),
            source: "response",
          },
          "7d": {
            utilization: 0.18,
            status: "allowed",
            reset_at: at(6 * 24 * 60),
            observed_at: at(-6),
            source: "response",
          },
        },
        plan: {
          name: "max-5x",
          effective_from: "1970-01-01T00:00:00Z",
          effective_to: null,
        },
        next_plan: null,
        in_flight: 1,
        concurrency: 8,
      },
    ],
    machines: [
      {
        name: "machine-a",
        created_at: at(-30 * 24 * 60),
        max_sessions: 12,
        live_sessions: 3,
        last_seen: at(-1),
      },
      {
        name: "machine-b",
        created_at: at(-9 * 24 * 60),
        max_sessions: 4,
        live_sessions: 1,
        last_seen: at(-6),
      },
      {
        name: "machine-c",
        created_at: at(-40 * 24 * 60),
        revoked_at: at(-3 * 24 * 60),
        max_sessions: 4,
        live_sessions: 0,
        last_seen: at(-3 * 24 * 60 - 10),
      },
    ],
    sessions: [
      {
        id: "K7Q2M4XA",
        client_machine: "machine-a",
        alias: "sample-ab",
        created_at: at(-28 * 60),
        last_seen: at(-27 * 60),
        client: "",
        client_version: "",
      },
      {
        id: "P3ZT8WQN",
        client_machine: "machine-a",
        alias: "sample-ab",
        created_at: at(-50),
        last_seen: at(-1),
        client: "claude-code",
        client_version: "2.1.0",
      },
      {
        id: "R5HD2LKE",
        client_machine: "machine-a",
        alias: "sample-ad",
        created_at: at(-200),
        last_seen: at(-14),
        client: "pi",
        client_version: "1.2.0",
      },
      {
        id: "W8MN4TQC",
        client_machine: "machine-b",
        alias: "sample-ad",
        created_at: at(-95),
        last_seen: at(-6),
        client: "codex-cli",
        client_version: "0.46.0",
      },
    ],
    idle_sessions: 1,
    ...counted,
    clients: [
      { client_machine: "machine-a", client: "", live: 1, sessions: 1 },
      {
        client_machine: "machine-a",
        client: "claude-code",
        live: 1,
        sessions: 6,
      },
      { client_machine: "machine-a", client: "pi", live: 1, sessions: 2 },
      {
        client_machine: "machine-b",
        client: "codex-cli",
        live: 1,
        sessions: 3,
      },
    ],
    client_hourly: clientHourly(counted.usage ?? []),
    model_usage: modelUsage(counted.usage ?? []),
    destinations: [
      {
        host: "github.com",
        port: 443,
        alias: "sample-ab",
        route: "direct",
        tunnels: 30,
        ok: 30,
        bytes_up: 61_440,
        bytes_down: 2_097_152,
        last: at(-3),
      },
      {
        host: "registry.npmjs.org",
        port: 443,
        alias: "sample-ab",
        route: "account",
        tunnels: 7,
        ok: 6,
        bytes_up: 20_480,
        bytes_down: 1_048_576,
        last: at(-40),
      },
    ],
    days: [6, 5, 4, 3, 2, 1, 0].map((back) => ({
      day: day(back),
      limited_inference: back === 0 ? 7 : 0,
      limited_connect: 0,
      tunnel_peak: 12 - back,
      session_peak: 9,
      leaked: 0,
    })),
    history: [
      { session: "P3ZT8WQN", alias: "sample-ab", from: at(-50), reason: "new" },
      {
        session: "W8MN4TQC",
        alias: "sample-ad",
        from: at(-90),
        reason: "quota_hard",
      },
      {
        session: "W8MN4TQC",
        alias: "sample-ac",
        from: at(-95),
        to: at(-90),
        reason: "new",
      },
      {
        session: "R5HD2LKE",
        alias: "sample-ad",
        from: at(-200),
        reason: "new",
      },
      {
        session: "K7Q2M4XA",
        alias: "sample-ab",
        from: at(-28 * 60),
        reason: "unavailable",
      },
    ],
    latency: {
      hourly: [
        {
          hour: day(0),
          requests: 405,
          duration_p50_ms: 8_420,
          duration_p95_ms: 41_900,
        },
      ],
      accounts: [
        {
          alias: "sample-ab",
          requests: 405,
          duration_p50_ms: 8_420,
          duration_p95_ms: 41_900,
        },
      ],
      machines: [
        {
          client_machine: "local",
          requests: 112,
          duration_p50_ms: 6_150,
          duration_p95_ms: 30_200,
        },
        {
          client_machine: "machine-a",
          requests: 293,
          duration_p50_ms: 9_310,
          duration_p95_ms: 44_800,
        },
      ],
    },
  };
}

// Requests per hour of the 24 h window, each source with its own made-up
// shape over the day; results are split from the same counts, so every
// table of the sample adds up to the others.
function traffic(
  now: Date,
): Pick<Dashboard, "usage" | "result_sources" | "hourly" | "results"> {
  const last = Math.floor(now.getTime() / 3_600_000) * 3_600_000;
  const sources = [
    { alias: "sample-ab", machine: "machine-a", kind: "inference", base: 220 },
    { alias: "sample-ab", machine: "machine-a", kind: "connect", base: 30 },
    { alias: "sample-ad", machine: "machine-b", kind: "inference", base: 90 },
    { alias: "", machine: "", kind: "inference", base: 2 },
  ];
  const usage: NonNullable<Dashboard["usage"]> = [];
  for (let i = 0; i <= 24; i++) {
    const hour = new Date(last - (24 - i) * 3_600_000).toISOString();
    const daytime =
      0.35 + 0.65 * Math.max(0, Math.sin(((i - 2) / 24) * 2 * Math.PI));
    for (const s of sources) {
      const requests = Math.round(s.base * daytime);
      if (requests === 0) continue;
      const failing =
        s.machine === ""
          ? requests
          : s.kind === "connect"
            ? 0
            : i % 7 === 3
              ? 9
              : i % 3;
      usage.push({
        hour,
        alias: s.alias,
        client_machine: s.machine,
        kind: s.kind,
        requests,
        bytes_up: requests * 24_576,
        bytes_down: requests * 6_144,
        errors: failing,
        limited:
          s.kind === "inference" && s.machine !== ""
            ? Math.floor(failing / 3)
            : 0,
        ...tokens(s.kind === "inference" && s.machine !== "" ? requests : 0),
      });
    }
  }
  const counts = new Map<string, number>();
  const count = (key: string, n: number) => {
    if (n > 0) counts.set(key, (counts.get(key) ?? 0) + n);
  };
  for (const u of usage) {
    const k = `${u.alias}\t${u.client_machine}\t${u.kind}`;
    const upstream = u.errors - u.limited;
    if (u.client_machine === "") {
      count(`${k}\tauth_failed\t0`, u.errors);
      continue;
    }
    count(`${k}\tok\t200`, u.requests - u.errors);
    count(`${k}\tlimited\t0`, u.limited);
    count(`${k}\tok\t529`, Math.ceil(upstream / 2));
    count(`${k}\tincomplete\t0`, Math.floor(upstream / 2));
  }
  const resultSources = [...counts].map(([key, n]) => {
    const [alias = "", machine = "", kind = "", result = "", status = "0"] =
      key.split("\t");
    return {
      alias,
      client_machine: machine,
      kind,
      result,
      status: Number(status),
      count: n,
      error: !(result === "ok" && Number(status) < 400),
    };
  });
  const results = new Map<string, Dashboard["results"][number]>();
  for (const r of resultSources) {
    const k = `${r.kind}\t${r.result}\t${String(r.status)}`;
    const into = results.get(k);
    if (into === undefined)
      results.set(k, {
        kind: r.kind,
        result: r.result,
        status: r.status,
        count: r.count,
      });
    else into.count += r.count;
  }
  const hourly = new Map<string, Dashboard["hourly"][number]>();
  for (const u of usage) {
    const k = `${u.hour}\t${u.alias}\t${u.kind}`;
    const into = hourly.get(k);
    if (into === undefined)
      hourly.set(k, {
        hour: u.hour,
        alias: u.alias,
        kind: u.kind,
        requests: u.requests,
        bytes_up: u.bytes_up,
        bytes_down: u.bytes_down,
      });
    else {
      into.requests += u.requests;
      into.bytes_up += u.bytes_up;
      into.bytes_down += u.bytes_down;
    }
  }
  return {
    usage,
    result_sources: resultSources,
    hourly: [...hourly.values()],
    results: [...results.values()],
  };
}

// The hourly requests by client program: machine-a's as claude-code's,
// machine-b's as codex-cli's; requests of no session are not counted.
function clientHourly(
  usage: NonNullable<Dashboard["usage"]>,
): NonNullable<Dashboard["client_hourly"]> {
  const programs: Record<string, string> = {
    "machine-a": "claude-code",
    "machine-b": "codex-cli",
  };
  const hourly = new Map<
    string,
    NonNullable<Dashboard["client_hourly"]>[number]
  >();
  for (const u of usage) {
    const client = programs[u.client_machine];
    if (client === undefined) continue;
    const k = `${u.hour}\t${u.client_machine}`;
    const into = hourly.get(k);
    if (into === undefined)
      hourly.set(k, {
        hour: u.hour,
        client_machine: u.client_machine,
        client,
        requests: u.requests,
      });
    else into.requests += u.requests;
  }
  return [...hourly.values()];
}

// Made-up token counts of a number of inference requests that reported usage.
function tokens(requests: number) {
  return {
    input_tokens: requests * 1_800,
    output_tokens: requests * 400,
    cache_read_input_tokens: requests * 52_000,
    cache_creation_input_tokens: requests * 3_000,
  };
}

// The hourly tokens by model: two thirds of each account's inference requests
// on one model and the rest on another.
function modelUsage(
  usage: NonNullable<Dashboard["usage"]>,
): NonNullable<Dashboard["model_usage"]> {
  const out: NonNullable<Dashboard["model_usage"]> = [];
  for (const u of usage) {
    if (u.kind !== "inference" || u.client_machine === "") continue;
    const main = Math.round((u.requests * 2) / 3);
    for (const [model, requests] of [
      ["claude-sample-large", main],
      ["claude-sample-small", u.requests - main],
    ] as const) {
      if (requests > 0)
        out.push({
          hour: u.hour,
          alias: u.alias,
          model,
          requests,
          ...tokens(requests),
        });
    }
  }
  return out;
}

// A quota history over the last span minutes, one reading every step minutes,
// rising by rise from one reading to the next and ending at end.
function quotaHistory(
  now: Date,
  span: number,
  step: number,
  end: number,
  rise: number,
): { at: string; utilization: number }[] {
  const points = [];
  for (let back = span - step; back >= 0; back -= step)
    points.push({
      at: new Date(now.getTime() - back * 60_000).toISOString(),
      utilization: Math.max(0, end - (back / step) * rise),
    });
  return points;
}
