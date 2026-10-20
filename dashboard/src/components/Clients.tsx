import { Cpu } from "lucide-react";
import { formatCount, formatMs, formatPercent } from "../lib/format";
import { routeHref } from "../lib/route";
import { byMachine, errorRate, errorRateState } from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName } from "../lib/words";
import { Ago, Empty, PageHead, Section, Status, Time } from "./ui";

/** The client machines registered with this gateway, with their last 24 h of requests. */
export function Clients({ data, now }: { data: Dashboard; now: Date }) {
  const latency = new Map(
    (data.latency?.machines ?? []).map((l) => [l.client_machine, l]),
  );
  const usage = new Map(byMachine(data.usage ?? []).map((m) => [m.machine, m]));
  const orphan = usage.get("");
  return (
    <>
      <PageHead
        title="Clients"
        sub={`${providerName(data.gateway.provider)} · the client machines registered with this gateway · last 24 h`}
      />
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
                  <th scope="col" className="n">
                    Live / max sessions
                  </th>
                  <th scope="col" className="n">
                    Requests, 24 h
                  </th>
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
