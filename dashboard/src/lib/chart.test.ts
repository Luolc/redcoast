import { expect, it } from "vitest";
import { runs } from "./chart";

it("breaks a series at each null and keeps a lone value as its own run", () => {
  expect(runs([1, null, 2, 3, null, null, 4], (_v, i) => i)).toEqual([
    [0],
    [2, 3],
    [6],
  ]);
  expect(runs([null, null], (v) => v)).toEqual([]);
});
