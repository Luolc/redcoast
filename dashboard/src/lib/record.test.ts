import { expect, it } from "vitest";
import { readRecord, sourceText, timelineOf, type HistoryRow } from "./record";

const SLACK = {
  kind: "slack",
  thread: "C0TEST/1700000000.000100",
  ts: "1700000000.000200",
};
const NONE = { kind: "" };

function row(
  time: string,
  action: string,
  changes: HistoryRow["changes"],
  source: HistoryRow["source"] = SLACK,
  ip = "admin-socket",
): HistoryRow {
  return { time, source_ip: ip, action, source, changes };
}

const created = row("2026-11-07T10:00:00Z", "account-create", {
  alias: { old: "", new: "sample-new" },
});
const record = (history: HistoryRow[], c: HistoryRow | null = created) => ({
  setup: null,
  created: c,
  history,
  history_truncated: false,
});

it("lists the rows oldest first, whatever order they come in", () => {
  const late = row("2026-11-07T10:05:00Z", "account-setup-report", {
    state: { old: "", new: "running" },
    step: { old: "", new: "email_sent" },
  });
  const early = row("2026-11-07T10:01:00Z", "account-setup", {
    setup: { old: "not_started", new: "requested:login_and_token" },
  });
  const out = timelineOf(record([late, created, early]));
  expect(out.map((x) => x.action)).toEqual([
    "account-create",
    "account-setup",
    "account-setup-report",
  ]);
});

it("keeps the order of rows that share a second", () => {
  const a = row("2026-11-07T10:01:00Z", "account-setup-report", {
    step: { old: "", new: "lock" },
  });
  const b = row("2026-11-07T10:01:00Z", "account-setup-report", {
    step: { old: "", new: "browser" },
  });
  expect(timelineOf(record([a, b], null)).map((x) => x.step)).toEqual([
    "lock",
    "browser",
  ]);
});

it("lists the creation once: marked in the history, or added when cut off", () => {
  const inside = timelineOf(record([created]));
  expect(inside).toHaveLength(1);
  expect(inside[0]?.kind).toBe("created");
  const report = row("2026-11-07T10:05:00Z", "account-setup-report", {});
  const cut = timelineOf(record([report]));
  expect(cut.map((x) => x.kind)).toEqual(["created", "report"]);
});

it("reads a report's codes and the outcome of a handover", () => {
  const wait = "2026-11-07T10:20:00Z";
  const out = timelineOf(
    record(
      [
        row("2026-11-07T10:06:00Z", "account-setup-report", {
          state: { old: "", new: "running" },
          step: { old: "", new: "awaiting_captcha" },
          wait_until: { old: "", new: wait },
        }),
        row("2026-11-07T10:07:00Z", "account-setup-report", {
          state: { old: "", new: "needs_human" },
          step: { old: "", new: "email_page" },
          reason: { old: "", new: "captcha_challenge" },
        }),
        row("2026-11-07T10:08:00Z", "account-setup-token", {
          setup: { old: "running", new: "failed:probe_refused" },
        }),
        row("2026-11-07T10:09:00Z", "account-setup-token", {
          setup: { old: "running", new: "done" },
        }),
      ],
      null,
    ),
  );
  expect(
    out.map((x) => [x.kind, x.state, x.step, x.reason, x.wait_until]),
  ).toEqual([
    ["report", "running", "awaiting_captcha", "", wait],
    ["report", "needs_human", "email_page", "captcha_challenge", ""],
    ["outcome", "failed", "", "probe_refused", ""],
    ["outcome", "done", "", "", ""],
  ]);
});

it("gives a Slack source as it is and any other source as the address", () => {
  const [slack] = timelineOf(record([created], null));
  expect(slack && sourceText(slack)).toBe(
    "slack C0TEST/1700000000.000100 1700000000.000200",
  );
  const page = row(
    "2026-11-07T10:00:00Z",
    "account-create",
    {},
    NONE,
    "192.0.2.50",
  );
  const [dashboard] = timelineOf(record([page], null));
  expect(dashboard && sourceText(dashboard)).toBe("192.0.2.50");
});

function answering(status: number, body: unknown): typeof fetch {
  return () => Promise.resolve(new Response(JSON.stringify(body), { status }));
}

it("reads the record, and says why when it cannot", async () => {
  const ok = await readRecord("sample-new", answering(200, record([created])));
  expect(ok.ok).toBe(true);
  expect(
    await readRecord(
      "sample-new",
      answering(501, { error: "account_write_unavailable" }),
    ),
  ).toEqual({ ok: false, text: "account_write_unavailable" });
  expect(
    await readRecord("sample-new", answering(200, { history: 3 })),
  ).toEqual({
    ok: false,
    text: "HTTP 200",
  });
  const down: typeof fetch = () => Promise.reject(new Error("no route"));
  expect((await readRecord("sample-new", down)).ok).toBe(false);
});

it("asks for the record of the alias, encoded", async () => {
  let asked = "";
  const spy: typeof fetch = (input) => {
    asked = typeof input === "string" ? input : "";
    return Promise.resolve(new Response("{}", { status: 404 }));
  };
  await readRecord("a b", spy);
  expect(asked).toBe("./accounts/a%20b/record");
});
