import { formatBytes } from "../lib/format";
import { byHour } from "../lib/traffic";
import type { Dashboard } from "../lib/types";
import { Cell, Section, Table, Time } from "./ui";

const kindLabels: Record<string, string> = {
  inference: "推理",
  connect: "CONNECT",
};

function Bytes({ up, down }: { up: number; down: number }) {
  return (
    <span className="text-xs text-muted-foreground">
      ↑{formatBytes(up)} ↓{formatBytes(down)}
    </span>
  );
}

export function Hourly({ data }: { data: Dashboard }) {
  const { columns, hours } = byHour(data.hourly);
  return (
    <Section
      title="流量：最近 24 h 按小时"
      note="每格是请求数和上下行字节；小时按洛杉矶时间显示。"
    >
      <Table
        head={[
          "小时",
          ...columns.map(
            (c) =>
              `${c.alias === "" ? "无账号" : c.alias} ${kindLabels[c.kind] ?? c.kind}`,
          ),
        ]}
        empty="最近 24 h 没有流量"
      >
        {hours.map((h) => (
          <tr key={h.hour} className="border-b border-border last:border-0">
            <Cell>
              <Time iso={h.hour} short />
            </Cell>
            {columns.map((c) => {
              const t = h.cells.get(c.key);
              return (
                <Cell key={c.key} numeric>
                  {t === undefined ? (
                    "·"
                  ) : (
                    <>
                      {t.requests} <Bytes up={t.bytes_up} down={t.bytes_down} />
                    </>
                  )}
                </Cell>
              );
            })}
          </tr>
        ))}
      </Table>
    </Section>
  );
}

export function Results({ data }: { data: Dashboard }) {
  return (
    <Section
      title="流量：最近 24 h 按结果"
      note="status 是上游或出口回的 HTTP 状态码，0 表示没有。"
    >
      <Table
        head={["类型", "result", "status", "次数"]}
        empty="最近 24 h 没有流量"
      >
        {data.results.map((r) => (
          <tr
            key={`${r.kind}\t${r.result}\t${String(r.status)}`}
            className="border-b border-border last:border-0"
          >
            <Cell>{kindLabels[r.kind] ?? r.kind}</Cell>
            <Cell>
              <code className="font-mono">{r.result}</code>
            </Cell>
            <Cell numeric>{r.status}</Cell>
            <Cell numeric>{r.count}</Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}

export function Destinations({ data }: { data: Dashboard }) {
  return (
    <Section
      title="CONNECT 目的地：最近 7 天"
      note="按 host:port、账号和路径聚合，含被拒绝和出口失败的隧道；「直连」是白名单里的目的地，从网关机直接连，不经账号出口。「成功」是 result 为 ok 的次数。隧道结束时才写入，正在打开的不在表里。"
    >
      <Table
        head={["host:port", "账号", "路径", "次数", "成功", "字节", "最近一次"]}
        empty="最近 7 天没有 CONNECT"
      >
        {data.destinations.map((d) => (
          <tr
            key={`${d.host}:${String(d.port)}\t${d.alias}\t${d.route}`}
            className="border-b border-border last:border-0"
          >
            <Cell>
              <code className="font-mono">
                {d.host}:{d.port}
              </code>
            </Cell>
            <Cell>
              {d.alias === "" ? (
                <span className="text-muted-foreground">无账号</span>
              ) : (
                d.alias
              )}
            </Cell>
            <Cell>{routeLabel(d.route)}</Cell>
            <Cell numeric>{d.tunnels}</Cell>
            <Cell numeric>{d.ok}</Cell>
            <Cell numeric>
              <Bytes up={d.bytes_up} down={d.bytes_down} />
            </Cell>
            <Cell>
              <Time iso={d.last} short />
            </Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}

function routeLabel(route: string) {
  switch (route) {
    case "direct":
      return "直连";
    case "account":
      return "账号出口";
    default:
      return <span className="text-muted-foreground">—</span>;
  }
}
