import type { ReactNode } from "react";
import { Accounts } from "./components/Accounts";
import { Clients } from "./components/Clients";
import { Exits } from "./components/Exits";
import { Monitor } from "./components/Monitor";
import { Overview } from "./components/Overview";
import { Sessions } from "./components/Sessions";
import { Sidebar, Topbar } from "./components/Shell";
import { Status } from "./components/ui";
import { formatAgo } from "./lib/format";
import { routeHref, VIEW_NAMES, type Route } from "./lib/route";
import type { Dashboard } from "./lib/types";
import { useData, useNow, useRoute, useTheme } from "./lib/useData";
import { providerName } from "./lib/words";

const REFRESH_MS = 30_000;
// Two missed refreshes: the page is showing old data.
const STALE_MS = 2 * REFRESH_MS + 5_000;

/** The page of route over data. */
export function Page({
  data,
  now,
  route,
}: {
  data: Dashboard;
  now: Date;
  route: Route;
}) {
  switch (route.view) {
    case "overview":
      return <Overview data={data} now={now} />;
    case "clients":
      return <Clients data={data} now={now} />;
    case "sessions":
      return <Sessions data={data} now={now} machine={route.arg} />;
    case "accounts":
      return <Accounts data={data} now={now} />;
    case "exits":
      return <Exits data={data} now={now} />;
    case "monitor":
      return <Monitor data={data} now={now} focus={route.arg} />;
  }
}

function crumbs(data: Dashboard | null, route: Route): ReactNode[] {
  const trail: ReactNode[] = [];
  if (data !== null)
    trail.push(
      <a href={routeHref({ view: "overview" })}>
        {providerName(data.gateway.provider)}
      </a>,
    );
  if (route.view === "sessions" && route.arg !== undefined) {
    trail.push(<a href={routeHref({ view: "sessions" })}>Sessions</a>);
    trail.push(route.arg);
  } else trail.push(VIEW_NAMES[route.view]);
  return trail;
}

export function App() {
  const { data, fetchError, reload } = useData("./dashboard.json", REFRESH_MS);
  const now = useNow(1_000);
  const route = useRoute();
  const [theme, setTheme] = useTheme();
  return (
    <div className="shell">
      <Sidebar data={data} route={route} />
      <div className="main">
        <Topbar
          data={data}
          crumbs={crumbs(data, route)}
          now={now}
          theme={theme}
          setTheme={setTheme}
          reload={reload}
        />
        <main className="content">
          {fetchError === null ? null : (
            <div role="alert" className="alert">
              <Status
                state={{ tone: "bad", word: `Could not read: ${fetchError}` }}
              />
              {data === null
                ? null
                : ` Showing the data of ${formatAgo(data.at, now)}.`}
            </div>
          )}
          {data !== null &&
          fetchError === null &&
          now.getTime() - Date.parse(data.at) > STALE_MS ? (
            <div role="alert" className="alert">
              <Status
                state={{
                  tone: "warn",
                  word: `The data is from ${formatAgo(data.at, now)}, older than the refresh interval.`,
                }}
              />
            </div>
          ) : null}
          {data === null ? (
            fetchError === null ? (
              <p className="muted">Loading…</p>
            ) : null
          ) : (
            <Page data={data} now={now} route={route} />
          )}
        </main>
      </div>
    </div>
  );
}
