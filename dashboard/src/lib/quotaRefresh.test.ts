import { expect, it } from "vitest";
import {
  refreshOffer,
  requestQuotaRefresh,
  shownRefusal,
} from "./quotaRefresh";

const now = new Date("2026-10-07T12:20:00Z");

it("offers the refresh only when no round runs and the wait is over", () => {
  expect(refreshOffer({ running: true }, now)).toEqual({
    enabled: false,
    note: "Refreshing every account…",
  });
  expect(
    refreshOffer(
      {
        running: false,
        last_finished: "2026-10-07T12:17:00Z",
        cooldown_until: "2026-10-07T12:22:00Z",
      },
      now,
    ),
  ).toEqual({ enabled: false, note: "Again in 2 min" });
  expect(
    refreshOffer(
      {
        running: false,
        last_finished: "2026-10-07T12:10:00Z",
        cooldown_until: "2026-10-07T12:15:00Z",
      },
      now,
    ),
  ).toEqual({ enabled: true, note: "Refreshed 10 min ago" });
  expect(refreshOffer({ running: false }, now)).toEqual({
    enabled: true,
    note: null,
  });
});

// A fetch that records its calls and answers status with headers.
function answering(status: number, headers: Record<string, string> = {}) {
  const calls: [string, RequestInit | undefined][] = [];
  const post: typeof fetch = (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    calls.push([url, init]);
    return Promise.resolve(new Response(null, { status, headers }));
  };
  return { post, calls };
}

it("posts to the page's own gateway with the action header", async () => {
  const { post, calls } = answering(202);
  expect(await requestQuotaRefresh(post)).toBeNull();
  expect(calls).toHaveLength(1);
  const [url, init] = calls[0] ?? [];
  expect(url).toBe("./quota/refresh");
  expect(init?.method).toBe("POST");
  expect(new Headers(init?.headers).get("X-Redcoast-Action")).toBe("refresh");
});

it("says why the gateway refused the refresh", async () => {
  expect(
    await requestQuotaRefresh(answering(429, { "Retry-After": "120" }).post),
  ).toBe("Refreshed recently; again in 2 min");
  expect(await requestQuotaRefresh(answering(403).post)).toBe(
    "Could not refresh: HTTP 403",
  );
  const failing = (() =>
    Promise.reject(new TypeError("network"))) as typeof fetch;
  expect(await requestQuotaRefresh(failing)).toBe(
    "Could not refresh: TypeError: network",
  );
});

it("shows a refusal only until the next reading of the gateway", () => {
  const refusal = {
    text: "Could not refresh: HTTP 403",
    at: "2026-10-07T12:20:00Z",
  };
  expect(shownRefusal(refusal, "2026-10-07T12:20:00Z")).toBe(
    "Could not refresh: HTTP 403",
  );
  expect(shownRefusal(refusal, "2026-10-07T12:20:30Z")).toBeNull();
  expect(shownRefusal(null, "2026-10-07T12:20:00Z")).toBeNull();
});
