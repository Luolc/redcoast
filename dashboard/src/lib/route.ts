// The page's place lives in the address's fragment, `#/<view>[/<arg>]`, so
// every view opens directly and another provider's dashboard opens on the
// same view when the fragment is carried over.
export type View =
  | "overview"
  | "clients"
  | "client"
  | "sessions"
  | "accounts"
  | "exits"
  | "monitor";

export interface Route {
  view: View;
  // A client machine for sessions; `<machine>/<program>` for client; a
  // section for monitor.
  arg?: string | undefined;
}

const VIEWS = new Set<string>([
  "overview",
  "clients",
  "client",
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
  client: "Client",
  sessions: "Sessions",
  accounts: "Accounts",
  exits: "Exits",
  monitor: "Monitor",
};

// A machine's name has no slash, so the first one ends it.
export function clientHref(machine: string, client: string): string {
  return routeHref({ view: "client", arg: `${machine}/${client}` });
}

/** The machine and program of a client route's arg; null without a slash. */
export function parseClient(
  arg: string | undefined,
): { machine: string; client: string } | null {
  const i = arg?.indexOf("/") ?? -1;
  if (arg === undefined || i < 0) return null;
  return { machine: arg.slice(0, i), client: arg.slice(i + 1) };
}
