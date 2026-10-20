import { expect, it } from "vitest";
import { resultWording } from "./words";

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
