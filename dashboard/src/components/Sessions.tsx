import { ArrowRightLeft } from "lucide-react";
import { formatAgo, formatSpan } from "../lib/format";
import { routeHref } from "../lib/route";
import {
  HOUR_MS,
  idleSessions,
  recentBindings,
  sessionState,
} from "../lib/stats";
import type { Dashboard } from "../lib/types";
import { bindingReason, providerName } from "../lib/words";
import { Ago, Empty, PageHead, Section, Status, Time } from "./ui";

/** The live sessions of this gateway, on one client machine or all, and how they were bound. */
export function Sessions({
  data,
  now,
  machine,
}: {
  data: Dashboard;
  now: Date;
  machine: string | undefined;
}) {
  const names = [...new Set(data.sessions.map((s) => s.client_machine))].sort();
  const shown = data.sessions
    .filter((s) => machine === undefined || s.client_machine === machine)
    .sort((a, b) => Date.parse(b.last_seen) - Date.parse(a.last_seen));
  const ids = new Set(shown.map((s) => s.id));
  const history = data.history
    .filter((h) => machine === undefined || ids.has(h.session))
    .sort((a, b) => Date.parse(b.from) - Date.parse(a.from));
  const active = shown.filter(
    (s) => now.getTime() - Date.parse(s.last_seen) < HOUR_MS,
  ).length;
  const bindings = recentBindings(history, now);
  const inUse = new Set(shown.map((s) => s.alias).filter((a) => a !== "")).size;
  const paused = data.accounts.filter((a) => a.paused_until !== undefined);
  return (
    <>
      <PageHead
        title="Sessions"
        sub={`${providerName(data.gateway.provider)} · live sessions and how they were bound to accounts`}
      />
      {names.length > 1 || machine !== undefined ? (
        <div className="toolbar">
          <div className="seg" role="group" aria-label="Client machine">
            {[undefined, ...names].map((m) => (
              <a
                key={m ?? ""}
                href={routeHref({ view: "sessions", arg: m })}
                aria-current={m === machine ? "page" : undefined}
              >
                {m ?? "All machines"}
                <span className="count">
                  {
                    data.sessions.filter(
                      (s) => m === undefined || s.client_machine === m,
                    ).length
                  }
                </span>
              </a>
            ))}
          </div>
        </div>
      ) : null}
      <div className="stats">
        <div>
          <div className="k">Live sessions</div>
          <div className="big">{shown.length}</div>
          <div className="figs-sub">
            {active} with a request in the last hour
          </div>
        </div>
        <div>
          <div className="k">Idle over 24 h</div>
          <div className="big">
            {machine === undefined
              ? data.idle_sessions
              : idleSessions(shown, now)}
          </div>
          <div className="figs-sub">live, but no request for 24 h</div>
        </div>
        <div>
          <div className="k">Bindings, last 24 h</div>
          <div className="big">{bindings.all}</div>
          <div className="figs-sub">
            {bindings.all - bindings.rebound} new · {bindings.rebound} rebound
          </div>
        </div>
        <div>
          <div className="k">Accounts in use</div>
          <div className="big">
            {inUse} <span className="unit">of {data.accounts.length}</span>
          </div>
          <div className="figs-sub">
            {paused.length === 0
              ? "none paused"
              : paused.map((a) => `${a.alias} paused`).join(" · ")}
          </div>
        </div>
      </div>
      <Section
        title="Live sessions"
        note={`${String(shown.length)} ${machine === undefined ? "on all machines" : `on ${machine}`}; sorted by last request; IDs are the first 8 characters`}
      >
        {shown.length === 0 ? (
          <Empty
            title={
              machine === undefined
                ? "No live sessions."
                : `No live sessions on ${machine}.`
            }
          >
            A session ends when its client exits, or after the gateway's idle
            expiry (7 days by default) without a request.
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Session</th>
                  <th scope="col">Client machine</th>
                  <th scope="col">Account</th>
                  <th scope="col" className="n">
                    Age
                  </th>
                  <th scope="col" className="n">
                    Last request
                  </th>
                  <th scope="col">State</th>
                </tr>
              </thead>
              <tbody>
                {shown.map((s) => (
                  <tr key={s.id}>
                    <td className="mono">{s.id}</td>
                    <td>{s.client_machine}</td>
                    <td>
                      {s.alias === "" ? (
                        <span className="faint">unbound</span>
                      ) : (
                        s.alias
                      )}
                    </td>
                    <td className="n">
                      <Time iso={s.created_at}>
                        {formatSpan(now.getTime() - Date.parse(s.created_at))}
                      </Time>
                    </td>
                    <td className="n muted">
                      <Ago iso={s.last_seen} now={now} />
                    </td>
                    <td>
                      <Status state={sessionState(s, now)} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
      <Section
        title="Binding history"
        note={
          machine === undefined
            ? "The gateway's last 50, newest first; each row is one binding of a session to an account"
            : `The bindings of ${machine}'s live sessions among the gateway's last 50; those of its ended sessions are not shown`
        }
      >
        {history.length === 0 ? (
          <Empty title="No bindings." />
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col" className="n">
                    Bound
                  </th>
                  <th scope="col">Session</th>
                  <th scope="col">Account</th>
                  <th scope="col">Until</th>
                  <th scope="col">Why</th>
                </tr>
              </thead>
              <tbody>
                {history.map((h) => (
                  <tr key={`${h.session}\t${h.from}\t${h.alias}`}>
                    <td className="n muted">
                      <Ago iso={h.from} now={now} />
                    </td>
                    <td className="mono">{h.session}</td>
                    <td>
                      <ArrowRightLeft
                        className="icon sm faint"
                        aria-hidden="true"
                      />{" "}
                      {h.alias}
                    </td>
                    <td className="muted">
                      {h.to === undefined ? (
                        "current"
                      ) : (
                        <Time iso={h.to}>until {formatAgo(h.to, now)}</Time>
                      )}
                    </td>
                    <td>
                      {h.reason === "new" ? (
                        bindingReason(h.reason)
                      ) : (
                        <Status
                          state={{
                            tone: h.reason === "quota_hard" ? "warn" : "info",
                            word: bindingReason(h.reason),
                          }}
                        />
                      )}{" "}
                      <span className="mono small faint">{h.reason}</span>
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
