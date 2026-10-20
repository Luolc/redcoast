/**
 * Runs load at most once at a time: run() while a load is in flight does
 * nothing, so a slow answer is waited out rather than cancelled by the next
 * scheduled one; stop() cancels the load in flight.
 */
export function poller(load: (signal: AbortSignal) => Promise<void>): {
  run: () => void;
  stop: () => void;
} {
  let current: AbortController | null = null;
  return {
    run() {
      if (current !== null) return;
      const controller = new AbortController();
      current = controller;
      void load(controller.signal).finally(() => {
        if (current === controller) current = null;
      });
    },
    stop() {
      current?.abort();
      current = null;
    },
  };
}
