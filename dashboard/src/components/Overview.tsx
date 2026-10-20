import { Fragment } from "react";
import {
  formatClock,
  formatMs,
  formatCount,
  formatPercent,
  formatSpan,
} from "../lib/format";
import { routeHref } from "../lib/route";
import {
  accountState,
  available,
  byMachine,
  errorKinds,
  errorRate,
  errorRateState,
  healthState,
  durationSeries,
  hourSeries,
  total,
  type HourCounts,
} from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName } from "../lib/words";
import { AccountsTable } from "./Accounts";
import { HourBars, HourLines } from "./charts";
import { Empty, PageHead, Section, Share, Status } from "./ui";

function machineLabel(machine: string): string {
  return machine === "" ? "No session" : machine;
}

function hourTitle(h: { hour: string }): string {
  const end = new Date(Date.parse(h.hour) + 3_600_000).toISOString();
  return `${formatClock(h.hour)}–${formatClock(end)} UTC`;
}

function peak(
  hours: HourCounts[],
  key: "requests" | "errors",
): HourCounts | undefined {
  return hours.reduce<HourCounts | undefined>(
    (best, h) => (best === undefined || h[key] > best[key] ? h : best),
    undefined,
  );
}

/** Requests and errors per hour over the gateway's 24 h window, with their figures. */
function Traffic({ data }: { data: Dashboard }) {
  if (data.usage === undefined)
    return (
      <Empty title="No hourly usage.">
        This gateway is older than the page and does not report usage by hour.
      </Empty>
    );
  const hours = hourSeries(data.usage, data.at);
  const all = total(hours);
  const rate = errorRate(all);
  const machines = byMachine(data.usage);
  const worst = peak(hours, "errors");
  const kinds = errorKinds(data.result_sources ?? []);
  const highest = machines
    .filter((m) => m.machine !== "" && m.errors > 0)
    .map((m) => ({ ...m, rate: errorRate(m) }))
    .sort((a, b) => b.rate - a.rate)[0];
  const labels = hours.map((h) => h.hour);
  const top = peak(hours, "requests");
  return (
    <div className="red">
      <div className="red-row">
        <div className="red-chart">
          <div className="title">
            Requests<span className="key">per hour, UTC</span>
          </div>
          <HourBars
            label={`Requests per hour over the last 24 h: ${formatCount(all.requests)} in all${top === undefined || top.requests === 0 ? "" : `, most at ${formatClock(top.hour)} UTC with ${formatCount(top.requests)}`}`}
            hours={labels}
            values={hours.map((h) => h.requests)}
            tip={(i) => {
              const h = hours[i];
              return h === undefined ? null : (
                <>
                  <b>{hourTitle(h)}</b>
                  <br />
                  Requests: {formatCount(h.requests)}
                </>
              );
            }}
          />
        </div>
        <div className="red-figs">
          <div className="big-row">
            <div className="big">{formatCount(all.requests)}</div>
            <div className="figs-sub">
              requests
              <br />
              {formatCount(all.requests / 24)} / h
            </div>
          </div>
          <div className="rank" aria-label="Requests by client machine">
            {machines.map((m) => (
              <div className="rank-row" key={m.machine}>
                <span className="who">{machineLabel(m.machine)}</span>
                <Share part={m.requests} whole={all.requests} />
                <span className="num">{formatCount(m.requests)}</span>
              </div>
            ))}
          </div>
        </div>
      </div>
      <div className="red-row">
        <div className="red-chart">
          <div className="title">
            Errors
            <span className="key">
              <span>
                <span className="sw sw-bad" />
                errors per hour, UTC
              </span>
            </span>
          </div>
          <HourBars
            label={`Errors per hour over the last 24 h: ${formatCount(all.errors)} in all`}
            hours={labels}
            values={hours.map((h) => h.errors)}
            bad
            tip={(i) => {
              const h = hours[i];
              return h === undefined ? null : (
                <>
                  <b>{hourTitle(h)}</b>
                  <br />
                  Errors: {formatCount(h.errors)}
                  <br />
                  Rate: {formatPercent(errorRate(h), 1)}
                </>
              );
            }}
          />
        </div>
        <div className="red-figs">
          <div className="big-row">
            <div className="big">{formatPercent(rate, 1)}</div>
            <Status state={errorRateState(rate)} pill />
          </div>
          <div className="figs-sub">
            <a
              className="text-link"
              href={routeHref({ view: "monitor", arg: "failures" })}
            >
              <span className="num">{formatCount(all.errors)}</span> errors
            </a>
            {worst === undefined || worst.errors === 0 ? null : (
              <>
                ; worst hour {formatClock(worst.hour)} UTC with{" "}
                <span className="num">{formatCount(worst.errors)}</span>
              </>
            )}
          </div>
          {data.result_sources === undefined ? null : (
            <dl className="kv">
              <dt>Upstream 4xx and 5xx</dt>
              <dd>{formatCount(kinds.upstream)}</dd>
              <dt>Limited by the gateway</dt>
              <dd>{formatCount(kinds.limited)}</dd>
              <dt>Refused or broken off</dt>
              <dd>{formatCount(kinds.other)}</dd>
            </dl>
          )}
          {highest === undefined ? null : (
            <div className="figs-sub">
              Highest: {machineLabel(highest.machine)} at{" "}
              <span className="num">{formatPercent(highest.rate, 1)}</span>
            </div>
          )}
        </div>
      </div>
      {data.latency === undefined ? null : (
        <Durations data={data} latency={data.latency} />
      )}
    </div>
  );
}

/** Request durations per hour, and each client machine's over the window. */
function Durations({
  data,
  latency,
}: {
  data: Dashboard;
  latency: NonNullable<Dashboard["latency"]>;
}) {
  const hours = durationSeries(latency.hourly, data.at);
  const machines = [...latency.machines].sort(
    (a, b) => b.duration_p95_ms - a.duration_p95_ms,
  );
  return (
    <div className="red-row">
      <div className="red-chart">
        <div className="title">
          Duration
          <span className="key">
            <span>UTC</span>
            <span>
              <span className="sw sw-1" />
              p95
            </span>
            <span>
              <span className="sw sw-1 dash" />
              p50
            </span>
          </span>
        </div>
        <HourLines
          label="Request duration per hour, p95 and p50, over the last 24 h"
          hours={hours.map((h) => h.hour)}
          lines={[
            { name: "p95", values: hours.map((h) => h.p95) },
            { name: "p50", values: hours.map((h) => h.p50), dashed: true },
          ]}
          formatAxis={formatMs}
          tip={(i) => {
            const h = hours[i];
            if (h === undefined) return null;
            return (
              <>
                <b>{hourTitle(h)}</b>
                <br />
                {h.p95 === null || h.p50 === null
                  ? "No answered request"
                  : `p95 ${formatMs(h.p95)} · p50 ${formatMs(h.p50)}`}
              </>
            );
          }}
        />
      </div>
      <div className="red-figs">
        <div className="figs-sub">
          Whole request until its response finished streaming; answered
          inference requests only. Percentiles of different groups do not add
          up, so there is no single figure for the day.
        </div>
        {machines.length === 0 ? null : (
          <dl className="kv" aria-label="Duration by client machine">
            {machines.map((m) => (
              <Fragment key={m.client_machine}>
                <dt>{machineLabel(m.client_machine)}</dt>
                <dd>
                  p95 {formatMs(m.duration_p95_ms)} · p50{" "}
                  {formatMs(m.duration_p50_ms)}
                </dd>
              </Fragment>
            ))}
          </dl>
        )}
      </div>
    </div>
  );
}

export function Overview({ data, now }: { data: Dashboard; now: Date }) {
  const g = data.gateway;
  const name = providerName(g.provider);
  const health = healthState(data);
  const near = data.accounts.filter(
    (a) => accountState(a, data.soft, data.hard, now).word === "Near limit",
  ).length;
  const matching = data.accounts.filter(
    (a) => a.egress_check?.outcome === "ok",
  ).length;
  const mismatch = data.accounts.filter(
    (a) => a.egress_check?.outcome === "mismatch",
  ).length;
  const unconfirmed = data.accounts.length - matching - mismatch;
  const machines = data.machines.filter((m) => m.revoked_at === undefined);
  return (
    <>
      <PageHead
        title="Overview"
        sub={
          <>
            {g.provider === undefined ? "Gateway" : `${name} gateway`}
            {g.version === undefined ? "" : ` · v${g.version}`} · up{" "}
            {formatSpan(now.getTime() - Date.parse(g.started))}
          </>
        }
      />
      <div className="strip">
        <div>
          <div className="k">Health</div>
          <div className="v">
            <Status state={health} pill />
          </div>
          {(data.health.reasons ?? []).map((r) => (
            <div className="reason" key={r}>
              {r}
            </div>
          ))}
        </div>
        <div>
          <div className="k">Accounts</div>
          <div className="v">
            <span className="num">{available(data, now).length}</span> of{" "}
            <span className="num">{data.accounts.length}</span> available
          </div>
          <div className="reason">
            {near > 0 ? (
              <Status
                state={{ tone: "warn", word: `${String(near)} near limit` }}
              />
            ) : (
              <Status state={{ tone: "ok", word: "none near limit" }} />
            )}
          </div>
        </div>
        <div>
          <div className="k">Exits</div>
          <div className="v">
            <span className="num">{matching}</span> of{" "}
            <span className="num">{data.accounts.length}</span> matching
          </div>
          <div className="reason">
            {mismatch > 0 ? (
              <Status
                state={{ tone: "bad", word: `${String(mismatch)} mismatch` }}
              />
            ) : unconfirmed > 0 ? (
              <Status
                state={{
                  tone: "warn",
                  word: `${String(unconfirmed)} not confirmed`,
                }}
              />
            ) : (
              <Status state={{ tone: "ok", word: "all match" }} />
            )}
          </div>
        </div>
        <div>
          <div className="k">Tunnels</div>
          <div className="v num">
            {g.open_tunnels} / {g.tunnel_limit}
          </div>
          <div className="reason">open / limit</div>
        </div>
        <div>
          <div className="k">Clients</div>
          <div className="v">
            <span className="num">{machines.length}</span> machine
            {machines.length === 1 ? "" : "s"}
          </div>
          <div className="reason">
            <span className="num">{data.sessions.length}</span> live sessions ·{" "}
            <a href={routeHref({ view: "clients" })} className="text-link">
              View
            </a>
          </div>
        </div>
      </div>
      <Section
        title="Requests and errors"
        note={
          g.provider === undefined
            ? "Last 24 h, all clients"
            : `Last 24 h, all clients of ${name}`
        }
      >
        <Traffic data={data} />
      </Section>
      <Section
        title="Account quota"
        note={`Soft limit ${formatPercent(data.soft)} (the tick on each bar), hard limit ${formatPercent(data.hard)}; the hollow mark is an even pace through the window`}
      >
        <AccountsTable data={data} now={now} />
      </Section>
    </>
  );
}
