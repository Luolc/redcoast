// An account's record: the answer of GET /accounts/{alias}/record, and the
// timeline made of it. Every field is a code, a reference, a setting or a time;
// the gateway puts no credential in it. A source is a record of where a change
// came from, not a sign that anyone approved it.
import { accountPath } from "./adminApi";
import {
  arrayOf,
  isString,
  nullable,
  object,
  optional,
  recordOf,
  type Guard,
} from "./guard";
import { isTaskSource, type SetupBlock, type TaskSource } from "./types";

interface FieldChange {
  old: string;
  new: string;
}

/** One audit row of the account. */
export interface HistoryRow {
  time: string;
  source_ip: string;
  action: string;
  source: TaskSource;
  changes: Record<string, FieldChange>;
}

export interface AccountRecord {
  // Absent when setup is off on the gateway.
  setup?: SetupSummary | null | undefined;
  created: HistoryRow | null;
  history: HistoryRow[];
  history_truncated: boolean;
}

const isChange = object<FieldChange>({ old: isString, new: isString });
const isHistoryRow = object<HistoryRow>({
  time: isString,
  source_ip: isString,
  action: isString,
  source: isTaskSource,
  changes: recordOf(isChange),
});

// The setup block is read as far as the timeline uses it; the page's own guard
// for dashboard.json checks the whole.
type SetupSummary = Pick<
  SetupBlock,
  "state" | "step" | "reason" | "wait_until"
>;
const isSetupSummary = object<SetupSummary>({
  state: isString,
  step: nullable(isString),
  reason: nullable(isString),
  wait_until: optional(nullable(isString)),
});

const isRecord: Guard<AccountRecord> = object<AccountRecord>({
  setup: optional(nullable(isSetupSummary)),
  created: nullable(isHistoryRow),
  history: arrayOf(isHistoryRow),
  history_truncated: (v): v is boolean => typeof v === "boolean",
});

export type RecordOutcome =
  { ok: true; record: AccountRecord } | { ok: false; text: string };

/** Reads the record of alias; resolves to the answer or why not, never throws. */
export async function readRecord(
  alias: string,
  get: typeof fetch = fetch,
): Promise<RecordOutcome> {
  try {
    const response = await get(accountPath(alias, "/record"), {
      cache: "no-store",
    });
    const text = await response.text();
    let answer: unknown;
    try {
      answer = JSON.parse(text);
    } catch {
      answer = undefined;
    }
    if (response.ok && isRecord(answer)) return { ok: true, record: answer };
    if (!response.ok && object<{ error: string }>({ error: isString })(answer))
      return { ok: false, text: answer.error };
    return { ok: false, text: `HTTP ${String(response.status)}` };
  } catch (e) {
    return { ok: false, text: String(e) };
  }
}

/** One line of the timeline. */
export interface TimelineItem {
  at: string;
  kind: "created" | "change" | "report" | "outcome";
  action: string;
  /** The state, step and reason of a report, or the outcome of a handover. */
  state: string;
  step: string;
  reason: string;
  wait_until: string;
  source: TaskSource;
  sourceIp: string;
}

const REPORT = "account-setup-report";
const TOKEN = "account-setup-token";

function item(row: HistoryRow, kind: TimelineItem["kind"]): TimelineItem {
  const c = row.changes;
  if (row.action === REPORT) kind = "report";
  else if (row.action === TOKEN) kind = "outcome";
  const outcome = c["setup"]?.new ?? "";
  const split = outcome.indexOf(":");
  const state = kind === "outcome" ? outcome : (c["state"]?.new ?? "");
  return {
    at: row.time,
    kind,
    action: row.action,
    state: kind === "outcome" && split >= 0 ? outcome.slice(0, split) : state,
    step: c["step"]?.new ?? "",
    reason:
      kind === "outcome" && split >= 0
        ? outcome.slice(split + 1)
        : (c["reason"]?.new ?? ""),
    wait_until: c["wait_until"]?.new ?? "",
    source: row.source,
    sourceIp: row.source_ip,
  };
}

/**
 * The record's lines in time order, oldest first: the creation, then every
 * audit row. The creation row is the one the gateway names as created; it also
 * stands among the history when that is not cut short, and is then listed once.
 */
export function timelineOf(r: AccountRecord): TimelineItem[] {
  const created = r.created;
  const isCreated = (h: HistoryRow) =>
    created !== null && h.time === created.time && h.action === created.action;
  const rows = r.history.map((h) =>
    item(h, isCreated(h) ? "created" : "change"),
  );
  if (created !== null && !r.history.some(isCreated))
    rows.push(item(created, "created"));
  return rows
    .map((x, i) => ({ x, i }))
    .sort((a, b) => {
      const by = Date.parse(a.x.at) - Date.parse(b.x.at);
      return by === 0 ? a.i - b.i : by;
    })
    .map(({ x }) => x);
}

/** Where a change came from, as text: a Slack message as given, else the IP. */
export function sourceText(
  x: Pick<TimelineItem, "source" | "sourceIp">,
): string {
  if (x.source.kind === "slack")
    return `slack ${x.source.thread ?? ""} ${x.source.ts ?? ""}`.trimEnd();
  return x.sourceIp;
}
