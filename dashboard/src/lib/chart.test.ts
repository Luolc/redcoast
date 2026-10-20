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
  const readings = [
    { at: at(0), utilization: 0.1 },
    { at: at(5), utilization: 0.1 },
    { at: at(10), utilization: 0.3 },
  ];
  expect(steps(readings)).toEqual([
    [
      [t(0), 0.1],
      [t(5), 0.1],
      [t(10), 0.1],
      [t(10), 0.3],
    ],
  ]);
  expect(steps([])).toEqual([]);
});

it("ends a run at a reset between two readings, and starts the next at the reading after it", () => {
  const at = (m: number) => new Date(Date.UTC(2026, 9, 7, 12, m)).toISOString();
  const t = (m: number) => Date.UTC(2026, 9, 7, 12, m);
  const readings = [
    { at: at(0), utilization: 0.9 },
    { at: at(60), utilization: 0.05 },
  ];
  expect(steps(readings, t(20))).toEqual([
    [
      [t(0), 0.9],
      [t(20), 0.9],
    ],
    [[t(60), 0.05]],
  ]);
  // A reset at a reading's time ends the run before that reading.
  expect(steps(readings, t(60))).toEqual([
    [
      [t(0), 0.9],
      [t(60), 0.9],
    ],
    [[t(60), 0.05]],
  ]);
  // A reset before every reading or after the last one cuts nothing.
  expect(steps(readings, t(-5))).toEqual(steps(readings));
  expect(steps(readings, t(90))).toEqual(steps(readings));
});
