import { expect, it } from "vitest";
import { poller } from "./poll";

function pending() {
  const signals: AbortSignal[] = [];
  const finishes: (() => void)[] = [];
  const load = (signal: AbortSignal) =>
    new Promise<void>((resolve) => {
      signals.push(signal);
      finishes.push(resolve);
    });
  return { load, signals, finishes };
}

it("waits out a load in flight instead of starting or cancelling one", async () => {
  const { load, signals, finishes } = pending();
  const p = poller(load);
  p.run();
  p.run();
  expect(signals).toHaveLength(1);
  expect(signals[0]?.aborted).toBe(false);
  finishes[0]?.();
  await Promise.resolve();
  await Promise.resolve();
  p.run();
  expect(signals).toHaveLength(2);
});

it("cancels the load in flight on stop, and runs again after", () => {
  const { load, signals } = pending();
  const p = poller(load);
  p.run();
  p.stop();
  expect(signals[0]?.aborted).toBe(true);
  p.run();
  expect(signals).toHaveLength(2);
});
