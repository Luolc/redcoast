import { ArrowLeft, Pencil, Play } from "lucide-react";
import { useState } from "react";
import {
  canWrite,
  changesOf,
  draftErrors,
  draftOf,
  findManaged,
  saveEdit,
  planOf,
  statusOf,
  writeOffReason,
  type Draft,
  type Managed,
} from "../lib/admin";
import type { Refusal } from "../lib/adminApi";
import { formatCount, formatPercent, formatTokens } from "../lib/format";
import { accountHref, goTo, routeHref } from "../lib/route";
import { byMachine, windowSpan } from "../lib/stats";
import type { Dashboard, ProxyRefOption } from "../lib/types";
import { accountStatusWording, egressWording } from "../lib/words";
import {
  Confirm,
  Estimate,
  ProxyLoginField,
  RefLine,
  RefusalCallout,
  SelectField,
  TextField,
  WriteNotice,
} from "./AdminUi";
import { QuotaCurve } from "./charts";
import {
  Ago,
  Empty,
  Meter,
  PageHead,
  Section,
  Share,
  Status,
  Time,
} from "./ui";

const NO_OPTIONS: ProxyRefOption[] = [];

function Fact({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="fact">
      <dt>{k}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function Unset({ value, mono }: { value: string; mono?: string }) {
  return value === "" ? (
    <span className="faint">not set</span>
  ) : (
    <span className={mono}>{value}</span>
  );
}

function Settings({ m, now }: { m: Managed; now: Date }) {
  const c = m.config;
  const a = m.account;
  const check = a?.egress_check;
  const w = egressWording(check?.outcome);
  return (
    <div className="two-col wide-left">
      <dl className="facts panel">
        <Fact k="Email">{c?.email ?? a?.email}</Fact>
        {c === undefined ? null : (
          <Fact k="Revision">
            <span className="num">{c.rev}</span>
          </Fact>
        )}
        <Fact k="Plan">{planOf(m) === "" ? "–" : planOf(m)}</Fact>
        {c === undefined ? null : (
          <Fact k="Proxy">
            {c.proxy.direct ? (
              "Direct, no proxy"
            ) : (
              <span className="mono">
                {c.proxy.host}:{c.proxy.port}
              </span>
            )}
          </Fact>
        )}
        <Fact k="Expected egress IP">
          <span className="mono">
            {c?.proxy.expected_egress_ip ?? a?.expected_egress_ip}
          </span>{" "}
          {check === undefined ? null : (
            <>
              <Status state={{ tone: w.tone, word: w.text }} />{" "}
              <span className="muted small">
                <Ago iso={check.at} now={now} />
              </span>
            </>
          )}
        </Fact>
        {c === undefined ? null : (
          <>
            <Fact k="Account UUID">
              <Unset value={c.account_uuid} mono="mono" />
            </Fact>
            <Fact k="Device ID">
              <Unset value={c.device_id} mono="mono small" />
            </Fact>
          </>
        )}
      </dl>
      {c === undefined ? null : (
        <div className="panel">
          <h3 className="panel-title">Credentials</h3>
          <p className="small muted">
            Held in the secret manager. The page shows the reference, never a
            value.
          </p>
          <RefLine
            label="OAuth token (from the alias)"
            reference={c.token_ref}
          />
          {c.proxy.direct ? null : (
            <>
              <RefLine label="Proxy user" reference={c.proxy.username_ref} />
              <RefLine
                label="Proxy password"
                reference={c.proxy.password_ref}
              />
            </>
          )}
        </div>
      )}
    </div>
  );
}

function EditForm({
  m,
  data,
  reload,
}: {
  m: Managed;
  data: Dashboard;
  reload: () => void;
}) {
  const options = data.proxy_ref_options ?? NO_OPTIONS;
  const config = m.config;
  // The values and the revision the form was opened on: the edit is made
  // against these, not against a later reading of the page.
  const [base] = useState(() => ({
    rev: config?.rev ?? 0,
    was: draftOf(m, options),
  }));
  const [d, setD] = useState<Draft>(base.was);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  if (config === undefined) return null;
  const set = (k: keyof Draft) => (v: string) => {
    setD({ ...d, [k]: v });
  };
  const diff = changesOf(base.was, d);
  const err = draftErrors(d, config.proxy.direct);
  const back = () => {
    goTo({ view: "account", arg: m.alias });
  };
  const save = () => {
    setBusy(true);
    void saveEdit(m.alias, base, d, options).then((r) => {
      setBusy(false);
      if (r.ok) {
        reload();
        back();
      } else setRefusal(r.refusal);
    });
  };
  const statuses = ["testing", "paused", "suspended", "retired"];
  if (base.was.status === "active") statuses.unshift("active");
  const direct = config.proxy.direct;
  return (
    <form
      className="panel form"
      onSubmit={(e) => {
        e.preventDefault();
      }}
    >
      {refusal === null ? null : (
        <RefusalCallout
          title="Not saved"
          refusal={refusal}
          onRefresh={() => {
            reload();
            back();
          }}
        />
      )}
      <div className="grid">
        <SelectField
          label="Status"
          value={d.status}
          onChange={set("status")}
          options={statuses.map((s) => ({
            value: s,
            label: accountStatusWording(s).word,
          }))}
          hint="Only active serves sessions, and Activate on the add page sets it; there is no delete, retire instead"
        />
        <TextField
          label="Plan"
          mono
          value={d.plan}
          onChange={set("plan")}
          error={err.plan}
          hint="The plan in effect from now on"
        />
        {direct ? null : (
          <>
            <TextField
              label="Proxy host"
              mono
              value={d.host}
              onChange={set("host")}
              error={err.host}
            />
            <TextField
              label="Proxy port"
              mono
              numeric
              value={d.port}
              onChange={set("port")}
              error={err.port}
            />
            <TextField
              label="Expected egress IP"
              mono
              value={d.ip}
              onChange={set("ip")}
              error={err.ip}
              hint="The gateway checks the exit reads this address"
            />
            <ProxyLoginField
              value={d.login}
              options={options}
              onChange={set("login")}
            />
          </>
        )}
        <TextField
          label="Device ID"
          mono
          value={d.device}
          onChange={set("device")}
          error={err.device}
        />
        <TextField
          label="Account UUID"
          mono
          value={d.uuid}
          onChange={set("uuid")}
          error={err.uuid}
        />
        <TextField
          label="Alias"
          readOnly
          mono
          value={config.alias}
          hint="Fixed once created"
        />
        <TextField
          label="Email"
          readOnly
          value={config.email}
          hint="Shown only"
        />
        <TextField
          label="OAuth token reference"
          readOnly
          mono
          value={config.token_ref}
          hint="Generated from the alias; to change the token, change the item's value in the secret manager, then verify"
        />
      </div>
      <div className="form-foot">
        <Confirm
          key={refusal === null ? "ask" : "refused"}
          label={
            diff.length === 0
              ? "Save"
              : `Save ${String(diff.length)} change${diff.length === 1 ? "" : "s"}`
          }
          primary
          disabled={diff.length === 0 || Object.keys(err).length > 0}
          busy={busy}
          yes="Yes, save"
          onYes={save}
          question={
            <>
              <div>
                Change {m.alias}? A new proxy or exit makes the gateway check
                the exit again.
              </div>
              <ul className="diff">
                {diff.map((c) => (
                  <li key={c.key}>
                    {c.label}:{" "}
                    <span className="mono">
                      {c.from === "" ? "not set" : c.from}
                    </span>
                    {" → "}
                    <span className="mono">
                      {c.to === "" ? "not set" : c.to}
                    </span>
                  </li>
                ))}
              </ul>
            </>
          }
        />
        <button type="button" className="btn" onClick={back}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function Quota({ m, data, now }: { m: Managed; data: Dashboard; now: Date }) {
  const a = m.account;
  if (a === undefined || Object.keys(a.quota).length === 0)
    return (
      <Empty title="No quota readings yet.">
        An account that is not serving has none.
      </Empty>
    );
  return (
    <div className="two-col wide-left even">
      {(["5h", "7d"] as const).map((w) => {
        const span = windowSpan(w) ?? 1;
        const to = Date.parse(data.at);
        const readings = a.quota_history?.[w] ?? [];
        return (
          <div className="panel" key={w}>
            <h3 className="panel-title">
              {w === "5h" ? "5 h window" : "7 d window"}
            </h3>
            <Meter
              quota={a.quota[w]}
              window={w}
              soft={data.soft}
              hard={data.hard}
              paused={statusOf(m) !== "active"}
              now={now}
            />
            <Estimate burn={a.burn?.[w]} now={now} />
            {readings.length === 0 ? null : (
              <>
                <QuotaCurve
                  label={`${w} window: ${String(readings.length)} readings`}
                  readings={readings}
                  from={to - span}
                  to={to}
                  soft={data.soft}
                  reset={null}
                />
                <div className="curve-axis">
                  <span>{w} ago</span>
                  <span>now</span>
                </div>
              </>
            )}
          </div>
        );
      })}
    </div>
  );
}

function Machines({ m, data }: { m: Managed; data: Dashboard }) {
  const requests = new Map(
    byMachine((data.usage ?? []).filter((u) => u.alias === m.alias)).map(
      (c) => [c.machine, c.requests],
    ),
  );
  const burns = m.account?.burn?.by_machine ?? [];
  const names = [
    ...new Set([...requests.keys(), ...burns.map((b) => b.machine)]),
  ].filter((n) => n !== "");
  if (names.length === 0)
    return <Empty title="No requests in the last 24 h." />;
  const of = (machine: string, window: string) =>
    burns.find((b) => b.machine === machine && b.window === window);
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Client machine</th>
            <th scope="col" className="n">
              Requests
              <br />
              <span className="faint">24 h, counted</span>
            </th>
            {(["5h", "7d"] as const).map((w) => (
              <th scope="col" key={w}>
                Share of the {w} window
                <br />
                <span className="faint">estimate</span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {names.map((n) => (
            <tr key={n}>
              <td className="mono">{n}</td>
              <td className="n">{formatCount(requests.get(n) ?? 0)}</td>
              {(["5h", "7d"] as const).map((w) => {
                const b = of(n, w);
                return (
                  <td key={w}>
                    {b === undefined ? (
                      <span className="faint">no estimate</span>
                    ) : (
                      <span className="share-cell">
                        <span className="num">≈ {formatPercent(b.share)}</span>
                        <Share part={b.share} whole={1} />
                        <span className="num small muted">
                          {(b.rate_per_hour * 100).toFixed(1)} %/h
                        </span>
                      </span>
                    )}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Tokens({ m }: { m: Managed }) {
  const t = m.account?.totals;
  if (t === undefined) return <Empty title="No figures for this account." />;
  const rows = [
    ["Requests", "requests"],
    ["Input", "input"],
    ["Output", "output"],
    ["Cache read", "cache_read"],
    ["Cache creation", "cache_creation"],
  ] as const;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Kind</th>
            <th scope="col" className="n">
              24 h
            </th>
            <th scope="col" className="n">
              7 d
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([k, key]) => {
            const f = key === "requests" ? formatCount : formatTokens;
            return (
              <tr key={k}>
                <td>{k}</td>
                <td className="n">{f(t["24h"][key])}</td>
                <td className="n">{f(t["7d"][key])}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function BoundSessions({
  alias,
  data,
  now,
}: {
  alias: string;
  data: Dashboard;
  now: Date;
}) {
  const sessions = data.sessions.filter((s) => s.alias === alias);
  if (sessions.length === 0) return <Empty title="No session bound." />;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Session</th>
            <th scope="col">Client machine</th>
            <th scope="col">Last seen</th>
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id}>
              <td className="mono">{s.id}</td>
              <td className="mono">{s.client_machine}</td>
              <td className="muted">
                <Ago iso={s.last_seen} now={now} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** One account: every setting, its use, and the form to change it. */
export function AccountDetail({
  data,
  now,
  arg,
  reload,
}: {
  data: Dashboard;
  now: Date;
  arg: string | undefined;
  reload: () => void;
}) {
  const [alias = "", mode] = (arg ?? "").split("/");
  const m = findManaged(data, alias);
  if (m === undefined)
    return (
      <Empty title="No such account.">
        <a href={routeHref({ view: "manage" })}>Back to the list</a>
      </Empty>
    );
  const status = statusOf(m);
  const writable = canWrite(data) && m.config !== undefined;
  const editing = mode === "edit" && writable;
  const off = writeOffReason(data);
  const a = m.account;
  return (
    <>
      <PageHead
        title={m.alias}
        sub={
          <>
            <Status state={accountStatusWording(status)} pill />{" "}
            {m.config?.email ?? a?.email}
            {planOf(m) === "" ? "" : ` · ${planOf(m)}`}
          </>
        }
      />
      <div className="toolbar">
        <a className="btn" href={routeHref({ view: "manage" })}>
          <ArrowLeft className="icon sm" aria-hidden="true" />
          All accounts
        </a>
        {editing || !writable ? null : (
          <a className="btn primary" href={accountHref(m.alias, true)}>
            <Pencil className="icon sm" aria-hidden="true" />
            Edit settings
          </a>
        )}
        {writable && status === "testing" ? (
          <a
            className="btn"
            href={routeHref({ view: "add", arg: `3/${m.alias}` })}
          >
            <Play className="icon sm" aria-hidden="true" />
            Continue setup
          </a>
        ) : null}
        {off === null ? null : <WriteNotice reason={off} />}
      </div>
      {a?.paused_until === undefined ? null : (
        <div className="alert">
          <Status state={{ tone: "warn", word: "Paused" }} /> until{" "}
          <Time iso={a.paused_until}>
            {a.paused_until.slice(5, 16).replace("T", " ")} UTC
          </Time>
          {a.pause_reason === undefined ? null : (
            <span className="mono"> · {a.pause_reason}</span>
          )}
        </div>
      )}
      <Section
        title={editing ? "Edit settings" : "Settings"}
        note={editing ? "Nothing changes until you confirm" : undefined}
      >
        {editing ? (
          <EditForm m={m} data={data} reload={reload} />
        ) : (
          <Settings m={m} now={now} />
        )}
      </Section>
      <Section
        title="Quota"
        note="Estimates run the last quarter of a window's readings on to full; use is not even, so they are rough."
      >
        <Quota m={m} data={data} now={now} />
      </Section>
      <Section
        title="Use by machine"
        note="Requests are counted; the share of the window is an estimate, apportioned by each machine's share of the account's tokens."
      >
        <Machines m={m} data={data} />
      </Section>
      <Section
        title="Requests and tokens"
        note="Last 24 h and last 7 d, as the responses reported them"
      >
        <Tokens m={m} />
      </Section>
      <Section title="Bound sessions">
        <BoundSessions alias={m.alias} data={data} now={now} />
      </Section>
    </>
  );
}
