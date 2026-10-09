import { formatBytes, formatDuration } from "../lib/format";
import type { Dashboard } from "../lib/types";
import { Flag, Section, Time } from "./ui";

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex gap-3 py-0.5">
      <dt className="w-36 shrink-0 text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function Commit({ hash }: { hash: string }) {
  if (hash === "") return <span className="text-muted-foreground">不明</span>;
  return (
    <code className="font-mono" title={hash}>
      {hash.slice(0, 12)}
    </code>
  );
}

export function Overview({ data, now }: { data: Dashboard; now: Date }) {
  const g = data.gateway;
  const reasons = data.health.reasons ?? [];
  return (
    <Section
      title="总览"
      note="带「本次启动以来」的几项是网关进程内存里的读数，重启后清零。"
    >
      <dl className="text-sm">
        <Row label="状态">
          {data.health.status === "ok" ? (
            <span className="font-medium text-emerald-700 dark:text-emerald-400">
              ok
            </span>
          ) : (
            <Flag tone="bad">{data.health.status}</Flag>
          )}
          {reasons.length === 0 ? null : (
            <ul className="mt-1 list-disc pl-5">
              {reasons.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
          )}
        </Row>
        <Row label="网关版本">
          {g.version === undefined ? (
            <span className="text-muted-foreground">不明</span>
          ) : (
            <code className="font-mono">v{g.version}</code>
          )}
        </Row>
        <Row label="页面 commit">
          <Commit hash={__DASHBOARD_COMMIT__} />
        </Row>
        <Row label="清单 commit">
          <Commit hash={g.inventory_commit} />
        </Row>
        <Row label="启动时间">
          <Time iso={g.started} /> (已运行{" "}
          {formatDuration(now.getTime() - Date.parse(g.started))})
        </Row>
        <Row label="库大小">
          {formatBytes(data.db_bytes)}，WAL {formatBytes(g.wal_bytes)}
        </Row>
        <Row label="最近 retention">
          {g.retention.at === undefined ? (
            "本次启动以来还没跑完"
          ) : (
            <>
              <Time iso={g.retention.at} />
              ，删除 traffic {g.retention.pruned.traffic}、换绑历史{" "}
              {g.retention.pruned.binding_history}、会话{" "}
              {g.retention.pruned.sessions}
              {g.retention.vacuumed ? "，做了 VACUUM" : ""}
              {g.retention.error === undefined ? null : (
                <Flag tone="bad">，失败：{g.retention.error}</Flag>
              )}
            </>
          )}
        </Row>
        <Row label="最近备份">
          {g.backup === null ? (
            "未配置"
          ) : (
            <>
              成功{" "}
              {g.backup.last_success === undefined ? (
                "本次启动以来没有"
              ) : (
                <Time iso={g.backup.last_success} />
              )}
              ；失败{" "}
              {g.backup.last_failure === undefined ? (
                "本次启动以来没有"
              ) : (
                <Time iso={g.backup.last_failure} />
              )}
            </>
          )}
        </Row>
        <Row label="隧道">
          {g.open_tunnels} / {g.tunnel_limit} 条正在打开
        </Row>
        <Row label="页面数据生成于">
          <Time iso={data.at} /> (UTC {data.at})
        </Row>
      </dl>
    </Section>
  );
}
