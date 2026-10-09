import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import { sampleDashboard } from "../../fixtures/sample.ts";
import { Accounts } from "./Accounts";
import { Machines, Sessions } from "./Clients";
import { Days, History } from "./Days";
import { Overview } from "./Overview";
import { Destinations, Hourly, Results } from "./Traffic";

const now = new Date("2026-10-07T12:00:00Z");
const data = sampleDashboard(now);

it("renders every section of the sample", () => {
  const html = renderToStaticMarkup(
    <>
      <Overview data={data} now={now} />
      <Accounts data={data} now={now} />
      <Days data={data} />
      <Destinations data={data} />
      <Results data={data} />
      <Hourly data={data} />
      <Machines data={data} now={now} />
      <Sessions data={data} now={now} />
      <History data={data} />
    </>,
  );
  for (const text of [
    "sample-ac: paused (upstream_429)",
    "ab@example.test",
    "registry.npmjs.org:443",
    "直连",
    "账号出口",
    "machine-a",
    "K7Q2M4XA",
    "泄漏 (确认)",
    "暂停",
    "已运行 1d 2h",
    "v0.1.0",
  ]) {
    expect(html).toContain(text);
  }
});

it("says so when a table is empty", () => {
  const empty = { ...data, sessions: [], destinations: [] };
  const html = renderToStaticMarkup(
    <>
      <Sessions data={empty} now={now} />
      <Destinations data={empty} />
    </>,
  );
  expect(html).toContain("没有活会话");
  expect(html).toContain("最近 7 天没有 CONNECT");
});
