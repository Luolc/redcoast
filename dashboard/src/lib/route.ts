// The page's place lives in the address's fragment, `#/<view>[/<arg>]`, so
// every view opens directly and another provider's dashboard opens on the
// same view when the fragment is carried over.
export type View =
  "overview" | "clients" | "sessions" | "accounts" | "exits" | "monitor";

export interface Route {
  view: View;
  // A client machine for clients and sessions; a section for monitor.
  arg?: string | undefined;
}

const VIEWS = new Set<string>([
  "overview",
  "clients",
  "sessions",
  "accounts",
  "exits",
  "monitor",
]);

function isView(v: string): v is View {
  return VIEWS.has(v);
}

/** The route of a fragment; overview for one the page does not know. */
export function parseRoute(hash: string): Route {
  const [view = "", ...rest] = hash.replace(/^#\/?/, "").split("/");
  if (!isView(view)) return { view: "overview" };
  if (rest.length === 0 || rest.join("/") === "") return { view };
  try {
    return { view, arg: decodeURIComponent(rest.join("/")) };
  } catch {
    return { view };
  }
}

export function routeHref(r: Route): string {
  return r.arg === undefined
    ? `#/${r.view}`
    : `#/${r.view}/${encodeURIComponent(r.arg)}`;
}

/**
 * The link to another provider's dashboard at url, opened on the same
 * fragment; null unless url is an absolute http or https URL, so that no
 * other scheme reaches an href.
 */
export function providerHref(url: string, hash: string): string | null {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return null;
  u.hash = hash;
  return u.toString();
}

export const VIEW_NAMES: Record<View, string> = {
  overview: "Overview",
  clients: "Clients",
  sessions: "Sessions",
  accounts: "Accounts",
  exits: "Exits",
  monitor: "Monitor",
};
