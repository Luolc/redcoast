import { formatBytes, formatCount } from "../lib/format";
import type { Dashboard } from "../lib/types";
import { providerName } from "../lib/words";
import { Egress } from "./Accounts";
import { Ago, Empty, PageHead, Section } from "./ui";

function route(r: string): string {
  switch (r) {
    case "direct":
      return "Direct";
    case "account":
      return "Account exit";
    default:
      return "—";
  }
}

export function Exits({ data, now }: { data: Dashboard; now: Date }) {
  return (
    <>
      <PageHead
        title="Exits"
        sub={`${providerName(data.gateway.provider)} · one exit per account; the expected egress IP is checked against what the exit shows`}
      />
      <Section
        title="Egress checks"
        note="Since the process started; a check that reads no IP pauses nothing"
      >
        {data.accounts.length === 0 ? (
          <Empty title="No accounts." />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Account</th>
                  <th scope="col">Egress IP and check</th>
                </tr>
              </thead>
              <tbody>
                {data.accounts.map((a) => (
                  <tr key={a.alias}>
                    <td>
                      <div className="client-name">{a.alias}</div>
                    </td>
                    <td className="two">
                      <Egress account={a} now={now} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
      <Section
        title="CONNECT destinations"
        note="Last 7 days, refused tunnels included, written when a tunnel ends; Direct does not use an account's exit"
      >
        {data.destinations.length === 0 ? (
          <Empty title="No CONNECT in the last 7 days." />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Host</th>
                  <th scope="col">Account</th>
                  <th scope="col">Route</th>
                  <th scope="col" className="n">
                    Tunnels
                  </th>
                  <th scope="col" className="n">
                    Ok
                  </th>
                  <th scope="col" className="n">
                    Up / down
                  </th>
                  <th scope="col" className="n">
                    Last
                  </th>
                </tr>
              </thead>
              <tbody>
                {data.destinations.map((d) => (
                  <tr
                    key={`${d.host}:${String(d.port)}\t${d.alias}\t${d.route}`}
                  >
                    <td className="mono">
                      {d.host}:{d.port}
                    </td>
                    <td>
                      {d.alias === "" ? (
                        <span className="faint">none</span>
                      ) : (
                        d.alias
                      )}
                    </td>
                    <td>{route(d.route)}</td>
                    <td className="n">{formatCount(d.tunnels)}</td>
                    <td className="n">{formatCount(d.ok)}</td>
                    <td className="n muted">
                      {formatBytes(d.bytes_up)} / {formatBytes(d.bytes_down)}
                    </td>
                    <td className="n muted">
                      <Ago iso={d.last} now={now} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
    </>
  );
}
