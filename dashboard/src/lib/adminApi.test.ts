import { expect, it } from "vitest";
import { requestSetup } from "./admin";
import { accountPath, isConflict, writeAccount } from "./adminApi";

function answer(status: number, body: string): typeof fetch {
  return () => Promise.resolve(new Response(body, { status }));
}

it("sends the action header and a JSON body with the method", async () => {
  let seen: { url: string; init: RequestInit } | undefined;
  const post: typeof fetch = (url, init) => {
    seen = { url: typeof url === "string" ? url : "", init: init ?? {} };
    return Promise.resolve(new Response("{}", { status: 200 }));
  };
  const out = await writeAccount(
    "account-update",
    "PATCH",
    accountPath("sample-ab"),
    { rev: 4 },
    post,
  );
  expect(out).toEqual({ ok: true, verify: undefined });
  expect(seen?.url).toBe("./accounts/sample-ab");
  expect(seen?.init.method).toBe("PATCH");
  expect(seen?.init.headers).toEqual({
    "X-Redcoast-Action": "account-update",
    "Content-Type": "application/json",
  });
  expect(seen?.init.body).toBe('{"rev":4}');
});

it("reads the checks of a verify", async () => {
  const body = JSON.stringify({
    verify: {
      token_ok: { result: "pass" },
      egress_ok: { result: "fail", reason: "exit reads another address" },
      probe_ok: { result: "skipped", reason: "an earlier check failed" },
    },
  });
  const out = await writeAccount(
    "account-verify",
    "POST",
    "./x",
    {},
    answer(200, body),
  );
  expect(out.ok && out.verify?.egress_ok.reason).toBe(
    "exit reads another address",
  );
});

it("names the revision when an edit meets a newer one", async () => {
  const out = await writeAccount(
    "account-update",
    "PATCH",
    "./x",
    {},
    answer(
      409,
      '{"error":"the account changed since it was read","field":"rev"}',
    ),
  );
  expect(out.ok).toBe(false);
  if (out.ok) return;
  expect(isConflict(out.refusal)).toBe(true);
  expect(out.refusal.text).toBe("the account changed since it was read");
});

it("names the check an activation failed", async () => {
  const out = await writeAccount(
    "account-activate",
    "POST",
    "./x",
    {},
    answer(409, '{"error":"egress_ok did not pass: x","check":"egress_ok"}'),
  );
  expect(out.ok).toBe(false);
  if (out.ok) return;
  expect(out.refusal).toMatchObject({ status: 409, check: "egress_ok" });
  // A 409 that is not about the revision is no conflict to refresh away.
  expect(isConflict(out.refusal)).toBe(false);
});

it("reports an answer that is not the gateway's, and no answer", async () => {
  const html = await writeAccount(
    "account-create",
    "POST",
    "./x",
    {},
    answer(502, "<html>"),
  );
  expect(html).toEqual({
    ok: false,
    refusal: { status: 502, text: "HTTP 502" },
  });
  const down = await writeAccount("account-create", "POST", "./x", {}, () =>
    Promise.reject(new Error("offline")),
  );
  expect(!down.ok && down.refusal.status).toBe(0);
});

it("asks for a setup with the setup action, the action and the revision", async () => {
  let seen: { url: string; init: RequestInit } | undefined;
  const post: typeof fetch = (url, init) => {
    seen = { url: typeof url === "string" ? url : "", init: init ?? {} };
    return Promise.resolve(new Response("{}", { status: 202 }));
  };
  const out = await requestSetup("sample-ab", 4, post);
  expect(out.ok).toBe(true);
  expect(seen?.url).toBe("./accounts/sample-ab/setup");
  expect(seen?.init.method).toBe("POST");
  expect(seen?.init.headers).toEqual({
    "X-Redcoast-Action": "account-setup",
    "Content-Type": "application/json",
  });
  expect(seen?.init.body).toBe('{"action":"login_and_token","rev":4}');
});

it("reads the code of a refused setup", async () => {
  const out = await requestSetup(
    "sample-ab",
    4,
    answer(409, '{"error":"direct_account"}'),
  );
  expect(out).toEqual({
    ok: false,
    refusal: {
      status: 409,
      text: "direct_account",
      field: undefined,
      check: undefined,
    },
  });
});
