import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import { sampleDashboard } from "../fixtures/sample.ts";
import { Page } from "./App";
import { Sidebar } from "./components/Shell";
import { HourBars, HourLines } from "./components/charts";
import {
  formatCount,
  formatMs,
  formatPercent,
  formatTokens,
} from "./lib/format";
import { durationSeries, hourSeries } from "./lib/stats";
import type { Route } from "./lib/route";
import appCss from "./styles/app.css?raw";
import tokensCss from "./styles/tokens.css?raw";

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

// The declarations of the rule whose selector is exactly sel.
function rule(css: string, sel: string): string {
  const at = css.indexOf(`\n${sel} {`);
  if (at < 0) throw new Error(`no rule ${sel}`);
  return css.slice(at, css.indexOf("}", at));
}

it("lets every page fill the main area of a wide screen", () => {
  const rem = (name: string) =>
    Number(new RegExp(`${name}: ([\\d.]+)rem;`).exec(tokensCss)?.[1]);
  const base = Number(/--t-font-size-base: (\d+)px;/.exec(tokensCss)?.[1]);
  // A 2560 px screen is filled up to the page's width.
  expect(
    (rem("--t-width-sidebar") + rem("--t-width-main")) * base,
  ).toBeGreaterThan(2560);
  expect(rule(appCss, ".content")).toContain("max-width: var(--t-width-main);");
  // Nothing in the page is held narrower than the page, and no section is
  // set apart to be wider than the rest.
  expect(appCss).not.toMatch(/\.content\s*>/);
  const views = [
    "overview",
    "clients",
    "sessions",
    "accounts",
    "exits",
    "monitor",
  ] as const;
  const classes = (view: (typeof views)[number]) => [
    ...new Set(
      [
        ...renderToStaticMarkup(
          <Page data={data} now={now} route={{ view }} />,
        ).matchAll(/<section class="([^"]*)"/g),
      ].map((m) => m[1]),
    ),
  ];
  expect(Object.fromEntries(views.map((v) => [v, classes(v)]))).toEqual(
    Object.fromEntries(views.map((v) => [v, ["section"]])),
  );
});

it("keeps a list's rows and the topbar to readable widths", () => {
  // A label and its bar or value stay within 40rem of each other.
  expect(tokensCss).toContain("--t-width-list: 40rem;");
  for (const sel of [".rank", ".kv"])
    expect(rule(appCss, sel)).toContain("max-width: var(--t-width-list);");
  // The topbar's contents are held to the page's width, as .content is.
  expect(rule(appCss, ".topbar")).toContain(
    "calc((100% - var(--t-width-main)) / 2 + var(--t-space-6))",
  );
});

it("lines up each quota row on its first line", () => {
  expect(rule(appCss, ".quota-table td")).toContain("vertical-align: top;");
  expect(rule(appCss, ".quota-table th")).toContain("vertical-align: bottom;");
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

it("counts each client machine and program on Clients and on its own page", () => {
  const code = (data.client_hourly ?? [])
    .filter(
      (h) => h.client_machine === "machine-a" && h.client === "claude-code",
    )
    .reduce((n, h) => n + h.requests, 0);
  const all = (data.client_hourly ?? []).reduce((n, h) => n + h.requests, 0);
  // The check reads one program's requests only if they are not all of them.
  expect(code).toBeGreaterThan(0);
  expect(code).not.toBe(all);
  const clients = page({ view: "clients" });
  // Machine · program, version, live sessions, sessions in 7 d, requests.
  expect(clients).toContain(
    `machine-a · claude-code2.1.016${formatCount(code)}`,
  );
  expect(clients).toContain(`All clients412${formatCount(all)}`);
  expect(clients).toContain("unknown, claude-code, pi");

  const one = page({ view: "client", arg: "machine-a/claude-code" });
  expect(one).toContain("version 2.1.0");
  expect(one).toContain(`${formatCount(code)}requests`);
  expect(one).toContain("P3ZT8WQN");
  expect(one).not.toContain("R5HD2LKE");
  expect(page({ view: "client", arg: "machine-a/" })).toContain("K7Q2M4XA");
  expect(page({ view: "client", arg: "machine-z/pi" })).toContain(
    "No such client on Claude.",
  );
});

it("shows no programs from a gateway that does not report them", () => {
  const older = {
    ...data,
    clients: undefined,
    client_hourly: undefined,
    sessions: data.sessions.map((s) => ({
      ...s,
      client: undefined,
      client_version: undefined,
    })),
  };
  const clients = page({ view: "clients" }, older);
  expect(clients).not.toContain("machine · program");
  expect(clients).not.toContain("Programs, 7 d");
  expect(clients).toContain("machine-a");
  const sessions = page({ view: "sessions" }, older);
  expect(sessions).not.toContain("Version");
  expect(sessions).not.toContain("unknown program");
  expect(page({ view: "sessions" })).toContain("machine-a · pi");
});

it("keeps Clients current in the sidebar on a client's page", () => {
  const html = renderToStaticMarkup(
    <Sidebar data={data} route={{ view: "client", arg: "machine-a/pi" }} />,
  );
  expect(html).toMatch(/href="#\/clients" aria-current="page"/);
});

it("says in words when the tunnels near their limit", () => {
  const near = {
    ...data,
    gateway: { ...data.gateway, open_tunnels: 200, tunnel_limit: 250 },
  };
  expect(page({ view: "monitor" }, near)).toContain("Near the tunnel limit");
  expect(page({ view: "monitor" })).not.toContain("Near the tunnel limit");
});

it("labels a count axis in whole numbers when every hour is zero", () => {
  const html = renderToStaticMarkup(
    <HourBars
      label="test"
      hours={[0, 1, 2].map((h) =>
        new Date(Date.UTC(2026, 9, 7, h)).toISOString(),
      )}
      values={[0, 0, 0]}
      tip={() => null}
    />,
  );
  const axis = [...html.matchAll(/text-anchor="end"[^>]*>([^<]*)</g)].map(
    (m) => m[1],
  );
  expect(axis).toEqual(["0", "1", "2"]);
});

it("gives a client's share of all clients' requests on its page", () => {
  const hourly = data.client_hourly ?? [];
  const pi = hourly
    .filter((h) => h.client_machine === "machine-a" && h.client === "pi")
    .reduce((n, h) => n + h.requests, 0);
  const all = hourly.reduce((n, h) => n + h.requests, 0);
  expect(pi).toBeGreaterThan(0);
  expect(page({ view: "client", arg: "machine-a/pi" })).toContain(
    `${formatPercent(pi / all, 1)} of all clients`,
  );
});

it("counts the rows of Clients in the sidebar", () => {
  const count = (d: typeof data) =>
    /href="#\/clients"[^]*?<span class="count">(\d+)</.exec(
      renderToStaticMarkup(<Sidebar data={d} route={{ view: "overview" }} />),
    )?.[1];
  // The sample has four machine and program rows on two active machines.
  expect(count(data)).toBe("4");
  expect(count({ ...data, clients: undefined, client_hourly: undefined })).toBe(
    "2",
  );
});

it("shows the tokens of the day on Overview and by model on Accounts", () => {
  const usage = data.usage ?? [];
  const input = usage.reduce((n, u) => n + (u.input_tokens ?? 0), 0);
  const output = usage.reduce((n, u) => n + (u.output_tokens ?? 0), 0);
  expect(formatTokens(input)).not.toBe(formatTokens(output));
  expect(page({ view: "overview" })).toContain(
    `Tokens ${formatTokens(input)} in · ${formatTokens(output)} out`,
  );
  const read = usage.reduce((n, u) => n + (u.cache_read_input_tokens ?? 0), 0);
  const written = usage.reduce(
    (n, u) => n + (u.cache_creation_input_tokens ?? 0),
    0,
  );
  expect(page({ view: "overview" })).toContain(
    `Cache ${formatTokens(read)} read · ${formatTokens(written)} written`,
  );

  const large = (data.model_usage ?? []).filter(
    (m) => m.model === "claude-sample-large",
  );
  const requests = large.reduce((n, m) => n + m.requests, 0);
  const largeIn = large.reduce((n, m) => n + m.input_tokens, 0);
  const largeOut = large.reduce((n, m) => n + m.output_tokens, 0);
  expect(page({ view: "accounts" })).toContain(
    `${formatCount(requests)}${formatTokens(largeIn)}${formatTokens(largeOut)}`,
  );
});

it("draws each window's quota history on Monitor's account cards", () => {
  const [ab] = data.accounts;
  const history = ab?.quota_history?.["5h"] ?? [];
  const last = history.at(-1);
  const first = history[0];
  if (ab === undefined || first === undefined || last === undefined)
    throw new Error("the sample has no quota history");
  const current = ab.quota["5h"];
  const readings =
    current !== undefined &&
    Date.parse(current.observed_at) > Date.parse(last.at)
      ? [...history, current]
      : history;
  const html = renderToStaticMarkup(
    <Page data={data} now={now} route={{ view: "monitor" }} />,
  );
  expect(html).toContain(
    `5 h window over the last 5 h: ${String(readings.length)} readings, from ${formatPercent(first.utilization)} to ${formatPercent(readings.at(-1)?.utilization ?? 0)}`,
  );
  expect(page({ view: "monitor" })).toContain("Pruned quota readings");
});

it("shows no tokens, models or quota history from a gateway that has none", () => {
  const older = {
    ...data,
    model_usage: undefined,
    usage: (data.usage ?? []).map((u) => ({
      ...u,
      input_tokens: undefined,
      output_tokens: undefined,
      cache_read_input_tokens: undefined,
      cache_creation_input_tokens: undefined,
    })),
    accounts: data.accounts.map((a) => ({ ...a, quota_history: undefined })),
  };
  expect(page({ view: "overview" }, older)).not.toContain("Tokens");
  expect(page({ view: "clients" }, older)).not.toContain("Tokens, 24 h");
  expect(page({ view: "accounts" }, older)).not.toContain("Models");
  const monitor = renderToStaticMarkup(
    <Page data={older} now={now} route={{ view: "monitor" }} />,
  );
  expect(monitor).not.toContain('class="curve"');
  expect(
    renderToStaticMarkup(
      <Page data={data} now={now} route={{ view: "monitor" }} />,
    ),
  ).toContain('class="curve"');
});

it("ends a quota history at the current reading when it is newer", () => {
  const [ab, ...rest] = data.accounts;
  const current = ab?.quota["5h"];
  if (ab === undefined || current === undefined)
    throw new Error("the sample has no 5 h reading");
  // Readings up to 30 min before the current one, ending below it.
  const kept = (ab.quota_history?.["5h"] ?? []).filter(
    (r) => Date.parse(r.at) <= Date.parse(current.observed_at) - 30 * 60_000,
  );
  const first = kept[0];
  expect(kept.at(-1)?.utilization).not.toBe(current.utilization);
  if (first === undefined) throw new Error("no reading kept");
  const earlier = {
    ...data,
    accounts: [
      { ...ab, quota_history: { ...ab.quota_history, "5h": kept } },
      ...rest,
    ],
  };
  expect(
    renderToStaticMarkup(
      <Page data={earlier} now={now} route={{ view: "monitor" }} />,
    ),
  ).toContain(
    `5 h window over the last 5 h: ${String(kept.length + 1)} readings, from ${formatPercent(first.utilization)} to ${formatPercent(current.utilization)}`,
  );
});

it("says how input is counted by the provider, not by the counts", () => {
  const zeroCache = {
    ...data,
    model_usage: (data.model_usage ?? []).map((m) => ({
      ...m,
      cache_read_input_tokens: 0,
      cache_creation_input_tokens: 0,
    })),
  };
  const claude = page({ view: "accounts" }, zeroCache);
  expect(claude).toContain("input leaves out the cache reads and writes");
  expect(claude).toContain("Cache read");
  const codex = page(
    { view: "accounts" },
    { ...zeroCache, gateway: { ...data.gateway, provider: "codex" } },
  );
  expect(codex).toContain("input includes cached tokens");
  expect(codex).not.toContain("Cache read");
});

it("gives each machine's tokens and each model's accounts", () => {
  const usage = data.usage ?? [];
  const machine = (name: string) => {
    const rows = usage.filter((u) => u.client_machine === name);
    const sum = (k: "input_tokens" | "output_tokens") =>
      rows.reduce((n, u) => n + (u[k] ?? 0), 0);
    return `${formatTokens(sum("input_tokens"))} · ${formatTokens(sum("output_tokens"))}`;
  };
  // The check reads each machine's tokens only if theirs differ.
  expect(machine("machine-a")).not.toBe(machine("machine-b"));
  const clients = page({ view: "clients" });
  expect(clients).toContain(machine("machine-a"));
  expect(clients).toContain(machine("machine-b"));

  const accounts = page({ view: "accounts" });
  const lists = ["claude-sample-large", "claude-sample-small"].map((model) => {
    const counts = new Map<string, number>();
    for (const m of data.model_usage ?? [])
      if (m.model === model)
        counts.set(m.alias, (counts.get(m.alias) ?? 0) + m.requests);
    return [...counts.entries()]
      .sort((a, b) => b[1] - a[1])
      .map(([alias, n]) => `${alias} ${formatCount(n)}`)
      .join(" · ");
  });
  expect(lists[0]).not.toBe(lists[1]);
  for (const list of lists) expect(accounts).toContain(list);
});

it("places the last reset and the first reading on the history's time axis", () => {
  const [ab] = data.accounts;
  const q = ab?.quota["5h"];
  const first = ab?.quota_history?.["5h"]?.[0];
  if (q?.reset_at === undefined || first === undefined)
    throw new Error("the sample has no 5 h history");
  const span = 5 * 3_600_000;
  const to = Date.parse(data.at);
  const from = to - span;
  // The card's curve before it is measured: 320 wide, 72 high.
  const x = (t: number) => ((t - from) / (to - from)) * 320;
  const y = (u: number) => 72 - u * 72;
  const html = renderToStaticMarkup(
    <Page data={data} now={now} route={{ view: "monitor" }} />,
  );
  expect(html).toContain(
    `class="reset" x1="${String(x(Date.parse(q.reset_at) - span))}"`,
  );
  expect(html).toContain(
    `class="used" points="${x(Date.parse(first.at)).toFixed(1)},${y(first.utilization).toFixed(1)} `,
  );
});
