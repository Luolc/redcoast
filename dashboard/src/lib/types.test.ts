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
