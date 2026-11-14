import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import { sampleDashboard } from "../../fixtures/sample.ts";
import { Page } from "../App";
import type { Route } from "../lib/route";
import type { AccountConfig, Dashboard } from "../lib/types";
import { Confirm, RefusalCallout } from "./AdminUi";

const now = new Date("2026-10-07T12:20:00Z");
const data = sampleDashboard(now);
const noop = () => undefined;

function html(route: Route, d: Dashboard = data): string {
  return renderToStaticMarkup(
    <Page data={d} now={now} route={route} reload={noop} />,
  );
}

function text(markup: string): string {
  return markup.replace(/<[^>]*>/g, "").replaceAll("&amp;", "&");
}

const off: Dashboard = {
  ...data,
  write: {
    enabled: false,
    reason: "Writing is off: this source is not allowed.",
    can_create: true,
  },
};

it("lists every account, the unserved one too, with the way to add one", () => {
  const out = html({ view: "manage" });
  for (const t of ["sample-ab", "sample-af", "Testing", "Not serving"])
    expect(text(out)).toContain(t);
  expect(out).toContain('href="#/add/1"');
});

it("hides editing and adding, and says why, when writes are off", () => {
  const list = html({ view: "manage" }, off);
  expect(list).not.toContain('href="#/add/1"');
  expect(text(list)).toContain("Writing is off: this source is not allowed.");
  const detail = html({ view: "account", arg: "sample-ab" }, off);
  expect(detail).not.toContain("Edit settings");
  expect(text(detail)).toContain("Writing is off");
  // The edit form does not open from its address either.
  const form = html({ view: "account", arg: "sample-ab/edit" }, off);
  expect(form).not.toContain("Save");
  expect(text(form)).toContain("Credentials");
  const add = html({ view: "add", arg: "1" }, off);
  // Nor does adding open from its address when no token reference can be named.
  const noTemplate = html(
    { view: "add", arg: "1" },
    { ...data, write: { enabled: true, can_create: false } },
  );
  expect(noTemplate).not.toContain("Save as testing");
  expect(text(noTemplate)).toContain("Adding accounts is off here.");
  expect(text(add)).toContain("Adding accounts is off here.");
  expect(add).not.toContain("Save as testing");
});

it("hides adding when the gateway cannot name a token reference", () => {
  const d = { ...data, write: { enabled: true, can_create: false } };
  expect(html({ view: "manage" }, d)).not.toContain('href="#/add/1"');
  expect(html({ view: "manage" }, d)).toContain("sample-ab");
  expect(html({ view: "account", arg: "sample-ab" }, d)).toContain(
    "Edit settings",
  );
});

it("shows a gateway that does not edit accounts as a plain list", () => {
  const codex = {
    ...data,
    accounts_editable: false,
    unserved_accounts: undefined,
    accounts: data.accounts.map((a) => ({ ...a, config: undefined })),
  };
  const list = html({ view: "manage" }, codex);
  expect(text(list)).toContain("sample-ab");
  expect(list).not.toContain("Add account");
  const detail = html({ view: "account", arg: "sample-ab" }, codex);
  expect(detail).not.toContain("Edit settings");
  expect(text(detail)).toContain("ab@example.test");
});

it("shows an account's settings, references and use", () => {
  const out = text(html({ view: "account", arg: "sample-ab" }));
  for (const t of [
    "op://sample-vault/claude-oauth-token-sample-ab/credential",
    "Revision",
    "Use by machine",
    "machine-a",
    "Bound sessions",
    "Lasts until reset",
  ])
    expect(out).toContain(t);
});

it("opens the edit form with the account's values and a save that waits", () => {
  const out = html({ view: "account", arg: "sample-ab/edit" });
  expect(out).toContain("Save</button>");
  expect(out).toContain("disabled");
  expect(out).toContain('value="proxy-1.example.test"');
  expect(out).toContain('readOnly=""');
});

it("offers kyc among the statuses an edit can set", () => {
  const out = html({ view: "account", arg: "sample-ab/edit" });
  expect(out).toContain('<option value="kyc">KYC</option>');
});

it("walks the steps of adding an account", () => {
  expect(text(html({ view: "add", arg: "1" }))).toContain(
    "Fill in the account",
  );
  expect(text(html({ view: "add", arg: "2/sample-af" }))).toContain(
    "Saved, not serving",
  );
  const verify = text(html({ view: "add", arg: "3/sample-af" }));
  expect(verify).toContain("Not verified");
  expect(verify).toContain(
    "op://sample-vault/claude-oauth-token-sample-af/credential",
  );
  expect(text(html({ view: "add", arg: "4/sample-af" }))).toContain(
    "Account UUID",
  );
  expect(text(html({ view: "add", arg: "5/sample-af" }))).toContain("Activate");
  // An account the gateway does not list yet is waited for.
  expect(text(html({ view: "add", arg: "2/sample-zz" }))).toContain(
    "Waiting for the gateway to list sample-zz",
  );
});

it("tells a revision conflict to refresh and names what an activation failed", () => {
  const conflict = text(
    renderToStaticMarkup(
      <RefusalCallout
        title="Not saved"
        refusal={{ status: 409, text: "the account changed", field: "rev" }}
        onRefresh={noop}
      />,
    ),
  );
  expect(conflict).toContain("Not saved");
  expect(conflict).toContain("Refresh");
  const failed = text(
    renderToStaticMarkup(
      <RefusalCallout
        title="Not activated"
        refusal={{
          status: 409,
          text: "egress_ok did not pass: exit reads another address",
          check: "egress_ok",
        }}
        onRefresh={noop}
      />,
    ),
  );
  expect(failed).toContain("Not activated");
  expect(failed).toContain("Concerns: Exit reads the expected IP");
  expect(failed).not.toContain("Refresh");
});

it("has no field that takes a credential", () => {
  // The forms take references and settings; a credential never reaches the page.
  for (const route of [
    { view: "add", arg: "1" },
    { view: "add", arg: "3/sample-af" },
    { view: "add", arg: "4/sample-af" },
    { view: "account", arg: "sample-ab" },
    { view: "account", arg: "sample-ab/edit" },
  ] satisfies Route[]) {
    const out = html(route);
    expect(out).not.toContain('type="password"');
    // The only field named for a token shows its reference and cannot be typed in.
    const tokenInputs = (out.match(/<input[^>]*>/g) ?? []).filter((input) =>
      input.includes("claude-oauth-token"),
    );
    expect(tokenInputs.every((input) => input.includes("readOnly"))).toBe(true);
  }
  expect(html({ view: "account", arg: "sample-ab/edit" })).toContain(
    "claude-oauth-token-sample-ab",
  );
});

it("offers only the first click of a confirmed act", () => {
  const out = renderToStaticMarkup(
    <Confirm
      label="Activate"
      yes="Yes, activate"
      question="Sure?"
      onYes={noop}
    />,
  );
  expect(out).toContain("Activate");
  expect(out).not.toContain("Yes, activate");
  expect(out).not.toContain("Sure?");
});

function withConfig(
  alias: string,
  patch: (c: AccountConfig) => Partial<AccountConfig>,
): Dashboard {
  const edit = (c: AccountConfig) =>
    c.alias === alias ? { ...c, ...patch(c) } : c;
  return {
    ...data,
    accounts: data.accounts.map((a) =>
      a.config === undefined ? a : { ...a, config: edit(a.config) },
    ),
    unserved_accounts: data.unserved_accounts?.map(edit),
  };
}

it("offers the setup button for a testing or active account with a proxy exit", () => {
  const step3 = html({ view: "add", arg: "3/sample-af" });
  expect(step3).toContain("Start setup");
  expect(step3).not.toContain("Yes, start setup");
  expect(html({ view: "account", arg: "sample-ab" })).toContain("Start setup");
});

it("says why there is no setup button instead", () => {
  const cases: [Dashboard, string][] = [
    [
      { ...data, gateway: { ...data.gateway, setup: null } },
      "Setup is off on this gateway.",
    ],
    [
      withConfig("sample-ab", () => ({ status: "paused" })),
      "only for accounts that are testing or active; this one is paused",
    ],
    [
      withConfig("sample-ab", (c) => ({
        proxy: { ...c.proxy, direct: true },
      })),
      "only offered for an account with a proxy exit",
    ],
    [
      withConfig("sample-ab", (c) => ({
        access: "direct",
        proxy: { ...c.proxy, host: "", port: 0, expected_egress_ip: "" },
      })),
      "only offered for an account with a proxy exit",
    ],
  ];
  for (const [d, why] of cases) {
    const out = html({ view: "account", arg: "sample-ab" }, d);
    expect(out).not.toContain("Start setup");
    expect(text(out)).toContain(why);
  }
  expect(html({ view: "account", arg: "sample-ab" }, off)).not.toContain(
    "Start setup",
  );
});

it("shows the state of the setup, and what a person must do when it needs one", () => {
  const out = text(html({ view: "add", arg: "3/sample-af" }));
  for (const t of [
    "Needs a person",
    "On the email page",
    "The login page asked for a CAPTCHA",
    "put its token in the secret manager",
    "press Verify",
  ])
    expect(out).toContain(t);
  const busy = withConfig("sample-af", (c) => ({
    setup: c.setup && { ...c.setup, state: "running" },
  }));
  const running = html({ view: "add", arg: "3/sample-af" }, busy);
  expect(running).not.toContain("Start setup");
  expect(text(running)).toContain("A setup is under way");
});

it("shows when the token was set and falls due, or that it is not recorded", () => {
  const set = text(html({ view: "account", arg: "sample-ab" }));
  expect(set).toContain("Token set");
  expect(set).toContain("Token expires");
  expect(set).not.toContain("not recorded");
  const unset = text(html({ view: "account", arg: "sample-af" }));
  expect(unset).toContain("not recorded");
  expect(unset).toContain("The token item has no expiry field");
  const unreadable = withConfig("sample-ab", (c) => ({
    setup: c.setup && {
      ...c.setup,
      token_expiry: { state: "unreadable", expires_at: null, read_at: null },
    },
  }));
  expect(
    text(html({ view: "account", arg: "sample-ab" }, unreadable)),
  ).toContain("could not be read");
});
