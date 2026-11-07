import { Plus } from "lucide-react";
import {
  canCreate,
  managedAccounts,
  statusOf,
  writeOffReason,
  type Managed,
} from "../lib/admin";
import { formatCount, formatTokens } from "../lib/format";
import { accountHref, goTo, routeHref } from "../lib/route";
import type { Dashboard } from "../lib/types";
import { accountStatusWording } from "../lib/words";
import { Estimate, WriteNotice } from "./AdminUi";
import { Empty, Meter, PageHead, Section, Status } from "./ui";

/** A 24 h figure with the 7 d one under it. */
function Pair({ a, b }: { a: string; b: string }) {
  return (
    <>
      <div>{a}</div>
      <div className="small muted">{b}</div>
    </>
  );
}

function Windows({ m, data, now }: { m: Managed; data: Dashboard; now: Date }) {
  const a = m.account;
  const status = statusOf(m);
  return (
    <>
      {(["5h", "7d"] as const).map((w) => (
        <td className="two" key={w}>
          {a === undefined ? (
            <span className="muted small">Not serving</span>
          ) : (
            <>
              <Meter
                quota={a.quota[w]}
                window={w}
                soft={data.soft}
                hard={data.hard}
                paused={status !== "active"}
                now={now}
              />
              {status === "active" ? (
                <Estimate burn={a.burn?.[w]} now={now} compact />
              ) : null}
            </>
          )}
        </td>
      ))}
    </>
  );
}

function Table({ data, now }: { data: Dashboard; now: Date }) {
  const rows = managedAccounts(data);
  if (rows.length === 0)
    return (
      <Empty title="No accounts.">The gateway has no account configured.</Empty>
    );
  return (
    <div className="table-wrap scroll">
      <table className="quota-table manage-table">
        <thead>
          <tr>
            <th scope="col">Account</th>
            <th scope="col">Status</th>
            <th scope="col">Plan</th>
            <th scope="col" className="win">
              5 h window
            </th>
            <th scope="col" className="win">
              7 d window
            </th>
            <th scope="col" className="n">
              Requests
              <br />
              <span className="faint">24 h / 7 d</span>
            </th>
            <th scope="col" className="n">
              Input / output
              <br />
              <span className="faint">tokens, 24 h / 7 d</span>
            </th>
            <th scope="col" className="n">
              Cache read / creation
              <br />
              <span className="faint">tokens, 24 h / 7 d</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((m) => {
            const t = m.account?.totals;
            return (
              <tr
                key={m.alias}
                className="link"
                onClick={() => {
                  goTo({ view: "account", arg: m.alias });
                }}
              >
                <td className="two wrap">
                  <div className="client-name">
                    <a href={accountHref(m.alias)}>{m.alias}</a>
                  </div>
                  <div className="small muted plan">
                    {m.account?.email ?? m.config?.email}
                  </div>
                </td>
                <td className="two">
                  <Status state={accountStatusWording(statusOf(m))} pill />
                  {m.account?.pause_reason === undefined ? null : (
                    <div className="small muted mono">
                      {m.account.pause_reason}
                    </div>
                  )}
                </td>
                <td className="muted">{m.account?.plan?.name ?? "–"}</td>
                <Windows m={m} data={data} now={now} />
                {t === undefined ? (
                  <td className="n muted" colSpan={3}>
                    No figures
                  </td>
                ) : (
                  <>
                    <td className="n">
                      <Pair
                        a={formatCount(t["24h"].requests)}
                        b={formatCount(t["7d"].requests)}
                      />
                    </td>
                    <td className="n">
                      <Pair
                        a={`${formatTokens(t["24h"].input)} / ${formatTokens(t["24h"].output)}`}
                        b={`${formatTokens(t["7d"].input)} / ${formatTokens(t["7d"].output)}`}
                      />
                    </td>
                    <td className="n">
                      <Pair
                        a={`${formatTokens(t["24h"].cache_read)} / ${formatTokens(t["24h"].cache_creation)}`}
                        b={`${formatTokens(t["7d"].cache_read)} / ${formatTokens(t["7d"].cache_creation)}`}
                      />
                    </td>
                  </>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/**
 * The way in to adding an account; where the page cannot write, one
 * sentence of why instead, and nothing at a gateway that cannot edit.
 */
function Actions({ data }: { data: Dashboard }) {
  const off = writeOffReason(data);
  if (off !== null) return <WriteNotice reason={off} />;
  if (!canCreate(data)) return null;
  return (
    <a className="btn primary" href={routeHref({ view: "add", arg: "1" })}>
      <Plus className="icon sm" aria-hidden="true" />
      Add account
    </a>
  );
}

/** Every account on one row: the list page of account management. */
export function ManageAccounts({ data, now }: { data: Dashboard; now: Date }) {
  return (
    <>
      <PageHead
        title="Manage accounts"
        sub="Settings, status and use of every account; open one to edit it"
      />
      <Section
        title="Accounts"
        note="Windows and estimates as on Accounts; an estimate runs the last quarter of a window's readings on to full."
        action={<Actions data={data} />}
      >
        <Table data={data} now={now} />
      </Section>
    </>
  );
}
