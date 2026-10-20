import { expect, it } from "vitest";
import {
  clientHref,
  parseClient,
  parseRoute,
  providerHref,
  routeHref,
} from "./route";

it("reads a view and its argument from the fragment, and back", () => {
  expect(parseRoute("")).toEqual({ view: "overview" });
  expect(parseRoute("#/monitor")).toEqual({ view: "monitor" });
  expect(parseRoute("#/sessions/machine%20a")).toEqual({
    view: "sessions",
    arg: "machine a",
  });
  expect(routeHref({ view: "sessions", arg: "machine a" })).toBe(
    "#/sessions/machine%20a",
  );
});

it("opens an unknown or broken fragment on the overview", () => {
  expect(parseRoute("#/nowhere")).toEqual({ view: "overview" });
  expect(parseRoute("#/sessions/%E0%A4%A")).toEqual({ view: "sessions" });
});

it("links another provider's dashboard on the same view", () => {
  expect(providerHref("http://127.0.0.1:7807/", "#/sessions/machine-a")).toBe(
    "http://127.0.0.1:7807/#/sessions/machine-a",
  );
  expect(providerHref("https://gw.example.test/codex/#/old", "#/monitor")).toBe(
    "https://gw.example.test/codex/#/monitor",
  );
});

it("links nothing but an absolute http or https URL", () => {
  expect(providerHref("javascript:alert(1)", "#/")).toBeNull();
  expect(providerHref(":7807", "#/")).toBeNull();
  expect(providerHref("ftp://gw.example.test/", "#/")).toBeNull();
});

it("names a client by machine and program, the program possibly unknown", () => {
  for (const client of ["claude-code", ""]) {
    const route = parseRoute(clientHref("machine-a", client));
    expect(route.view).toBe("client");
    expect(parseClient(route.arg)).toEqual({ machine: "machine-a", client });
  }
  expect(parseClient("machine-a")).toBeNull();
  expect(parseClient(undefined)).toBeNull();
});
