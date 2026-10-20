import { expect, it } from "vitest";
import {
  formatAgo,
  formatBytes,
  formatCount,
  formatMs,
  formatPercent,
  formatSpan,
  formatTime,
  formatUntil,
  LA,
} from "./format";

it("shows Los Angeles time with its daylight-saving name, and UTC as is", () => {
  expect(formatTime("2026-10-01T16:15:03Z", LA)).toBe(
    "2026-10-01 09:15:03 PDT",
  );
  expect(formatTime("2026-12-01T16:15:03Z", LA)).toBe(
    "2026-12-01 08:15:03 PST",
  );
  expect(formatTime("2026-10-02T03:05:00Z", LA, true)).toBe("10-01 20:05");
  expect(formatTime("2026-10-01T16:15:03Z", "UTC")).toBe(
    "2026-10-01 16:15:03 UTC",
  );
});

// The spaces between a number and its unit are non-breaking.
it("writes a span in its two largest units, a number never apart from its unit", () => {
  expect(formatSpan(45_000)).toBe("45\u00a0s");
  expect(formatSpan(185_000)).toBe("3\u00a0min");
  expect(formatSpan(2 * 3_600_000)).toBe("2\u00a0h");
  expect(formatSpan((2 * 60 + 10) * 60_000)).toBe("2\u00a0h 10\u00a0min");
  expect(formatSpan(3 * 86_400_000)).toBe("3\u00a0d");
  expect(formatSpan((3 * 24 + 4) * 3_600_000)).toBe("3\u00a0d 4\u00a0h");
  expect(formatSpan(-1)).toBe("0\u00a0s");
});

it("writes ages and countdowns, and a passed countdown as due", () => {
  const now = new Date("2026-10-07T12:00:00Z");
  expect(formatAgo("2026-10-07T11:53:00Z", now)).toBe("7\u00a0min ago");
  expect(formatUntil("2026-10-07T14:36:00Z", now)).toBe(
    "in 2\u00a0h 36\u00a0min",
  );
  expect(formatUntil("2026-10-07T12:00:00Z", now)).toBe("due now");
});

it("formats bytes, fractions and counts", () => {
  expect(formatBytes(512)).toBe("512\u00a0B");
  expect(formatBytes(1536)).toBe("1.5\u00a0KiB");
  expect(formatBytes(2 * 1024 ** 3)).toBe("2.0\u00a0GiB");
  expect(formatPercent(0.42)).toBe("42%");
  expect(formatPercent(0.0123, 1)).toBe("1.2%");
  expect(formatCount(6405)).toBe("6,405");
});

it("writes a request's duration in ms, s or min", () => {
  expect(formatMs(850)).toBe("850\u00a0ms");
  expect(formatMs(15_040)).toBe("15.0\u00a0s");
  expect(formatMs(65_000)).toBe("1\u00a0min 05\u00a0s");
});
