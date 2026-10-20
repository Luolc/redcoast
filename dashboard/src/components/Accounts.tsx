import { formatCount, formatPercent, formatTokens } from "../lib/format";
import {
  accountState,
  byModel,
  pausedForGood,
  type TokenCounts,
} from "../lib/stats";
import type { Account, Dashboard } from "../lib/types";
import { egressWording, providerName } from "../lib/words";
import { Ago, Empty, Meter, PageHead, Section, Status, Time } from "./ui";

export function Egress({ account: a, now }: { account: Account; now: Date }) {
  const w = egressWording(a.egress_check?.outcome);
  return (
    <>
      <div className="mono">{a.expected_egress_ip}</div>
      <div className="small">
        <Status state={{ tone: w.tone, word: w.text }} />{" "}
        {a.egress_check === undefined ? null : (
          <span className="muted">
            <Ago iso={a.egress_check.at} now={now} />
          </span>
        )}
      </div>
    </>
  );
}

function Pause({ account: a }: { account: Account }) {
  if (a.paused_until === undefined) return null;
  return (
    <div className="small muted">
      {pausedForGood(a.paused_until) ? (
        "until resumed"
      ) : (
        <Time iso={a.paused_until}>
          until {a.paused_until.slice(5, 16).replace("T", " ")} UTC
        </Time>
      )}
      {a.pause_reason === undefined ? null : (
        <span className="mono"> · {a.pause_reason}</span>
      )}
    </div>
  );
}

function Plan({ account: a }: { account: Account }) {
  return (
    <div className="small muted">
      {a.email} · {a.plan?.name ?? "no plan"}
      {a.next_plan === null ? null : (
        <>
          {" "}
          ·{" "}
          <Time iso={a.next_plan.effective_from}>
            {a.next_plan.name} from {a.next_plan.effective_from.slice(0, 10)}
          </Time>
        </>
      )}
    </div>
  );
}

/** Each account's state, quota windows, exit, sessions and requests in flight. */
export function AccountsTable({ data, now }: { data: Dashboard; now: Date }) {
  if (data.accounts.length === 0)
    return (
      <Empty title="No accounts.">The gateway has no account configured.</Empty>
    );
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Account</th>
            <th scope="col">Status</th>
            <th scope="col">5 h window</th>
            <th scope="col">7 d window</th>
            <th scope="col">Egress IP</th>
            <th scope="col" className="n">
              Sessions
              <br />
              <span className="faint">bound / active</span>
            </th>
            <th scope="col" className="n">
              In flight
              <br />
              <span className="faint">now / limit</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {data.accounts.map((a) => (
            <tr key={a.alias}>
              <td className="two">
                <div className="client-name">{a.alias}</div>
                <Plan account={a} />
              </td>
              <td className="two">
                <Status
                  state={accountState(a, data.soft, data.hard, now)}
                  pill
                />
                <Pause account={a} />
              </td>
              {(["5h", "7d"] as const).map((w) => (
                <td className="two" key={w}>
                  <Meter
                    quota={a.quota[w]}
                    window={w}
                    soft={data.soft}
                    hard={data.hard}
                    paused={a.paused_until !== undefined}
                    now={now}
                  />
                </td>
              ))}
              <td className="two">
                <Egress account={a} now={now} />
              </td>
              <td className="n">
                {a.bound_sessions} / {a.active_sessions}
              </td>
              <td className="n">
                {a.in_flight} / {a.concurrency}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function TokenCells({
  tokens,
  cache,
}: {
  tokens: TokenCounts;
  cache: boolean;
}) {
  return (
    <>
      <td className="n">{formatTokens(tokens.input)}</td>
      <td className="n">{formatTokens(tokens.output)}</td>
      {cache ? (
        <>
          <td className="n">{formatTokens(tokens.cacheRead)}</td>
          <td className="n">{formatTokens(tokens.cacheWrite)}</td>
        </>
      ) : null}
    </>
  );
}

/** Requests and tokens by model over the last 24 h, with each model's accounts. */
function Models({ data }: { data: Dashboard }) {
  if (data.model_usage === undefined) return null;
  const rows = data.model_usage;
  const models = byModel(rows);
  // Codex counts no cache tokens apart; its columns would be zeros.
  const cache = models.some(
    (m) => m.tokens.cacheRead > 0 || m.tokens.cacheWrite > 0,
  );
  return (
    <Section
      title="Models"
      note={
        cache
          ? "Last 24 h, the requests whose response reported its tokens; input leaves out the cache reads and writes, counted apart"
          : "Last 24 h, the requests whose response reported its tokens; input includes cached tokens"
      }
    >
      {models.length === 0 ? (
        <Empty title="No tokens reported in the last 24 h." />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Model</th>
                <th scope="col">Accounts</th>
                <th scope="col" className="n">
                  Requests
                </th>
                <th scope="col" className="n">
                  Input
                </th>
                <th scope="col" className="n">
                  Output
                </th>
                {cache ? (
                  <>
                    <th scope="col" className="n">
                      Cache read
                    </th>
                    <th scope="col" className="n">
                      Cache write
                    </th>
                  </>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.model}>
                  <td className="mono">
                    {m.model === "" ? (
                      <span className="faint">not named</span>
                    ) : (
                      m.model
                    )}
                  </td>
                  <td className="small muted">
                    {byAccount(rows.filter((r) => r.model === m.model))}
                  </td>
                  <td className="n">{formatCount(m.requests)}</td>
                  <TokenCells tokens={m.tokens} cache={cache} />
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}

// Each account's requests among rows, most first: `sample-ab 1,200 · sample-ad 300`.
function byAccount(rows: NonNullable<Dashboard["model_usage"]>): string {
  const counts = new Map<string, number>();
  for (const r of rows)
    counts.set(r.alias, (counts.get(r.alias) ?? 0) + r.requests);
  return [...counts.entries()]
    .sort((a, b) => (a[1] !== b[1] ? b[1] - a[1] : a[0].localeCompare(b[0])))
    .map(
      ([alias, n]) =>
        `${alias === "" ? "no account" : alias} ${formatCount(n)}`,
    )
    .join(" · ");
}

export function Accounts({ data, now }: { data: Dashboard; now: Date }) {
  return (
    <>
      <PageHead
        title="Accounts"
        sub={`${providerName(data.gateway.provider)} · quota, status and egress of each account`}
      />
      <Section
        title="Accounts"
        note={`Soft limit ${formatPercent(data.soft)}, hard limit ${formatPercent(data.hard)}. A reading older than 30 min is marked with the time it was read. Egress checks and requests in flight are the process's memory since it started.`}
      >
        <AccountsTable data={data} now={now} />
      </Section>
      <Models data={data} />
    </>
  );
}
