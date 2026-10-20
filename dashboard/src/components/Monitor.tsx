import { CircleX, TriangleAlert } from "lucide-react";
import { useEffect } from "react";
import {
  formatBytes,
  formatClock,
  formatCount,
  formatPercent,
  formatSpan,
} from "../lib/format";
import {
  accountState,
  errorRate,
  errorRateState,
  failures,
  healthState,
  hourSeries,
} from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName, resultWording } from "../lib/words";
import { Egress } from "./Accounts";
import { Spark } from "./charts";
import { Ago, Empty, Meter, PageHead, Section, Share, Status } from "./ui";

/** The current hour against the last 24 h, and what the gateway holds open now. */
function Now({ data, now }: { data: Dashboard; now: Date }) {
  const g = data.gateway;
  const health = healthState(data);
  const reasons = data.health.reasons?.length ?? 0;
  const inFlight = data.accounts.reduce((n, a) => n + a.in_flight, 0);
  const limit = data.accounts.reduce((n, a) => n + a.concurrency, 0);
  const tunnels = g.tunnel_limit === 0 ? 0 : g.open_tunnels / g.tunnel_limit;
  const hours = data.usage === undefined ? [] : hourSeries(data.usage, data.at);
  const last = hours[hours.length - 1];
  const labels = hours.map((h) => h.hour);
  const lastRate = last === undefined ? 0 : errorRate(last);
  return (
    <div className="stats five">
      <div>
        <div className="k">Health</div>
        <div className="v">
          <Status state={health} pill />
        </div>
        <div className="figs-sub">
          {reasons} reason{reasons === 1 ? "" : "s"} · up{" "}
          {formatSpan(now.getTime() - Date.parse(g.started))}
        </div>
      </div>
      {last === undefined ? null : (
        <>
          <div>
            <div className="k">Requests this hour</div>
            <div className="big">{formatCount(last.requests)}</div>
            <Spark
              label="Requests per hour"
              hours={labels}
              values={hours.map((h) => h.requests)}
              format={formatCount}
            />
          </div>
          <div>
            <div className="k">Error rate this hour</div>
            <div className="big">{formatPercent(lastRate, 1)}</div>
            <Status state={errorRateState(lastRate)} />
            <Spark
              label="Error rate per hour"
              hours={labels}
              values={hours.map((h) => errorRate(h))}
              format={(v) => formatPercent(v, 1)}
            />
          </div>
          <div>
            <div className="k">Limited this hour</div>
            <div className="big">{formatCount(last.limited)}</div>
            <div className="figs-sub">
              {formatCount(hours.reduce((n, h) => n + h.limited, 0))} in 24 h
            </div>
            <Spark
              label="Limited per hour"
              hours={labels}
              values={hours.map((h) => h.limited)}
              format={formatCount}
            />
          </div>
        </>
      )}
      <div>
        <div className="k">Capacity now</div>
        <div className="v">
          <span className="num">
            {inFlight} / {limit}
          </span>{" "}
          in flight
        </div>
        <div className={`meter s-${tunnels >= 0.8 ? "warn" : "ok"}`}>
          <div className="track">
            <div
              className="fill"
              style={{ width: `${(Math.min(tunnels, 1) * 100).toFixed(1)}%` }}
            />
          </div>
          <span className="pct">{formatPercent(tunnels)}</span>
        </div>
        <div className="figs-sub">
          <span className="num">
            {g.open_tunnels} / {g.tunnel_limit}
          </span>{" "}
          tunnels open
        </div>
      </div>
    </div>
  );
}

function AccountCards({ data, now }: { data: Dashboard; now: Date }) {
  if (data.accounts.length === 0) return <Empty title="No accounts." />;
  return (
    <div className="cards">
      {data.accounts.map((a) => (
        <div className="card" key={a.alias}>
          <div className="card-head">
            <span className="client-name">{a.alias}</span>
            <span className="small muted">{a.plan?.name ?? "no plan"}</span>
            <Status state={accountState(a, data.soft, data.hard)} pill />
          </div>
          {(["5h", "7d"] as const).map((w) => (
            <div className="win" key={w}>
              <div className="win-head">
                <span>{w === "5h" ? "5 h" : "7 d"} window</span>
              </div>
              <Meter
                quota={a.quota[w]}
                window={w}
                soft={data.soft}
                hard={data.hard}
                paused={a.paused_until !== undefined}
                now={now}
              />
            </div>
          ))}
          <div className="line">
            <Egress account={a} now={now} />
          </div>
          <div className="line muted">
            In flight{" "}
            <span className="num">
              {a.in_flight} / {a.concurrency}
            </span>{" "}
            · sessions <span className="num">{a.bound_sessions}</span>
          </div>
        </div>
      ))}
    </div>
  );
}

function Failures({ data }: { data: Dashboard }) {
  if (data.result_sources === undefined)
    return (
      <Empty title="No result counts.">
        This gateway is older than the page.
      </Empty>
    );
  const { answered, failed } = failures(data.result_sources);
  const all = failed.reduce((n, f) => n + f.count, 0);
  return (
    <div className="panel">
      <div className="figs-sub">
        <span className="num">{formatCount(answered)}</span> answered ·{" "}
        <span className="num">{formatCount(all)}</span> failed; the failures add
        up to the Errors row on Overview
      </div>
      {failed.length === 0 ? null : (
        <div className="rank">
          {failed.map((f) => {
            const w = resultWording(f.kind, f.result, f.status);
            const Icon = w.tone === "bad" ? CircleX : TriangleAlert;
            return (
              <div
                className="rank-row static"
                key={`${f.kind}\t${f.result}\t${String(f.status)}`}
              >
                <Icon
                  className={`icon sm solid s-${w.tone}`}
                  aria-hidden="true"
                />
                <span className="who">
                  {w.text}
                  <span className="code mono">
                    {f.kind} · {f.result}
                    {f.status === 0 ? "" : ` · ${String(f.status)}`}
                  </span>
                </span>
                <Share part={f.count} whole={all} />
                <span className="num">{formatCount(f.count)}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function Daily({ data }: { data: Dashboard }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">UTC day</th>
            <th scope="col" className="n">
              Limited
              <br />
              <span className="faint">inference</span>
            </th>
            <th scope="col" className="n">
              Limited
              <br />
              <span className="faint">CONNECT</span>
            </th>
            <th scope="col" className="n">
              Tunnel
              <br />
              <span className="faint">peak</span>
            </th>
            <th scope="col" className="n">
              Session
              <br />
              <span className="faint">peak</span>
            </th>
            <th scope="col" className="n">
              Leaked
              <br />
              <span className="faint">sessions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {data.days.map((d) => (
            <tr key={d.day}>
              <td className="mono small">{d.day.slice(0, 10)}</td>
              <td className="n">{d.limited_inference}</td>
              <td className="n">{d.limited_connect}</td>
              <td className="n">{d.tunnel_peak}</td>
              <td className="n">{d.session_peak}</td>
              <td className="n">
                {d.leaked === 0 ? (
                  "0"
                ) : (
                  <Status state={{ tone: "warn", word: String(d.leaked) }} />
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Store({ data, now }: { data: Dashboard; now: Date }) {
  const g = data.gateway;
  const r = g.retention;
  return (
    <div className="panel">
      <dl className="kv">
        <dt>Gateway version</dt>
        <dd className="mono">
          {g.version === undefined ? "unknown" : `v${g.version}`}
        </dd>
        <dt>Page commit</dt>
        <dd className="mono">
          {__DASHBOARD_COMMIT__ === ""
            ? "unknown"
            : __DASHBOARD_COMMIT__.slice(0, 12)}
        </dd>
        <dt>Inventory commit</dt>
        <dd className="mono">
          {g.inventory_commit === ""
            ? "unknown"
            : g.inventory_commit.slice(0, 12)}
        </dd>
        <dt>Started</dt>
        <dd>
          <Ago iso={g.started} now={now} />
        </dd>
        <dt>Database</dt>
        <dd>{formatBytes(data.db_bytes)}</dd>
        <dt>Write-ahead log</dt>
        <dd>{formatBytes(g.wal_bytes)}</dd>
        <dt>Last backup</dt>
        <dd>
          {g.backup === null ? (
            "not configured"
          ) : g.backup.last_success === undefined ? (
            "none since start"
          ) : (
            <Ago iso={g.backup.last_success} now={now} />
          )}
        </dd>
        {g.backup?.last_failure === undefined ? null : (
          <>
            <dt>Last failed backup</dt>
            <dd>
              <Status state={{ tone: "bad", word: "failed" }} />{" "}
              <Ago iso={g.backup.last_failure} now={now} />
            </dd>
          </>
        )}
        <dt>Retention ran</dt>
        <dd>
          {r.at === undefined ? (
            "not yet since start"
          ) : (
            <Ago iso={r.at} now={now} />
          )}
        </dd>
        {r.at === undefined ? null : (
          <>
            <dt>Pruned traffic rows</dt>
            <dd>{formatCount(r.pruned.traffic)}</dd>
            <dt>Pruned bindings · sessions</dt>
            <dd>
              {formatCount(r.pruned.binding_history)} ·{" "}
              {formatCount(r.pruned.sessions)}
            </dd>
            <dt>Vacuumed</dt>
            <dd>{r.vacuumed ? "yes" : "no"}</dd>
          </>
        )}
        {r.error === undefined ? null : (
          <>
            <dt>Retention error</dt>
            <dd>
              <Status state={{ tone: "bad", word: r.error }} />
            </dd>
          </>
        )}
      </dl>
    </div>
  );
}

/** The gateway's own state, in the order a request passes through it. */
export function Monitor({
  data,
  now,
  focus,
}: {
  data: Dashboard;
  now: Date;
  focus: string | undefined;
}) {
  useEffect(() => {
    if (focus !== undefined) document.getElementById(focus)?.scrollIntoView();
  }, [focus]);
  const name = providerName(data.gateway.provider);
  return (
    <>
      <PageHead
        title="Monitor"
        sub={`${name} · the gateway's own state · up ${formatSpan(now.getTime() - Date.parse(data.gateway.started))}`}
      />
      <Section
        title="Now"
        note={
          data.usage === undefined || data.usage.length === 0
            ? "What the gateway holds open now"
            : `Hour starting ${formatClock(new Date(Math.floor(Date.parse(data.at) / 3_600_000) * 3_600_000).toISOString())} UTC; lines are the last 24 h`
        }
      >
        <Now data={data} now={now} />
      </Section>
      <Section
        title="Why requests failed"
        note="Last 24 h, by result and status"
        id="failures"
      >
        <Failures data={data} />
      </Section>
      <Section
        title="Accounts"
        note="Each window's use this cycle; the hollow mark is an even pace through it"
      >
        <AccountCards data={data} now={now} />
      </Section>
      <section className="section">
        <div className="two-col wide-left">
          <div>
            <div className="section-head">
              <h2>Daily</h2>
              <span className="note">Last 7 UTC days</span>
            </div>
            <Daily data={data} />
          </div>
          <div>
            <div className="section-head">
              <h2>Store</h2>
              <span className="note">This process</span>
            </div>
            <Store data={data} now={now} />
          </div>
        </div>
      </section>
    </>
  );
}
