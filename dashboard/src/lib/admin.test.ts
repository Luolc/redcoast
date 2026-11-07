import { expect, it } from "vitest";
import { sampleDashboard } from "../../fixtures/sample.ts";
import {
  canCreate,
  canWrite,
  changesOf,
  draftErrors,
  draftOf,
  findManaged,
  managedAccounts,
  patchBody,
  saveEdit,
  validAlias,
  validDevice,
  validIp,
  validPort,
  validUuid,
  writeOffReason,
} from "./admin";

const data = sampleDashboard(new Date("2026-10-07T12:00:00Z"));
const options = data.proxy_ref_options ?? [];

it("lists the served accounts, then those not served", () => {
  expect(managedAccounts(data).map((m) => m.alias)).toEqual([
    "sample-ab",
    "sample-ac",
    "sample-ad",
    "sample-af",
  ]);
  expect(findManaged(data, "sample-af")?.account).toBeUndefined();
  expect(findManaged(data, "sample-ab")?.config?.status).toBe("active");
});

it("says whether the page may write, and why not", () => {
  expect(canWrite(data)).toBe(true);
  expect(canCreate(data)).toBe(true);
  expect(writeOffReason(data)).toBeNull();
  const off = {
    ...data,
    write: { enabled: false, reason: "source not allowed", can_create: true },
  };
  expect(canWrite(off)).toBe(false);
  expect(canCreate(off)).toBe(false);
  expect(writeOffReason(off)).toBe("source not allowed");
  const noTemplate = { ...data, write: { enabled: true, can_create: false } };
  expect(canWrite(noTemplate)).toBe(true);
  expect(canCreate(noTemplate)).toBe(false);
  // A gateway that does not edit accounts has nothing to explain.
  const codex = { ...data, accounts_editable: false };
  expect(canWrite(codex)).toBe(false);
  expect(writeOffReason(codex)).toBeNull();
});

it("checks the shapes the gateway checks", () => {
  expect(validAlias("sample-ab")).toBe(true);
  expect(validAlias("Sample")).toBe(false);
  expect(validAlias("")).toBe(false);
  expect(validIp("192.0.2.10")).toBe(true);
  expect(validIp("192.0.2.256")).toBe(false);
  expect(validIp("192.0.2")).toBe(false);
  expect(validPort("65535")).toBe(true);
  expect(validPort("0")).toBe(false);
  expect(validPort("65536")).toBe(false);
  expect(validUuid("11111111-1111-4222-8333-111111111111")).toBe(true);
  expect(validUuid("11111111-1111-4222-8333-11111111111G")).toBe(false);
  expect(validDevice("ab".repeat(32))).toBe(true);
  expect(validDevice("ab".repeat(31))).toBe(false);
});

it("makes the PATCH of only what the draft changed", () => {
  const m = findManaged(data, "sample-ab");
  if (m === undefined) throw new Error("no sample-ab");
  const was = draftOf(m, options);
  expect(changesOf(was, was)).toEqual([]);
  const changes = changesOf(was, {
    ...was,
    status: "paused",
    port: "9000",
    login: "1",
    plan: "max-5x",
  });
  expect(changes.map((c) => c.label)).toEqual([
    "Status",
    "Plan",
    "Proxy port",
    "Proxy login",
  ]);
  expect(patchBody(7, changes, options)).toEqual({
    rev: 7,
    status: "paused",
    plan: "max-5x",
    proxy: {
      port: 9000,
      username_ref: "op://sample-vault/sample-proxy-b/username",
      password_ref: "op://sample-vault/sample-proxy-b/password",
    },
  });
});

it("sends no plan when the field was cleared", () => {
  const m = findManaged(data, "sample-ab");
  if (m === undefined) throw new Error("no sample-ab");
  const was = draftOf(m, options);
  const changes = changesOf(was, { ...was, plan: "" });
  expect(patchBody(1, changes, options)).toEqual({ rev: 1 });
});

it("holds the draft to the gateway's shapes", () => {
  const m = findManaged(data, "sample-af");
  if (m === undefined) throw new Error("no sample-af");
  const was = draftOf(m, options);
  expect(draftErrors(was)).toEqual({});
  expect(
    Object.keys(
      draftErrors({ ...was, host: " ", port: "x", ip: "1.2", uuid: "u" }),
    ),
  ).toEqual(["host", "port", "ip", "uuid"]);
});

it("checks no proxy field of a direct account", () => {
  const m = findManaged(data, "sample-ab");
  if (m?.config === undefined) throw new Error("no sample-ab");
  const direct = {
    ...m,
    config: {
      ...m.config,
      proxy: {
        provider: "",
        type: "",
        direct: true,
        host: "",
        port: 0,
        expected_egress_ip: "",
        username_ref: "",
        password_ref: "",
      },
    },
  };
  const draft = draftOf(direct, options);
  expect(Object.keys(draftErrors(draft))).toEqual(["host", "port", "ip"]);
  expect(draftErrors(draft, true)).toEqual({});
  expect(draftErrors({ ...draft, uuid: "u" }, true)).toEqual({
    uuid: "Not a UUID",
  });
});

it("saves against the revision the form was opened on", async () => {
  const m = findManaged(data, "sample-ab");
  if (m === undefined) throw new Error("no sample-ab");
  const was = draftOf(m, options);
  let sent: { url: string; init: RequestInit } | undefined;
  const post: typeof fetch = (url, init) => {
    sent = { url: typeof url === "string" ? url : "", init: init ?? {} };
    return Promise.resolve(new Response("{}", { status: 200 }));
  };
  // The account has moved on since: the edit still names revision 4.
  await saveEdit(
    "sample-ab",
    { rev: 4, was },
    { ...was, port: "9000" },
    options,
    post,
  );
  expect(sent?.url).toBe("./accounts/sample-ab");
  expect(sent?.init.method).toBe("PATCH");
  expect(sent?.init.body).toBe('{"rev":4,"proxy":{"port":9000}}');
});

it("edits the receiver email and the proxy's provider and type", () => {
  const m = findManaged(data, "sample-ab");
  if (m === undefined) throw new Error("no sample-ab");
  const was = draftOf(m, options);
  expect(was.receiver).toBe(m.config?.receiver_email);
  expect(was.provider).toBe(m.config?.proxy.provider);
  const changes = changesOf(was, {
    ...was,
    receiver: "other@example.test",
    provider: "other-provider",
    kind: "",
  });
  expect(patchBody(2, changes, options)).toEqual({
    rev: 2,
    receiver_email: "other@example.test",
    proxy: { provider: "other-provider", type: "" },
  });
  expect(draftErrors({ ...was, receiver: "" })).toEqual({});
  expect(
    Object.keys(
      draftErrors({
        ...was,
        receiver: "Name <a@example.test>",
        provider: "a b",
        kind: "a b",
      }),
    ),
  ).toEqual(["receiver", "provider", "kind"]);
});
