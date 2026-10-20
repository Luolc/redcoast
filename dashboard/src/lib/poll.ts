/**
 * Runs load at most once at a time: run() while a load is in flight does
 * nothing, so a slow answer is waited out rather than cancelled by the next
 * scheduled one. A load that has not settled after timeoutMs is aborted
 * with a TimeoutError, so one that never settles blocks nothing for long;
 * stop() aborts the load in flight.
 */
export function poller(
  load: (signal: AbortSignal) => Promise<void>,
  timeoutMs: number,
): {
  run: () => void;
  stop: () => void;
} {
  let current: AbortController | null = null;
  return {
    run() {
      if (current !== null) return;
      const controller = new AbortController();
      current = controller;
      const signal = AbortSignal.any([
        controller.signal,
        AbortSignal.timeout(timeoutMs),
      ]);
      void load(signal).finally(() => {
        if (current === controller) current = null;
      });
    },
    stop() {
      current?.abort();
      current = null;
    },
  };
}

/** Whether a load's signal was aborted by stop(), not by its timeout. */
export function stopped(signal: AbortSignal): boolean {
  return (
    signal.aborted &&
    !(
      signal.reason instanceof DOMException &&
      signal.reason.name === "TimeoutError"
    )
  );
}
