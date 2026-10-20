import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import { sampleDashboard } from "../fixtures/sample.ts";
import { Page } from "./App";
import { Sidebar } from "./components/Shell";
import { HourLines } from "./components/charts";
import { formatCount, formatMs } from "./lib/format";
import { durationSeries, hourSeries } from "./lib/stats";
import type { Route } from "./lib/route";

const now = new Date("2026-10-07T12:20:00Z");
const data = sampleDashboard(now);

// The page's text, without its markup.
function page(route: Route, d = data): string {
  return renderToStaticMarkup(<Page data={d} now={now} route={route} />)
    .replace(/<[^>]*>/g, "")
    .replaceAll("&amp;", "&");
}

it("renders each page of the sample", () => {
  const expected: [Route, string[]][] = [
    [
      { view: "overview" },
      [
        "Degraded",
        "2 of 3 available",
        "1 not confirmed",
        "machine-b",
        "No session",
        "Pace",
      ],
    ],
    [
      { view: "clients" },
      [
        "44.8\u00a0s · 9.3\u00a0s",
        "machine-a",
        "Revoked",
        "came with no session",
      ],
    ],
    [
      { view: "sessions" },
      ["P3ZT8WQN", "Account over its hard limit", "Quiet"],
    ],
    [{ view: "sessions", arg: "machine-b" }, ["W8MN4TQC", "on machine-b"]],
    [{ view: "accounts" }, ["ab@example.test", "max-20x from"]],
    [{ view: "exits" }, ["IP not read", "registry.npmjs.org:443", "Direct"]],
    [
      { view: "monitor" },
      [
        "Requests this hour",
        "p95 this hour",
        "p95 41.9\u00a0s",
        "tunnels open",
        "5 h window",
        "Upstream server error",
        "v0.1.0",
      ],
    ],
  ];
  for (const [route, texts] of expected) {
    const html = page(route);
    for (const text of texts)
      expect(html, `${route.view}: ${text}`).toContain(text);
  }
});

it("filters sessions to one machine", () => {
  const html = page({ view: "sessions", arg: "machine-b" });
  expect(html).not.toContain("P3ZT8WQN");
});

it("says so when a gateway sends no usage or tables are empty", () => {
  const older = { ...data, usage: undefined, result_sources: undefined };
  expect(page({ view: "overview" }, older)).toContain("No hourly usage.");
  const clients = page({ view: "clients" }, older);
  expect(clients).toContain("does not report requests by machine");
  // machine-a: live / max sessions, then no request count.
  expect(clients).toContain("3 / 12—");
  expect(page({ view: "monitor" }, older)).toContain("No result counts.");
  const empty = { ...data, sessions: [], destinations: [], history: [] };
  expect(page({ view: "sessions" }, empty)).toContain("No live sessions.");
  expect(page({ view: "exits" }, empty)).toContain(
    "No CONNECT in the last 7 days.",
  );
});

it("offers the other providers' dashboards only when the gateway lists them", () => {
  const route: Route = { view: "monitor" };
  const listed = renderToStaticMarkup(<Sidebar data={data} route={route} />);
  expect(listed).toContain('aria-controls="provider-links"');
  expect(listed).toContain("Claude");
  const alone = renderToStaticMarkup(
    <Sidebar
      data={{ ...data, gateway: { ...data.gateway, dashboards: [] } }}
      route={route}
    />,
  );
  expect(alone).not.toContain("aria-controls");
});

it("reads a pause with no end as until resumed", () => {
  const [first, ...rest] = data.accounts;
  if (first === undefined) throw new Error("the sample has no account");
  const forGood = {
    ...data,
    accounts: [
      {
        ...first,
        paused_until: "9999-12-31T23:59:59Z",
        pause_reason: "manual",
      },
      ...rest,
    ],
  };
  const html = page({ view: "accounts" }, forGood);
  expect(html).toContain("until resumed");
  expect(page({ view: "accounts" })).not.toContain("until resumed");
});

it("shows the current hour's figures in Monitor's Now strip", () => {
  const hours = hourSeries(data.usage ?? [], data.at);
  const durations = durationSeries(data.latency?.hourly ?? [], data.at);
  const [last, before] = [hours.at(-1), hours.at(-2)];
  const [p95, p50] = [durations.at(-1)?.p95, durations.at(-1)?.p50];
  if (last === undefined || before === undefined || p95 == null || p50 == null)
    throw new Error("the sample has no current hour");
  // The check tells the current hour from the one before only if they differ.
  expect(last.requests).not.toBe(before.requests);
  const text = page({ view: "monitor" });
  expect(text).toContain(`Requests this hour${formatCount(last.requests)}`);
  expect(text).toContain(`p95 this hour${formatMs(p95)}`);
  expect(formatMs(p95)).not.toBe(formatMs(p50));
  expect(durations.at(-2)?.p95).not.toBe(p95);
});

it("breaks a duration line at an hour without a value", () => {
  const html = renderToStaticMarkup(
    <HourLines
      label="test"
      hours={[0, 1, 2, 3, 4].map((h) =>
        new Date(Date.UTC(2026, 9, 7, h)).toISOString(),
      )}
      lines={[{ name: "p95", values: [1000, null, 2000, 3000, null] }]}
      formatAxis={formatMs}
      tip={() => null}
    />,
  );
  expect(html.match(/<polyline/g)).toHaveLength(1);
  expect(html.match(/class="dot-1"/g)).toHaveLength(1);
});
