import { CircleX, TriangleAlert } from "lucide-react";
import { useEffect } from "react";
import { formatBytes, formatCount, formatSpan } from "../lib/format";
import { failures } from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName, resultWording } from "../lib/words";
import { Ago, Empty, PageHead, Section, Share, Status } from "./ui";

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
        title="Why requests failed"
        note="Last 24 h, by result and status"
        id="failures"
      >
        <Failures data={data} />
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
