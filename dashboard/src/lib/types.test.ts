import { expect, it } from "vitest";
import { sampleDashboard } from "../../fixtures/sample.ts";
import { parseDashboard } from "./types";

it("accepts the endpoint's shape", () => {
  const text = JSON.stringify(
    sampleDashboard(new Date("2026-10-07T12:00:00Z")),
  );
  expect(parseDashboard(text).accounts[1]?.next_plan?.name).toBe("max-20x");
});

it("refuses a shape that drifted", () => {
  const drifted = {
    ...sampleDashboard(new Date("2026-10-07T12:00:00Z")),
    days: [{ day: "2026-10-07" }],
  };
  expect(() => parseDashboard(JSON.stringify(drifted))).toThrow();
});

it("accepts a gateway that reports no version", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const older = {
    ...sample,
    gateway: { ...sample.gateway, version: undefined, binary_commit: "" },
  };
  expect(parseDashboard(JSON.stringify(older)).gateway.version).toBeUndefined();
});

it("accepts a gateway without the per-machine tables and latency", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const older = {
    ...sample,
    usage: undefined,
    result_sources: undefined,
    latency: undefined,
  };
  expect(parseDashboard(JSON.stringify(older)).latency).toBeUndefined();
});

it("refuses latency that drifted", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const drifted = {
    ...sample,
    latency: { ...sample.latency, hourly: [{ hour: "2026-10-07" }] },
  };
  expect(() => parseDashboard(JSON.stringify(drifted))).toThrow();
});

it("reads the provider and the other providers' dashboards", () => {
  const gateway = parseDashboard(
    JSON.stringify(sampleDashboard(new Date("2026-10-07T12:00:00Z"))),
  ).gateway;
  expect(gateway.provider).toBe("claude");
  expect(gateway.dashboards).toEqual([
    { provider: "codex", url: "http://127.0.0.1:7807/" },
  ]);
});

it("accepts a gateway that reports no provider or dashboards", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const older = {
    ...sample,
    gateway: { ...sample.gateway, provider: undefined, dashboards: undefined },
  };
  expect(
    parseDashboard(JSON.stringify(older)).gateway.dashboards,
  ).toBeUndefined();
});

it("refuses a dashboard link without a url", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const drifted = {
    ...sample,
    gateway: { ...sample.gateway, dashboards: [{ provider: "codex" }] },
  };
  expect(() => parseDashboard(JSON.stringify(drifted))).toThrow();
});

it("accepts a gateway that reports no client programs", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const older = {
    ...sample,
    sessions: sample.sessions.map((s) => ({
      ...s,
      client: undefined,
      client_version: undefined,
    })),
    clients: undefined,
    client_hourly: undefined,
  };
  const parsed = parseDashboard(JSON.stringify(older));
  expect(parsed.clients).toBeUndefined();
  expect(parsed.sessions[1]?.client).toBeUndefined();
});

it("refuses a client count that drifted", () => {
  const drifted = {
    ...sampleDashboard(new Date("2026-10-07T12:00:00Z")),
    clients: [{ client_machine: "machine-a", client: "pi" }],
  };
  expect(() => parseDashboard(JSON.stringify(drifted))).toThrow();
});

it("reads the tokens and the quota history", () => {
  const parsed = parseDashboard(
    JSON.stringify(sampleDashboard(new Date("2026-10-07T12:00:00Z"))),
  );
  expect(parsed.model_usage?.[0]?.model).toBe("claude-sample-large");
  expect(parsed.usage?.[0]?.input_tokens).toBeGreaterThan(0);
  expect(parsed.accounts[0]?.quota_history?.["5h"]?.at(-1)?.utilization).toBe(
    0.42,
  );
});

it("accepts a gateway that reports no tokens or quota history", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  const older = {
    ...sample,
    gateway: {
      ...sample.gateway,
      retention: {
        ...sample.gateway.retention,
        pruned: { traffic: 0, binding_history: 0, sessions: 0 },
      },
    },
    accounts: sample.accounts.map((a) => ({ ...a, quota_history: undefined })),
    usage: sample.usage?.map((u) => ({
      ...u,
      input_tokens: undefined,
      output_tokens: undefined,
      cache_read_input_tokens: undefined,
      cache_creation_input_tokens: undefined,
    })),
    model_usage: undefined,
  };
  const parsed = parseDashboard(JSON.stringify(older));
  expect(parsed.model_usage).toBeUndefined();
  expect(parsed.accounts[0]?.quota_history).toBeUndefined();
});

it("refuses model usage or a quota history that drifted", () => {
  const sample = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  for (const drifted of [
    { ...sample, model_usage: [{ hour: "2026-10-07", model: "m" }] },
    {
      ...sample,
      accounts: sample.accounts.map((a) => ({
        ...a,
        quota_history: { "5h": [{ at: "2026-10-07" }] },
      })),
    },
  ])
    expect(() => parseDashboard(JSON.stringify(drifted))).toThrow();
});

// A gateway older than this page still sends the top-level hourly and results,
// which the page no longer reads; a newer one leaves them out.
it("accepts a gateway with or without the top-level hourly and results", () => {
  const newer = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
  expect("hourly" in newer || "results" in newer).toBe(false);
  const older = {
    ...newer,
    hourly: [
      {
        hour: "2026-10-07T11:00:00Z",
        alias: "sample-ab",
        kind: "inference",
        requests: 3,
        bytes_up: 1,
        bytes_down: 2,
      },
    ],
    results: [{ kind: "inference", result: "ok", status: 200, count: 3 }],
  };
  for (const sample of [newer, older])
    expect(parseDashboard(JSON.stringify(sample)).at).toBe(newer.at);
});
