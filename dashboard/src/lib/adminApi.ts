// The account writes. Each carries the action header the quota refresh
// already uses and a JSON body of settings only, never a credential's value;
// the browser adds the page's Origin, which the gateway requires.
import { isString, object, optional } from "./guard";

const ACTION_HEADER = "X-Redcoast-Action";

export type Action =
  "account-create" | "account-update" | "account-verify" | "account-activate";

/** Why the gateway refused a write, or that it could not be reached. */
export interface Refusal {
  status: number; // 0 when there was no answer
  text: string;
  field?: string | undefined; // the field of the request it concerns
  check?: string | undefined; // the check or condition of an activation that failed
}

const isRefusal = object<{
  error: string;
  field: string | undefined;
  check: string | undefined;
}>({
  error: isString,
  field: optional(isString),
  check: optional(isString),
});

/** A 409 on the revision: the account changed since the page read it. */
export function isConflict(r: Refusal): boolean {
  return r.status === 409 && r.field === "rev";
}

/** What a verify or an activation found, by check. */
interface CheckReport {
  result: string; // pass, fail or skipped
  reason?: string | undefined;
}
export type VerifyReport = Record<
  "token_ok" | "egress_ok" | "probe_ok",
  CheckReport
>;

const isCheck = object<CheckReport>({
  result: isString,
  reason: optional(isString),
});
const isReport = object<VerifyReport>({
  token_ok: isCheck,
  egress_ok: isCheck,
  probe_ok: isCheck,
});

export type Outcome =
  | { ok: true; verify?: VerifyReport | undefined }
  | { ok: false; refusal: Refusal };

function parse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/** One write; resolves to the gateway's answer or its refusal, never throws. */
export async function writeAccount(
  action: Action,
  method: "POST" | "PATCH",
  path: string,
  body: unknown,
  post: typeof fetch = fetch,
): Promise<Outcome> {
  try {
    const response = await post(path, {
      method,
      headers: { [ACTION_HEADER]: action, "Content-Type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
    });
    const answer = parse(await response.text());
    if (response.ok) {
      const verify =
        typeof answer === "object" && answer !== null && "verify" in answer
          ? answer.verify
          : undefined;
      return { ok: true, verify: isReport(verify) ? verify : undefined };
    }
    if (isRefusal(answer))
      return {
        ok: false,
        refusal: {
          status: response.status,
          text: answer.error,
          field: answer.field,
          check: answer.check,
        },
      };
    return {
      ok: false,
      refusal: {
        status: response.status,
        text: `HTTP ${String(response.status)}`,
      },
    };
  } catch (e) {
    return { ok: false, refusal: { status: 0, text: String(e) } };
  }
}

export function accountPath(alias: string, tail = ""): string {
  return `./accounts/${encodeURIComponent(alias)}${tail}`;
}
