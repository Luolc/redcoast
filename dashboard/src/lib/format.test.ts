import { expect, it } from "vitest";
import {
  formatBytes,
  formatDuration,
  formatPercent,
  formatTime,
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

it("formats durations in their two largest units", () => {
  expect(formatDuration(45_000)).toBe("45s");
  expect(formatDuration(185_000)).toBe("3m 05s");
  expect(formatDuration((2 * 60 + 10) * 60_000)).toBe("2h 10m");
  expect(formatDuration((3 * 24 + 4) * 3_600_000)).toBe("3d 4h");
  expect(formatDuration(-1)).toBe("0s");
});

it("formats bytes and fractions", () => {
  expect(formatBytes(512)).toBe("512 B");
  expect(formatBytes(1536)).toBe("1.5 KiB");
  expect(formatBytes(2 * 1024 ** 3)).toBe("2.0 GiB");
  expect(formatPercent(0.425)).toBe("43%");
});
