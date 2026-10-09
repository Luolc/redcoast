import { expect, it } from "vitest";
import { byHour } from "./traffic";

it("puts each hour on one row, newest first, one column per account and kind", () => {
  const row = (
    hour: string,
    alias: string,
    kind: string,
    requests: number,
  ) => ({
    hour,
    alias,
    kind,
    requests,
    bytes_up: 0,
    bytes_down: 0,
  });
  const { columns, hours } = byHour([
    row("2026-10-07T10:00:00Z", "sample-b", "inference", 1),
    row("2026-10-07T11:00:00Z", "sample-a", "connect", 2),
    row("2026-10-07T10:00:00Z", "sample-a", "inference", 3),
  ]);
  expect(columns.map((c) => c.key)).toEqual([
    "sample-a\tconnect",
    "sample-a\tinference",
    "sample-b\tinference",
  ]);
  expect(hours.map((h) => h.hour)).toEqual([
    "2026-10-07T11:00:00Z",
    "2026-10-07T10:00:00Z",
  ]);
  expect(hours[1]?.cells.get("sample-a\tinference")?.requests).toBe(3);
  expect(hours[0]?.cells.has("sample-b\tinference")).toBe(false);
});
