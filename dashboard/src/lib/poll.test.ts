import { expect, it } from "vitest";
import { poller, stopped } from "./poll";

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
  const p = poller(load, 60_000);
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
  const p = poller(load, 60_000);
  p.run();
  p.stop();
  expect(signals[0]?.aborted).toBe(true);
  p.run();
  expect(signals).toHaveLength(2);
});

it("gives up a load that does not settle, as a timeout, not a stop", async () => {
  const { load, signals } = pending();
  const p = poller(load, 20);
  p.run();
  await new Promise((resolve) => setTimeout(resolve, 60));
  const [first] = signals;
  expect(first?.aborted).toBe(true);
  expect(first === undefined ? true : stopped(first)).toBe(false);
  p.stop();
});

it("tells a stop from a timeout", () => {
  const { load, signals } = pending();
  const p = poller(load, 60_000);
  p.run();
  p.stop();
  const [first] = signals;
  expect(first === undefined ? false : stopped(first)).toBe(true);
});
