import { Accounts } from "./components/Accounts";
import { Machines, Sessions } from "./components/Clients";
import { Days, History } from "./components/Days";
import { Overview } from "./components/Overview";
import { Destinations, Hourly, Results } from "./components/Traffic";
import { Flag } from "./components/ui";
import { formatAge } from "./lib/format";
import { useData, useNow } from "./lib/useData";

const REFRESH_MS = 30_000;
// Two missed refreshes: the page is showing old data.
const STALE_MS = 2 * REFRESH_MS + 5_000;

export function App() {
  const { data, fetchError } = useData("./dashboard.json", REFRESH_MS);
  const now = useNow(1_000);
  return (
    <main className="mx-auto max-w-7xl space-y-4 p-4">
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h1 className="text-xl font-semibold">Claude 网关</h1>
        <p className="text-sm text-muted-foreground">
          只读；每 30 秒刷新。时间按洛杉矶显示，鼠标悬停看 UTC。
        </p>
      </header>
      {fetchError === null ? null : (
        <div
          role="alert"
          className="rounded-md border border-red-300 p-3 text-sm"
        >
          <Flag tone="bad">读取失败：{fetchError}</Flag>
          {data === null
            ? null
            : `。下面是 ${formatAge(data.at, now)} 的数据。`}
        </div>
      )}
      {data !== null &&
      fetchError === null &&
      now.getTime() - Date.parse(data.at) > STALE_MS ? (
        <div
          role="alert"
          className="rounded-md border border-amber-300 p-3 text-sm"
        >
          <Flag tone="warn">
            数据生成于 {formatAge(data.at, now)}，比刷新间隔旧。
          </Flag>
        </div>
      ) : null}
      {data === null ? (
        fetchError === null ? (
          <p className="text-sm text-muted-foreground">读取中…</p>
        ) : null
      ) : (
        <>
          <Overview data={data} now={now} />
          <Accounts data={data} now={now} />
          <Days data={data} />
          <Destinations data={data} />
          <Results data={data} />
          <Hourly data={data} />
          <Machines data={data} now={now} />
          <Sessions data={data} now={now} />
          <History data={data} />
        </>
      )}
    </main>
  );
}
