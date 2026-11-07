// Account management: which accounts the Manage pages list, whether the page
// may write, and the edit of one account as a draft, its validation and the
// request body it makes.
import { accountPath, writeAccount, type Outcome } from "./adminApi";
import type {
  Account,
  AccountConfig,
  Dashboard,
  ProxyRefOption,
} from "./types";

/** An account of the accounts table, with its served state when it has one. */
export interface Managed {
  alias: string;
  // Absent at a gateway that keeps no accounts table (Codex).
  config?: AccountConfig | undefined;
  // Absent while the gateway does not serve the account.
  account?: Account | undefined;
}

/** The served accounts, then those the gateway does not serve. */
export function managedAccounts(d: Dashboard): Managed[] {
  const served: Managed[] = d.accounts.map((account) => ({
    alias: account.alias,
    config: account.config,
    account,
  }));
  const rest: Managed[] = (d.unserved_accounts ?? []).map((config) => ({
    alias: config.alias,
    config,
  }));
  return [...served, ...rest];
}

export function findManaged(d: Dashboard, alias: string): Managed | undefined {
  return managedAccounts(d).find((m) => m.alias === alias);
}

/** The status of an account; a served account without a row is active. */
export function statusOf(m: Managed): string {
  return m.config?.status ?? "active";
}

export function planOf(m: Managed): string {
  return m.account?.plan?.name ?? "";
}

/** Whether the gateway edits accounts and takes writes from this request. */
export function canWrite(d: Dashboard): boolean {
  return d.accounts_editable === true && d.write?.enabled === true;
}

export function canCreate(d: Dashboard): boolean {
  return canWrite(d) && d.write?.can_create === true;
}

/**
 * Why writing is off, for a gateway that edits accounts; null when it is on,
 * or when the gateway does not edit accounts at all.
 */
export function writeOffReason(d: Dashboard): string | null {
  if (d.accounts_editable !== true || d.write === undefined) return null;
  if (d.write.enabled) return null;
  return d.write.reason ?? "Writing is off.";
}

// The alias, plan, email, device ID and account UUID take the shapes the
// gateway checks.
const ALIAS_PATTERN = /^[a-z0-9][a-z0-9_-]{0,63}$/;
const PLAN = /^[a-z0-9][a-z0-9._-]{0,31}$/;
const EMAIL = /^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const DEVICE = /^[0-9a-f]{64}$/;
const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

export function validIp(v: string): boolean {
  const m = IPV4.exec(v);
  return m?.slice(1).every((p) => Number(p) <= 255) === true;
}

export function validPort(v: string): boolean {
  return /^\d{1,5}$/.test(v) && Number(v) >= 1 && Number(v) <= 65535;
}

export function validAlias(v: string): boolean {
  return ALIAS_PATTERN.test(v);
}

export function validPlan(v: string): boolean {
  return PLAN.test(v);
}

export function validEmail(v: string): boolean {
  return v.length <= 254 && EMAIL.test(v);
}

export function validUuid(v: string): boolean {
  return UUID.test(v);
}

export function validDevice(v: string): boolean {
  return DEVICE.test(v);
}

/** The editable fields of an account, as the text the form holds. */
export interface Draft {
  status: string;
  plan: string;
  host: string;
  port: string;
  ip: string;
  device: string;
  uuid: string;
  // The proxy login: the index in the gateway's list of those in use.
  login: string;
}

function loginIndex(
  options: ProxyRefOption[],
  username: string,
  password: string,
): string {
  return String(
    options.findIndex(
      (o) => o.username_ref === username && o.password_ref === password,
    ),
  );
}

export function draftOf(m: Managed, options: ProxyRefOption[]): Draft {
  const c = m.config;
  return {
    status: statusOf(m),
    plan: planOf(m),
    host: c?.proxy.host ?? "",
    port: String(c?.proxy.port ?? ""),
    ip: c?.proxy.expected_egress_ip ?? "",
    device: c?.device_id ?? "",
    uuid: c?.account_uuid ?? "",
    login: loginIndex(
      options,
      c?.proxy.username_ref ?? "",
      c?.proxy.password_ref ?? "",
    ),
  };
}

const DRAFT_KEYS: (keyof Draft)[] = [
  "status",
  "plan",
  "host",
  "port",
  "ip",
  "device",
  "uuid",
  "login",
];

const DRAFT_LABELS: Record<keyof Draft, string> = {
  status: "Status",
  plan: "Plan",
  host: "Proxy host",
  port: "Proxy port",
  ip: "Expected egress IP",
  device: "Device ID",
  uuid: "Account UUID",
  login: "Proxy login",
};

/** What is wrong with the draft; a direct account has no proxy fields to check. */
export function draftErrors(
  d: Draft,
  direct = false,
): Partial<Record<keyof Draft, string>> {
  const e: Partial<Record<keyof Draft, string>> = {};
  if (!direct) {
    if (d.host.trim() === "") e.host = "Required";
    if (!validPort(d.port)) e.port = "A port from 1 to 65535";
    if (!validIp(d.ip)) e.ip = "An IPv4 address, like 192.0.2.10";
  }
  if (d.plan !== "" && !validPlan(d.plan))
    e.plan = "Lowercase letters, digits, . - and _, up to 32";
  if (d.uuid !== "" && !validUuid(d.uuid)) e.uuid = "Not a UUID";
  if (d.device !== "" && !validDevice(d.device))
    e.device = "64 lowercase hex characters";
  return e;
}

export interface Change {
  key: keyof Draft;
  label: string;
  from: string;
  to: string;
}

/** The fields the draft changes. */
export function changesOf(was: Draft, d: Draft): Change[] {
  return DRAFT_KEYS.filter((k) => was[k] !== d[k]).map((k) => ({
    key: k,
    label: DRAFT_LABELS[k],
    from: was[k],
    to: d[k],
  }));
}

/**
 * Saves the draft as an edit made against the revision and values the form
 * was opened on, not against a later reading of the page.
 */
export function saveEdit(
  alias: string,
  base: { rev: number; was: Draft },
  draft: Draft,
  options: ProxyRefOption[],
  post?: typeof fetch,
): Promise<Outcome> {
  return writeAccount(
    "account-update",
    "PATCH",
    accountPath(alias),
    patchBody(base.rev, changesOf(base.was, draft), options),
    post,
  );
}

/** The body of the PATCH that makes the changes: the revision and the fields changed. */
export function patchBody(
  rev: number,
  changes: Change[],
  options: ProxyRefOption[],
): Record<string, unknown> {
  const body: Record<string, unknown> = { rev };
  const proxy: Record<string, unknown> = {};
  for (const c of changes) {
    switch (c.key) {
      case "status":
        body["status"] = c.to;
        break;
      case "plan":
        // A plan cleared in the form is no change the gateway can take.
        if (c.to !== "") body["plan"] = c.to;
        break;
      case "device":
        body["device_id"] = c.to;
        break;
      case "uuid":
        body["account_uuid"] = c.to;
        break;
      case "host":
        proxy["host"] = c.to;
        break;
      case "port":
        proxy["port"] = Number(c.to);
        break;
      case "ip":
        proxy["expected_egress_ip"] = c.to;
        break;
      case "login": {
        const o = options[Number(c.to)];
        if (o !== undefined) {
          proxy["username_ref"] = o.username_ref;
          proxy["password_ref"] = o.password_ref;
        }
        break;
      }
    }
  }
  if (Object.keys(proxy).length > 0) body["proxy"] = proxy;
  return body;
}
