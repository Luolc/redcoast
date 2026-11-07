import { expect, it } from "vitest";
import {
  resultWording,
  setupReasonWording,
  setupRefusalWording,
  setupStateWording,
  setupStepWording,
  tokenExpiryWording,
} from "./words";

it("words a refusal by what was refused", () => {
  expect(resultWording("connect", "refused", 0).text).toBe(
    "CONNECT target refused",
  );
  expect(resultWording("inference", "refused", 426).text).toBe(
    "Refused before forwarding",
  );
});

it("words an ok result failed by the upstream's status", () => {
  expect(resultWording("inference", "ok", 529).text).toBe(
    "Upstream server error",
  );
  expect(resultWording("inference", "ok", 429).text).toBe(
    "Upstream rate limit",
  );
  expect(resultWording("inference", "ok", 400).text).toBe(
    "Upstream refused the request",
  );
});

it("words the setup codes, and leaves an unknown code as it is", () => {
  expect(setupStateWording("needs_human").word).toBe("Needs a person");
  expect(setupStepWording("handover")).toBe("Handing the token to the gateway");
  expect(setupStepWording("new_step")).toBe("new_step");
  expect(setupReasonWording("direct_account")).toContain("direct exit");
  expect(setupRefusalWording("account_status")).toContain("testing or active");
  expect(setupRefusalWording("account_retired")).toBe("account_retired");
});

it("words what was read of a token's expiry", () => {
  expect(tokenExpiryWording("no_field")).toContain("no expiry field");
  expect(tokenExpiryWording("unreadable")).toContain("could not be read");
  expect(tokenExpiryWording("other")).toBe("other");
});
