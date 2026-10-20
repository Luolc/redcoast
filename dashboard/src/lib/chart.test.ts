import { expect, it } from "vitest";
import { runs, steps } from "./chart";

it("breaks a series at each null and keeps a lone value as its own run", () => {
  expect(runs([1, null, 2, 3, null, null, 4], (_v, i) => i)).toEqual([
    [0],
    [2, 3],
    [6],
  ]);
  expect(runs([null, null], (v) => v)).toEqual([]);
});

it("holds each quota reading until the next, and steps only on a change", () => {
  const at = (m: number) => new Date(Date.UTC(2026, 9, 7, 12, m)).toISOString();
  const t = (m: number) => Date.UTC(2026, 9, 7, 12, m);
  expect(
    steps([
      { at: at(0), utilization: 0.1 },
      { at: at(5), utilization: 0.1 },
      { at: at(10), utilization: 0.3 },
    ]),
  ).toEqual([
    [t(0), 0.1],
    [t(5), 0.1],
    [t(10), 0.1],
    [t(10), 0.3],
  ]);
  expect(steps([])).toEqual([]);
});
