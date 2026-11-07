import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import type { AccountRecord, HistoryRow } from "../lib/record";
import { TimelineView } from "./Timeline";

const now = new Date("2026-11-07T10:10:00Z");
const slack = {
  kind: "slack",
  thread: "C0TEST/1700000000.000100",
  ts: "1700000000.000200",
};

function row(
  time: string,
  action: string,
  changes: HistoryRow["changes"],
): HistoryRow {
  return { time, source_ip: "admin-socket", action, source: slack, changes };
}

const created = row("2026-11-07T10:00:00Z", "account-create", {});

function html(r: Partial<AccountRecord>): string {
  return renderToStaticMarkup(
    <TimelineView
      record={{ created, history: [created], history_truncated: false, ...r }}
      now={now}
    />,
  );
}

const text = (markup: string) =>
  markup.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ");

it("shows the creation with its time and the Slack source as given", () => {
  const out = text(html({}));
  expect(out).toContain("2026-11-07 10:00:00");
  expect(out).toContain("Created");
  expect(out).toContain("slack C0TEST/1700000000.000100 1700000000.000200");
});

it("shows each step of a task in time order with its result", () => {
  const out = text(
    html({
      history: [
        row("2026-11-07T10:06:00Z", "account-setup-report", {
          state: { old: "", new: "needs_human" },
          step: { old: "", new: "email_page" },
          reason: { old: "", new: "captcha_challenge" },
        }),
        created,
        row("2026-11-07T10:03:00Z", "account-setup-report", {
          state: { old: "", new: "running" },
          step: { old: "", new: "browser" },
        }),
      ],
    }),
  );
  const at = (s: string) => out.indexOf(s);
  expect(at("Created")).toBeLessThan(at("Starting the browser"));
  expect(at("Starting the browser")).toBeLessThan(at("On the email page"));
  expect(out).toContain("Needs a person");
  expect(out).toContain("The login page asked for a CAPTCHA");
});

it("says where the latest task stopped and why", () => {
  const out = text(
    html({
      setup: {
        state: "failed",
        step: "probe",
        reason: "probe_refused",
        wait_until: null,
      },
    }),
  );
  expect(out).toContain("Failed at: Testing the token with one request");
  expect(out).toContain("The upstream refused the token");
});

it("shows the CAPTCHA deadline only while the task waits at that step", () => {
  const waiting = {
    state: "running",
    step: "awaiting_captcha",
    reason: null,
    wait_until: "2026-11-07T10:20:00Z",
  };
  const out = text(html({ setup: waiting }));
  expect(out).toContain("Waiting for a person to pass the CAPTCHA");
  expect(out).toContain("10:20 UTC");
  expect(out).toContain("in 10 min");
  // Past that step, or past that state, the deadline is not offered again.
  expect(
    text(html({ setup: { ...waiting, step: "email_sent" } })),
  ).not.toContain("pass the CAPTCHA");
  expect(
    text(
      html({
        setup: {
          ...waiting,
          state: "needs_human",
          reason: "captcha_challenge",
        },
      }),
    ),
  ).not.toContain("pass the CAPTCHA");
});

it("never words a source as an approval", () => {
  const out = text(html({})).toLowerCase();
  for (const word of ["approved", "authorized", "authorised", "confirmed by"])
    expect(out).not.toContain(word);
});

it("says so when the gateway has no record yet", () => {
  expect(text(html({ created: null, history: [] }))).toContain("No record yet");
});
