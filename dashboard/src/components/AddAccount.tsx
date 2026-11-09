import { ArrowLeft, ArrowRight, ShieldCheck } from "lucide-react";
import { useState } from "react";
import {
  canCreate,
  canWrite,
  findManaged,
  validAlias,
  validDevice,
  validEmail,
  validIp,
  validPlan,
  validProxyKind,
  validReceiver,
  validPort,
  validUuid,
  writeOffReason,
  type Managed,
} from "../lib/admin";
import {
  accountPath,
  writeAccount,
  type Refusal,
  type VerifyReport,
} from "../lib/adminApi";
import { goTo } from "../lib/route";
import type { Dashboard, ProxyRefOption } from "../lib/types";
import { accountStatusWording } from "../lib/words";
import {
  ChecksTable,
  Confirm,
  ProxyLoginField,
  RefLine,
  RefusalCallout,
  Stepper,
  TextField,
  WriteNotice,
} from "./AdminUi";
import { SetupPanel } from "./Setup";
import { Empty, PageHead, Section, Status } from "./ui";

const NO_OPTIONS: ProxyRefOption[] = [];

function go(step: number, alias?: string) {
  goTo({
    view: "add",
    arg: alias === undefined ? String(step) : `${String(step)}/${alias}`,
  });
}

function Nav({
  back,
  next,
  nextLabel = "Continue",
  nextDisabled = false,
  busy = false,
}: {
  back?: () => void;
  next?: () => void;
  nextLabel?: string;
  nextDisabled?: boolean;
  busy?: boolean;
}) {
  return (
    <div className="form-foot">
      {back === undefined ? null : (
        <button type="button" className="btn" onClick={back}>
          <ArrowLeft className="icon sm" aria-hidden="true" />
          Back
        </button>
      )}
      {next === undefined ? null : (
        <button
          type="button"
          className="btn primary"
          disabled={nextDisabled || busy}
          onClick={next}
        >
          {nextLabel}
          <ArrowRight className="icon sm" aria-hidden="true" />
        </button>
      )}
    </div>
  );
}

interface Form {
  alias: string;
  email: string;
  receiver: string;
  provider: string;
  kind: string;
  device: string;
  host: string;
  port: string;
  ip: string;
  plan: string;
  login: string;
}

function FillIn({ data, reload }: { data: Dashboard; reload: () => void }) {
  const options = data.proxy_ref_options ?? NO_OPTIONS;
  const [f, setF] = useState<Form>({
    alias: "",
    email: "",
    receiver: "",
    provider: "",
    kind: "",
    device: "",
    host: "",
    port: "",
    ip: "",
    plan: "",
    login: options.length > 0 ? "0" : "",
  });
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  const set = (k: keyof Form) => (v: string) => {
    setF({ ...f, [k]: v });
  };
  const taken = managedAliases(data).has(f.alias);
  const e = {
    alias: !validAlias(f.alias)
      ? "Lowercase letters, digits, - and _, up to 64"
      : taken
        ? "An account has this alias"
        : undefined,
    email: validEmail(f.email) ? undefined : "An email address",
    receiver: validReceiver(f.receiver)
      ? undefined
      : "An email address, no name",
    provider: validProxyKind(f.provider)
      ? undefined
      : "Letters, digits, . - and _, up to 64",
    kind: validProxyKind(f.kind)
      ? undefined
      : "Letters, digits, . - and _, up to 64",
    device: validDevice(f.device) ? undefined : "64 lowercase hex characters",
    port: validPort(f.port) ? undefined : "A port from 1 to 65535",
    ip: validIp(f.ip) ? undefined : "An IPv4 address, like 192.0.2.10",
    plan: validPlan(f.plan)
      ? undefined
      : "Lowercase letters, digits, . - and _",
  };
  const shown = (k: keyof typeof e) => (f[k] === "" ? undefined : e[k]);
  const login = options[Number(f.login)];
  const complete =
    f.host.trim() !== "" &&
    login !== undefined &&
    Object.values(e).every((x) => x === undefined);
  const save = () => {
    if (login === undefined) return;
    setBusy(true);
    void writeAccount("account-create", "POST", "./accounts", {
      alias: f.alias,
      email: f.email,
      device_id: f.device,
      access: "gateway",
      plan: f.plan,
      // Left out when empty, so the gateway stores nothing for it.
      ...(f.receiver === "" ? {} : { receiver_email: f.receiver }),
      proxy: {
        ...(f.provider === "" ? {} : { provider: f.provider }),
        ...(f.kind === "" ? {} : { type: f.kind }),
        direct: false,
        host: f.host.trim(),
        port: Number(f.port),
        expected_egress_ip: f.ip,
        username_ref: login.username_ref,
        password_ref: login.password_ref,
      },
    }).then((r) => {
      setBusy(false);
      if (r.ok) {
        reload();
        go(2, f.alias);
      } else setRefusal(r.refusal);
    });
  };
  return (
    <Section
      title="1 · Fill in the account"
      note="Nothing is saved until you save as testing. No credential is typed here: the references are only names."
    >
      <form
        className="panel form"
        onSubmit={(ev) => {
          ev.preventDefault();
        }}
      >
        {refusal === null ? null : (
          <RefusalCallout title="Not saved" refusal={refusal} />
        )}
        <div className="grid">
          <TextField
            label="Alias"
            mono
            value={f.alias}
            onChange={set("alias")}
            error={shown("alias")}
            hint="Fixed once created; names the token item"
            placeholder="sample-ah"
          />
          <TextField
            label="Email"
            value={f.email}
            onChange={set("email")}
            error={shown("email")}
            hint="Shown only"
            placeholder="name@example.test"
          />
          <TextField
            label="Receiver email"
            value={f.receiver}
            onChange={set("receiver")}
            error={shown("receiver")}
            hint="Optional; the address that receives the account's mail"
            placeholder="inbox@example.test"
          />
          <TextField
            label="Proxy provider"
            mono
            value={f.provider}
            onChange={set("provider")}
            error={shown("provider")}
            hint="Optional"
          />
          <TextField
            label="Proxy type"
            mono
            value={f.kind}
            onChange={set("kind")}
            error={shown("kind")}
            hint="Optional"
          />
          <TextField
            label="Device ID"
            mono
            value={f.device}
            onChange={set("device")}
            error={shown("device")}
            hint="64 hex characters; differs from every other account's"
          />
          <TextField
            label="Proxy host"
            mono
            value={f.host}
            onChange={set("host")}
            placeholder="proxy-1.example.test"
          />
          <TextField
            label="Proxy port"
            mono
            numeric
            value={f.port}
            onChange={set("port")}
            error={shown("port")}
          />
          <TextField
            label="Expected egress IP"
            mono
            value={f.ip}
            onChange={set("ip")}
            error={shown("ip")}
            hint="Must differ from every other Claude account's"
            placeholder="192.0.2.10"
          />
          <TextField
            label="Plan"
            mono
            value={f.plan}
            onChange={set("plan")}
            error={shown("plan")}
            hint="A plan the gateway knows, such as max"
          />
          <ProxyLoginField
            value={f.login}
            options={options}
            onChange={set("login")}
          />
        </div>
        <p className="small muted">
          The OAuth token reference is generated from the alias when the account
          is saved.
        </p>
        <Nav
          next={save}
          nextLabel="Save as testing"
          nextDisabled={!complete}
          busy={busy}
        />
      </form>
    </Section>
  );
}

function managedAliases(data: Dashboard): Set<string> {
  return new Set([
    ...data.accounts.map((a) => a.alias),
    ...(data.unserved_accounts ?? []).map((a) => a.alias),
  ]);
}

function Saved({ m }: { m: Managed }) {
  const c = m.config;
  if (c === undefined) return null;
  return (
    <Section
      title="2 · Saved, not serving"
      note="The account exists with status testing; the gateway does not select it."
    >
      <div className="panel">
        <div>
          <Status state={{ tone: "ok", word: "Saved" }} /> {m.alias}{" "}
          <Status state={accountStatusWording(c.status)} pill />
        </div>
        <dl className="facts">
          <div className="fact">
            <dt>Proxy</dt>
            <dd className="mono">
              {c.proxy.host}:{c.proxy.port}
            </dd>
          </div>
          <div className="fact">
            <dt>Expected egress IP</dt>
            <dd className="mono">{c.proxy.expected_egress_ip}</dd>
          </div>
        </dl>
        <p className="small muted">
          Next: log the account in and put its token in the secret manager at
          the reference below. The page never sees the token.
        </p>
        <RefLine label="OAuth token reference" reference={c.token_ref} />
        <Nav
          next={() => {
            go(3, m.alias);
          }}
          nextLabel="Go to login"
        />
      </div>
    </Section>
  );
}

function Verify({
  data,
  m,
  now,
  reload,
}: {
  data: Dashboard;
  m: Managed;
  now: Date;
  reload: () => void;
}) {
  const c = m.config;
  const [report, setReport] = useState<VerifyReport | undefined>();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  if (c === undefined) return null;
  const run = () => {
    setBusy(true);
    setRefusal(null);
    void writeAccount(
      "account-verify",
      "POST",
      accountPath(m.alias, "/verify"),
      { rev: c.rev },
    ).then((r) => {
      setBusy(false);
      if (r.ok) {
        setReport(r.verify);
        reload();
      } else setRefusal(r.refusal);
    });
  };
  return (
    <>
      <Section
        title="3 · Log in and set up the token"
        note="A login machine does the login; this page only asks for it."
      >
        <div className="panel">
          <RefLine label="OAuth token reference" reference={c.token_ref} />
          <p className="small muted">
            Start the setup below, or put the account&apos;s token in the secret
            manager at this reference by hand; then press Verify.
          </p>
        </div>
        <SetupPanel data={data} m={m} now={now} reload={reload} />
      </Section>
      <Section
        title="Verify"
        note="Run now at the gateway, which reads the reference, checks the exit and makes one small request through it. Changing the proxy, the exit IP or a reference voids all three."
      >
        {refusal === null ? null : (
          <RefusalCallout
            title="Not verified"
            refusal={refusal}
            onRefresh={reload}
          />
        )}
        <ChecksTable verify={c.verify} report={report} now={now} />
        <div className="form-foot">
          <button type="button" className="btn" disabled={busy} onClick={run}>
            <ShieldCheck className="icon sm" aria-hidden="true" />
            Verify
          </button>
        </div>
        <Nav
          back={() => {
            go(2, m.alias);
          }}
          next={() => {
            go(4, m.alias);
          }}
          nextDisabled={c.verify.token_ok !== true}
        />
      </Section>
    </>
  );
}

function Details({ m, reload }: { m: Managed; reload: () => void }) {
  const c = m.config;
  const [uuid, setUuid] = useState(c?.account_uuid ?? "");
  const [device, setDevice] = useState(c?.device_id ?? "");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  if (c === undefined) return null;
  const changed = uuid !== c.account_uuid || device !== c.device_id;
  const save = () => {
    if (!changed) {
      go(5, m.alias);
      return;
    }
    setBusy(true);
    void writeAccount("account-update", "PATCH", accountPath(m.alias), {
      rev: c.rev,
      account_uuid: uuid,
      device_id: device,
    }).then((r) => {
      setBusy(false);
      if (r.ok) {
        reload();
        go(5, m.alias);
      } else setRefusal(r.refusal);
    });
  };
  return (
    <Section
      title="4 · Account details"
      note="Read from the logged-in account; they change the request body, not the exit."
    >
      <form
        className="panel form"
        onSubmit={(ev) => {
          ev.preventDefault();
        }}
      >
        {refusal === null ? null : (
          <RefusalCallout
            title="Not saved"
            refusal={refusal}
            onRefresh={reload}
          />
        )}
        <div className="grid">
          <TextField
            label="Account UUID"
            mono
            value={uuid}
            onChange={setUuid}
            error={uuid !== "" && !validUuid(uuid) ? "Not a UUID" : undefined}
            hint="Lowercase, 8-4-4-4-12"
          />
          <TextField
            label="Device ID"
            mono
            value={device}
            onChange={setDevice}
            error={
              device !== "" && !validDevice(device)
                ? "64 lowercase hex characters"
                : undefined
            }
            hint="64 hex characters"
          />
        </div>
        <Nav
          back={() => {
            go(3, m.alias);
          }}
          next={save}
          nextLabel="Save and go on"
          nextDisabled={!validUuid(uuid) || !validDevice(device)}
          busy={busy}
        />
      </form>
    </Section>
  );
}

function Activate({
  m,
  now,
  reload,
}: {
  m: Managed;
  now: Date;
  reload: () => void;
}) {
  const c = m.config;
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  if (c === undefined) return null;
  const activate = () => {
    setBusy(true);
    void writeAccount(
      "account-activate",
      "POST",
      accountPath(m.alias, "/activate"),
      { rev: c.rev },
    ).then((r) => {
      setBusy(false);
      reload();
      if (r.ok) goTo({ view: "account", arg: m.alias });
      else setRefusal(r.refusal);
    });
  };
  return (
    <Section
      title="5 · Activate"
      note="The gateway runs the three checks again at this moment, then checks the plan and that the exit is unique among Claude accounts."
    >
      <div className="panel">
        {refusal === null ? null : (
          <RefusalCallout
            title="Not activated"
            refusal={refusal}
            onRefresh={reload}
          />
        )}
        <ChecksTable verify={c.verify} now={now} />
        <ul className="small muted pre">
          <li>
            Exit: <span className="mono">{c.proxy.expected_egress_ip}</span>
          </li>
          <li>
            Details filled:{" "}
            {c.account_uuid === "" ? "account UUID missing" : "account UUID"},{" "}
            {c.device_id === "" ? "device ID missing" : "device ID"}
          </li>
        </ul>
        <div className="form-foot">
          <Confirm
            key={refusal === null ? "a" : "b"}
            label="Activate"
            primary
            busy={busy}
            yes="Yes, activate"
            question={
              <>
                Activate {m.alias}? It starts serving sessions as soon as the
                gateway reloads.
              </>
            }
            onYes={activate}
          />
          <button
            type="button"
            className="btn"
            onClick={() => {
              go(4, m.alias);
            }}
          >
            <ArrowLeft className="icon sm" aria-hidden="true" />
            Back
          </button>
        </div>
      </div>
    </Section>
  );
}

/** The five steps of adding an account, as one page that follows the step. */
export function AddAccount({
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
  const [stepArg = "1", alias = ""] = (arg ?? "1").split("/");
  const parsed = Number.parseInt(stepArg, 10);
  const wanted = Number.isNaN(parsed) ? 1 : Math.min(5, Math.max(1, parsed));
  const m = alias === "" ? undefined : findManaged(data, alias);
  // A step after the first belongs to an account that exists.
  const step = alias === "" ? 1 : wanted;
  const off = writeOffReason(data);
  const allowed = step === 1 ? canCreate(data) : canWrite(data);
  const head = (
    <PageHead
      title="Add account"
      sub="Five steps from a form to an account that serves"
    />
  );
  if (!allowed)
    return (
      <>
        {head}
        <Empty title="Adding accounts is off here.">
          {off === null ? (
            data.accounts_editable === true ? (
              "The gateway has no token reference template, so it cannot create accounts."
            ) : (
              "This gateway keeps its accounts elsewhere."
            )
          ) : (
            <WriteNotice reason={off} />
          )}
        </Empty>
      </>
    );
  return (
    <>
      {head}
      <Stepper step={step} />
      {step === 1 ? <FillIn data={data} reload={reload} /> : null}
      {step > 1 && m === undefined ? (
        <p className="muted">Waiting for the gateway to list {alias}…</p>
      ) : null}
      {m === undefined ? null : (
        <>
          {step === 2 ? <Saved m={m} /> : null}
          {step === 3 ? (
            <Verify data={data} m={m} now={now} reload={reload} />
          ) : null}
          {step === 4 ? <Details m={m} reload={reload} /> : null}
          {step === 5 ? <Activate m={m} now={now} reload={reload} /> : null}
        </>
      )}
    </>
  );
}
