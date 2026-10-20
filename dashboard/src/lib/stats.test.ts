import { describe, expect, it } from "vitest";
import {
  accountState,
  idleSessions,
  pausedForGood,
  windowTier,
  byMachine,
  errorKinds,
  errorRate,
  errorRateState,
  failures,
  hourSeries,
  pace,
  recentBindings,
  sessionState,
  total,
} from "./stats";
import type { Account, Quota, SourceCount, UsagePoint } from "./types";

function usage(
  hour: string,
  client_machine: string,
  requests: number,
  errors = 0,
  limited = 0,
): UsagePoint {
  return {
    hour,
    alias: "sample-ab",
    client_machine,
    kind: "inference",
    requests,
    bytes_up: 0,
    bytes_down: 0,
    errors,
    limited,
  };
}

describe("hourSeries", () => {
  it("has every hour the 24 h window touches, oldest first, with zeros", () => {
    const at = "2026-10-07T12:20:00Z";
    const series = hourSeries(
      [
        usage("2026-10-07T12:00:00Z", "machine-a", 5, 1),
        usage("2026-10-07T12:00:00Z", "machine-b", 2),
        usage("2026-10-06T12:00:00Z", "machine-a", 3),
      ],
      at,
    );
    expect(series).toHaveLength(25);
    expect(series[0]).toEqual({
      hour: "2026-10-06T12:00:00.000Z",
      requests: 3,
      errors: 0,
      limited: 0,
    });
    expect(series[24]?.requests).toBe(7);
    expect(series[24]?.errors).toBe(1);
    expect(series[12]?.requests).toBe(0);
  });

  it("leaves out an hour outside the window", () => {
    const series = hourSeries(
      [usage("2026-10-06T11:00:00Z", "machine-a", 5)],
      "2026-10-07T12:20:00Z",
    );
    expect(total(series).requests).toBe(0);
  });
});

it("sums each client machine, most requests first", () => {
  const rows = [
    usage("2026-10-07T11:00:00Z", "machine-b", 2, 1),
    usage("2026-10-07T11:00:00Z", "machine-a", 5, 0),
    usage("2026-10-07T12:00:00Z", "machine-b", 4, 2, 1),
    usage("2026-10-07T12:00:00Z", "", 1, 1),
  ];
  expect(byMachine(rows)).toEqual([
    { machine: "machine-b", requests: 6, errors: 3, limited: 1 },
    { machine: "machine-a", requests: 5, errors: 0, limited: 0 },
    { machine: "", requests: 1, errors: 1, limited: 0 },
  ]);
});

it("rates errors per request, 0 with no requests, and names the rate", () => {
  expect(errorRate({ requests: 200, errors: 3, limited: 0 })).toBe(0.015);
  expect(errorRate({ requests: 0, errors: 0, limited: 0 })).toBe(0);
  expect(errorRateState(0.0099).word).toBe("Normal");
  expect(errorRateState(0.01).word).toBe("Elevated");
  expect(errorRateState(0.02).word).toBe("High");
});

function source(
  result: string,
  status: number,
  count: number,
  error: boolean,
  client_machine = "machine-a",
): SourceCount {
  return {
    alias: "sample-ab",
    client_machine,
    kind: "inference",
    result,
    status,
    count,
    error,
  };
}

const sources = [
  source("ok", 200, 90, false),
  source("ok", 529, 4, true),
  source("ok", 529, 2, true, "machine-b"),
  source("limited", 0, 3, true),
  source("auth_failed", 0, 1, true, ""),
];

it("splits errors into the upstream's, the gateway's limits and the rest", () => {
  expect(errorKinds(sources)).toEqual({ upstream: 6, limited: 3, other: 1 });
});

it("sums failures over machines, most first, and counts the answered", () => {
  const { answered, failed } = failures(sources);
  expect(answered).toBe(90);
  expect(failed.map((f) => [f.result, f.status, f.count])).toEqual([
    ["ok", 529, 6],
    ["limited", 0, 3],
    ["auth_failed", 0, 1],
  ]);
});

describe("pace", () => {
  const now = new Date("2026-10-07T12:00:00Z");
  const quota = (utilization: number, reset_at?: string): Quota => ({
    utilization,
    status: "allowed",
    observed_at: "2026-10-07T11:59:00Z",
    source: "response",
    ...(reset_at === undefined ? {} : { reset_at }),
  });

  it("compares use with an even pace through the window", () => {
    // 2 of 5 hours passed: an even pace is 40%.
    expect(pace(quota(0.48, "2026-10-07T15:00:00Z"), "5h", now)).toEqual({
      elapsed: 0.4,
      by: 8,
    });
    // 1 of 7 days passed.
    expect(pace(quota(0.1, "2026-10-13T12:00:00Z"), "7d", now)?.by).toBe(-4);
  });

  it("has none without a reset ahead or for an unknown window", () => {
    expect(pace(quota(0.5), "5h", now)).toBeNull();
    expect(pace(quota(0.5, "2026-10-07T12:00:00Z"), "5h", now)).toBeNull();
    expect(pace(quota(0.5, "2026-10-07T13:00:00Z"), "1h", now)).toBeNull();
  });
});

describe("accountState", () => {
  const now = new Date("2026-10-07T12:00:00Z");
  const base: Account = {
    alias: "sample-ab",
    email: "ab@example.test",
    expected_egress_ip: "192.0.2.1",
    bound_sessions: 0,
    active_sessions: 0,
    quota: {},
    plan: {
      name: "max-5x",
      effective_from: "1970-01-01T00:00:00Z",
      effective_to: null,
    },
    next_plan: null,
    in_flight: 0,
    concurrency: 4,
  };
  const q = (
    utilization: number,
    reset_at = "2026-10-07T14:00:00Z",
    status = "allowed",
  ): Quota => ({
    utilization,
    status,
    reset_at,
    observed_at: "2026-10-07T11:59:00Z",
    source: "response",
  });
  const word = (a: Account) => accountState(a, 0.9, 0.95, now).word;

  it("reads the highest window against the thresholds, from soft and hard on", () => {
    expect(word({ ...base, quota: { "5h": q(0.89) } })).toBe("Available");
    expect(word({ ...base, quota: { "5h": q(0.2), "7d": q(0.9) } })).toBe(
      "Near limit",
    );
    expect(word({ ...base, quota: { "5h": q(0.95) } })).toBe("Over hard limit");
  });

  it("reads windows as selection does: a passed reset unread, rejected as hard", () => {
    const passed = q(1, "2026-10-07T12:00:00Z");
    expect(windowTier(passed, 0.9, 0.95, now)).toBe("unread");
    expect(word({ ...base, quota: { "5h": passed } })).toBe("Available");
    const rejected = q(0.6, "2026-10-07T14:00:00Z", "rejected");
    expect(windowTier(rejected, 0.9, 0.95, now)).toBe("exhausted");
    expect(word({ ...base, quota: { "5h": rejected } })).toBe(
      "Over hard limit",
    );
  });

  it("puts a pause, then a missing plan, before quota", () => {
    const paused = {
      ...base,
      plan: null,
      paused_until: "2026-10-07T13:00:00Z",
      quota: { "5h": q(1) },
    };
    expect(
      accountState(
        { ...paused, pause_reason: "egress_ip_mismatch" },
        0.9,
        0.95,
        now,
      ),
    ).toEqual({ tone: "bad", word: "Paused · egress mismatch" });
    expect(
      accountState({ ...paused, pause_reason: "for a test" }, 0.9, 0.95, now),
    ).toEqual({ tone: "off", word: "Paused" });
    expect(word({ ...base, plan: null, quota: { "5h": q(1) } })).toBe(
      "No plan",
    );
  });

  it("tells a pause with no end from one with an end", () => {
    expect(pausedForGood("9999-12-31T23:59:59Z")).toBe(true);
    expect(pausedForGood("2026-10-07T13:00:00Z")).toBe(false);
  });
});

it("counts a session idle from exactly 24 h without a request", () => {
  const now = new Date("2026-10-08T12:00:00Z");
  const s = (last_seen: string) => ({
    id: "K7Q2M4XA",
    client_machine: "machine-a",
    alias: "",
    created_at: "2026-10-01T10:00:00Z",
    last_seen,
  });
  expect(
    idleSessions(
      [s("2026-10-07T12:00:00Z"), s("2026-10-07T12:00:00.001Z")],
      now,
    ),
  ).toBe(1);
});

it("tells working sessions from quiet ones at 2 min", () => {
  const now = new Date("2026-10-07T12:00:00Z");
  const s = (last_seen: string) => ({
    id: "K7Q2M4XA",
    client_machine: "machine-a",
    alias: "",
    created_at: "2026-10-07T10:00:00Z",
    last_seen,
  });
  expect(sessionState(s("2026-10-07T11:58:01Z"), now).word).toBe("Working");
  expect(sessionState(s("2026-10-07T11:58:00Z"), now).word).toBe("Quiet");
});

it("counts the bindings of the last 24 h and the rebinds among them", () => {
  const now = new Date("2026-10-07T12:00:00Z");
  const h = (from: string, reason: string) => ({
    session: "K7Q2M4XA",
    alias: "sample-ab",
    from,
    reason,
  });
  expect(
    recentBindings(
      [
        h("2026-10-07T11:00:00Z", "new"),
        h("2026-10-07T10:00:00Z", "quota_hard"),
        h("2026-10-06T12:00:00Z", "new"),
      ],
      now,
    ),
  ).toEqual({ all: 2, rebound: 1 });
});
