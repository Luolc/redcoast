import { Cpu, Terminal } from "lucide-react";
import {
  formatCount,
  formatMs,
  formatPercent,
  formatTokens,
} from "../lib/format";
import { clientHref, routeHref } from "../lib/route";
import {
  byMachine,
  clientRows,
  errorRate,
  errorRateState,
  tokenTotal,
  type ClientRow,
} from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName } from "../lib/words";
import { RowSpark } from "./charts";
import { Ago, Empty, PageHead, Section, Share, Status, Time } from "./ui";

/** A client machine and its program, `machine-a · claude-code`. */
export function ClientName({
  machine,
  client,
  link = false,
}: {
  machine: string;
  client: string;
  link?: boolean;
}) {
  const Icon = client === "codex-cli" ? Cpu : Terminal;
  const name = (
    <>
      {machine} <span className="sep">·</span>{" "}
      {client === "" ? (
        <span className="faint">unknown program</span>
      ) : (
        <span className="mono">{client}</span>
      )}
    </>
  );
  return (
    <div className="client-name">
      <Icon className="icon sm" aria-hidden="true" />
      {link ? <a href={clientHref(machine, client)}>{name}</a> : name}
    </div>
  );
}

const sum = (values: number[]) => values.reduce((a, b) => a + b, 0);

/** One row per client machine and program; null when the gateway does not count them. */
function ClientTable({ data }: { data: Dashboard }) {
  if (data.clients === undefined || data.client_hourly === undefined)
    return null;
  const rows = clientRows(data);
  const all = sum(rows.map((r) => sum(r.requests)));
  const row = (r: ClientRow) => {
    const requests = sum(r.requests);
    return (
      <tr key={`${r.machine}\t${r.client}`}>
        <td>
          <ClientName machine={r.machine} client={r.client} link />
        </td>
        <td className="muted mono small">
          {r.versions.length === 0 ? (
            <span className="faint">—</span>
          ) : (
            r.versions.join(", ")
          )}
        </td>
        <td className="n">{r.live}</td>
        <td className="n">{r.sessions}</td>
        <td className="n">{formatCount(requests)}</td>
        <td className="share-cell">
          <Share part={requests} whole={all} />
          <span className="num small muted">
            {formatPercent(all === 0 ? 0 : requests / all, 1)}
          </span>
        </td>
        <td>
          <RowSpark
            label="Requests per hour, last 24 h"
            values={r.requests}
            format={formatCount}
          />
        </td>
      </tr>
    );
  };
  return (
    <Section
      title="Clients"
      note="One row per client machine and program; sessions created in the last 7 UTC days, today included; requests of the last 24 h"
    >
      {rows.length === 0 ? (
        <Empty title="No sessions in the last 7 UTC days." />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">
                  Client <span className="faint">machine · program</span>
                </th>
                <th scope="col">Version</th>
                <th scope="col" className="n">
                  Live sessions
                </th>
                <th scope="col" className="n">
                  Sessions, 7 UTC days
                </th>
                <th scope="col" className="n">
                  Requests, 24 h
                </th>
                <th scope="col">Share</th>
                <th scope="col">24 h</th>
              </tr>
            </thead>
            <tbody>{rows.map(row)}</tbody>
            <tfoot>
              <tr>
                <td>All clients</td>
                <td />
                <td className="n">{sum(rows.map((r) => r.live))}</td>
                <td className="n">{sum(rows.map((r) => r.sessions))}</td>
                <td className="n">{formatCount(all)}</td>
                <td />
                <td />
              </tr>
            </tfoot>
          </table>
        </div>
      )}
      <p className="small muted foot">
        The version is that of the program's live sessions. Requests of no
        session count on Overview but on no client, so the rows add up to less
        than its total.
      </p>
    </Section>
  );
}

/** The client machines registered with this gateway, with their last 24 h of requests. */
export function Clients({ data, now }: { data: Dashboard; now: Date }) {
  const programs = new Map<string, string[]>();
  for (const c of data.clients ?? [])
    programs.set(c.client_machine, [
      ...(programs.get(c.client_machine) ?? []),
      c.client,
    ]);
  const latency = new Map(
    (data.latency?.machines ?? []).map((l) => [l.client_machine, l]),
  );
  const usage = new Map(byMachine(data.usage ?? []).map((m) => [m.machine, m]));
  const orphan = usage.get("");
  const tokens = tokenTotal(data.usage ?? []);
  const machineTokens = (name: string) => {
    const t = tokenTotal(
      (data.usage ?? []).filter((u) => u.client_machine === name),
    );
    return t === null || t.input + t.output === 0 ? (
      <span className="faint">—</span>
    ) : (
      `${formatTokens(t.input)} · ${formatTokens(t.output)}`
    );
  };
  return (
    <>
      <PageHead
        title="Clients"
        sub={`${providerName(data.gateway.provider)} · the client machines registered with this gateway and the programs that run on them`}
      />
      <ClientTable data={data} />
      <Section
        title="Machines"
        note={
          data.gateway.provider === undefined
            ? "Registered with this gateway"
            : `Registered with the ${providerName(data.gateway.provider)} gateway`
        }
      >
        {data.machines.length === 0 ? (
          <Empty title="No client machines.">
            A machine is registered with{" "}
            <span className="mono">machine add</span>.
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Machine</th>
                  {data.clients === undefined ? null : (
                    <th scope="col">Programs, 7 UTC days</th>
                  )}
                  <th scope="col" className="n">
                    Live / max sessions
                  </th>
                  <th scope="col" className="n">
                    Requests, 24 h
                  </th>
                  {tokens === null ? null : (
                    <th scope="col" className="n">
                      Tokens, 24 h
                      <br />
                      <span className="faint">in · out</span>
                    </th>
                  )}
                  <th scope="col" className="n">
                    Error rate
                  </th>
                  <th scope="col" className="n">
                    p95 · p50, 24 h
                  </th>
                  <th scope="col" className="n">
                    Last seen
                  </th>
                  <th scope="col" className="n">
                    Registered
                  </th>
                  <th scope="col">State</th>
                </tr>
              </thead>
              <tbody>
                {data.machines.map((m) => {
                  const u = usage.get(m.name);
                  const rate = u === undefined ? 0 : errorRate(u);
                  const rs = errorRateState(rate);
                  const d = latency.get(m.name);
                  return (
                    <tr key={m.name}>
                      <td>
                        <div className="client-name">
                          <Cpu className="icon sm" aria-hidden="true" />
                          <a
                            href={routeHref({ view: "sessions", arg: m.name })}
                          >
                            {m.name}
                          </a>
                        </div>
                      </td>
                      {data.clients === undefined ? null : (
                        <td className="small">
                          {(programs.get(m.name) ?? []).length === 0 ? (
                            <span className="faint">—</span>
                          ) : (
                            (programs.get(m.name) ?? []).map((c, i) => (
                              <span key={c}>
                                {i === 0 ? null : ", "}
                                <a
                                  className={c === "" ? undefined : "mono"}
                                  href={clientHref(m.name, c)}
                                >
                                  {c === "" ? "unknown" : c}
                                </a>
                              </span>
                            ))
                          )}
                        </td>
                      )}
                      <td className="n">
                        {m.live_sessions} / {m.max_sessions}
                      </td>
                      <td className="n">
                        {data.usage === undefined ? (
                          <span className="faint">—</span>
                        ) : (
                          formatCount(u?.requests ?? 0)
                        )}
                      </td>
                      {tokens === null ? null : (
                        <td className="n">{machineTokens(m.name)}</td>
                      )}
                      <td className="n">
                        {u === undefined || u.requests === 0 ? (
                          <span className="faint">—</span>
                        ) : rs.tone === "ok" ? (
                          formatPercent(rate, 1)
                        ) : (
                          <Status
                            state={{
                              tone: rs.tone,
                              word: formatPercent(rate, 1),
                            }}
                          />
                        )}
                      </td>
                      <td className="n">
                        {d === undefined ? (
                          <span className="faint">—</span>
                        ) : (
                          `${formatMs(d.duration_p95_ms)} · ${formatMs(d.duration_p50_ms)}`
                        )}
                      </td>
                      <td className="n muted">
                        {m.last_seen === undefined ? (
                          "never"
                        ) : (
                          <Ago iso={m.last_seen} now={now} />
                        )}
                      </td>
                      <td className="n muted">
                        <Time iso={m.created_at}>
                          {m.created_at.slice(0, 10)}
                        </Time>
                      </td>
                      <td>
                        {m.revoked_at === undefined ? (
                          <Status state={{ tone: "ok", word: "Active" }} />
                        ) : (
                          <Time iso={m.revoked_at}>
                            <Status state={{ tone: "off", word: "Revoked" }} />
                          </Time>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        {data.usage === undefined ? (
          <p className="small muted foot">
            This gateway is older than the page and does not report requests by
            machine.
          </p>
        ) : null}
        {orphan === undefined ? null : (
          <p className="small muted foot">
            {formatCount(orphan.requests)} requests in the last 24 h came with
            no session (refused before one was found); they count on Overview
            but on no machine.
          </p>
        )}
      </Section>
    </>
  );
}
