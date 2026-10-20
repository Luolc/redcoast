import { ArrowLeft } from "lucide-react";
import { formatCount, formatHourSpan, formatPercent } from "../lib/format";
import { routeHref } from "../lib/route";
import { clientRows, windowHourStarts } from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { providerName } from "../lib/words";
import { HourBars } from "./charts";
import { SessionTable } from "./Sessions";
import { Empty, PageHead, Section } from "./ui";

function Back() {
  return (
    <a className="back" href={routeHref({ view: "clients" })}>
      <ArrowLeft className="icon sm" aria-hidden="true" />
      All clients
    </a>
  );
}

/** One client machine and program: its requests over 24 h and its live sessions. */
export function Client({
  data,
  now,
  machine,
  client,
}: {
  data: Dashboard;
  now: Date;
  machine: string;
  client: string;
}) {
  const provider = providerName(data.gateway.provider);
  const title = `${machine} · ${client === "" ? "unknown program" : client}`;
  const rows = clientRows(data);
  const row = rows.find((r) => r.machine === machine && r.client === client);
  if (row === undefined)
    return (
      <>
        <Back />
        <PageHead title={title} sub={provider} />
        <Empty title={`No such client on ${provider}.`}>
          It had no session in the last 7 days, or this gateway is older than
          the page and does not report programs.
        </Empty>
      </>
    );
  const sessions = data.sessions
    .filter((s) => s.client_machine === machine && (s.client ?? "") === client)
    .sort((a, b) => Date.parse(b.last_seen) - Date.parse(a.last_seen));
  const hours = windowHourStarts(data.at);
  const requests = row.requests.reduce((a, b) => a + b, 0);
  const all = rows.reduce(
    (sum, r) => sum + r.requests.reduce((a, b) => a + b, 0),
    0,
  );
  return (
    <>
      <Back />
      <PageHead
        title={title}
        sub={
          <>
            {provider}
            {row.versions.length === 0 ? null : (
              <>
                {" · version "}
                <span className="mono">{row.versions.join(", ")}</span>
              </>
            )}
          </>
        }
      />
      <Section title="Requests" note="Last 24 h, this client only">
        <div className="red">
          <div className="red-row">
            <div className="red-chart">
              <div className="title">
                Requests<span className="key">per hour, UTC</span>
              </div>
              <HourBars
                label={`Requests per hour of ${title} over the last 24 h: ${formatCount(requests)} in all`}
                hours={hours}
                values={row.requests}
                tip={(i) => {
                  const h = hours[i];
                  return h === undefined ? null : (
                    <>
                      <b>{formatHourSpan(h)}</b>
                      <br />
                      Requests: {formatCount(row.requests[i] ?? 0)}
                    </>
                  );
                }}
              />
            </div>
            <div className="red-figs">
              <div className="big-row">
                <div className="big">{formatCount(requests)}</div>
                <div className="figs-sub">
                  requests
                  <br />
                  {formatPercent(all === 0 ? 0 : requests / all, 1)} of all
                  clients
                </div>
              </div>
              <p className="small muted">
                {row.live} live {row.live === 1 ? "session" : "sessions"};{" "}
                {row.sessions} created in the last 7 days.
              </p>
            </div>
          </div>
        </div>
      </Section>
      <Section
        title="Live sessions"
        note={`${String(sessions.length)} on this client`}
      >
        {sessions.length === 0 ? (
          <Empty title="No live sessions." />
        ) : (
          <SessionTable sessions={sessions} now={now} onClient />
        )}
      </Section>
      <p className="small muted foot">
        Errors and durations are counted by machine, not by program: see{" "}
        <a href={routeHref({ view: "clients" })}>{machine} on Clients</a>.
      </p>
    </>
  );
}
