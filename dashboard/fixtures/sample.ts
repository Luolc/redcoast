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
  return {
    at: at(0),
    soft: 0.8,
    hard: 0.9,
    health: {
      status: "degraded",
      reasons: ["sample-ac: paused (upstream_429)"],
    },
    gateway: {
      binary_commit: "0123456789abcdef0123456789abcdef01234567",
      inventory_commit: "fedcba9876543210fedcba9876543210fedcba98",
      started: at(-26 * 60),
      wal_bytes: 4_194_304,
      retention: {
        at: at(-25 * 60),
        pruned: { traffic: 0, binding_history: 0, sessions: 0 },
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
            utilization: 0.83,
            status: "allowed_warning",
            reset_at: at(3 * 24 * 60),
            observed_at: at(-1),
            source: "response",
          },
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
    ],
    machines: [
      {
        name: "machine-a",
        created_at: at(-30 * 60),
        max_sessions: 12,
        live_sessions: 9,
        last_seen: at(-1),
      },
    ],
    sessions: [
      {
        id: "K7Q2M4XA",
        client_machine: "machine-a",
        alias: "sample-ab",
        created_at: at(-28 * 60),
        last_seen: at(-27 * 60),
      },
      {
        id: "P3ZT8WQN",
        client_machine: "machine-a",
        alias: "sample-ab",
        created_at: at(-50),
        last_seen: at(-1),
      },
    ],
    idle_sessions: 1,
    hourly: [
      {
        hour: day(0),
        alias: "sample-ab",
        kind: "inference",
        requests: 412,
        bytes_up: 9_830_400,
        bytes_down: 2_202_009,
      },
      {
        hour: day(0),
        alias: "sample-ab",
        kind: "connect",
        requests: 37,
        bytes_up: 81_920,
        bytes_down: 3_145_728,
      },
    ],
    results: [
      { kind: "connect", result: "ok", status: 200, count: 37 },
      { kind: "inference", result: "ok", status: 200, count: 405 },
      { kind: "inference", result: "limited", status: 0, count: 7 },
    ],
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
        session: "K7Q2M4XA",
        alias: "sample-ab",
        from: at(-28 * 60),
        reason: "unavailable",
      },
    ],
  };
}
