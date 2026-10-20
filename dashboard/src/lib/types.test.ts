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
