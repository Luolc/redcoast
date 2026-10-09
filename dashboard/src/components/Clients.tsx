import { formatAge, formatDuration } from "../lib/format";
import type { Dashboard } from "../lib/types";
import { Cell, Flag, Section, Table, Time } from "./ui";

export function Machines({ data, now }: { data: Dashboard; now: Date }) {
  return (
    <Section
      title="客户机"
      note="最近活动取该客户机名下会话最近一次请求的时间。"
    >
      <Table
        head={["名字", "会话 (当前 / 上限)", "最近活动", "登记时间"]}
        empty="没有登记的客户机"
      >
        {data.machines.map((m) => (
          <tr key={m.name} className="border-b border-border last:border-0">
            <Cell>
              {m.name}
              {m.revoked_at === undefined ? null : (
                <div className="text-xs">
                  <Flag tone="warn">已吊销</Flag>{" "}
                  <Time iso={m.revoked_at} short />
                </div>
              )}
            </Cell>
            <Cell numeric>
              {m.live_sessions} / {m.max_sessions}
            </Cell>
            <Cell>
              {m.last_seen === undefined
                ? "没有请求"
                : formatAge(m.last_seen, now)}
            </Cell>
            <Cell>
              <Time iso={m.created_at} />
            </Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}

export function Sessions({ data, now }: { data: Dashboard; now: Date }) {
  const oldest = data.sessions[0]; // the Go side sorts by creation
  return (
    <Section
      title="会话"
      note={
        <>
          活会话 {data.sessions.length} 个
          {oldest === undefined
            ? null
            : `，最老的已存在 ${formatDuration(now.getTime() - Date.parse(oldest.created_at))}`}
          。超过 24 h 没有请求的 {data.idle_sessions}{" "}
          个，只作参考，不算泄漏。会话 ID 只显示前 8 个字符。
        </>
      }
    >
      <Table
        head={["会话", "客户机", "账号", "年龄", "空闲"]}
        empty="没有活会话"
      >
        {data.sessions.map((s) => (
          <tr key={s.id} className="border-b border-border last:border-0">
            <Cell>
              <code className="font-mono">{s.id}</code>
            </Cell>
            <Cell>{s.client_machine}</Cell>
            <Cell>
              {s.alias === "" ? (
                <span className="text-muted-foreground">未绑定</span>
              ) : (
                s.alias
              )}
            </Cell>
            <Cell numeric>
              {formatDuration(now.getTime() - Date.parse(s.created_at))}
            </Cell>
            <Cell numeric>
              {formatDuration(now.getTime() - Date.parse(s.last_seen))}
            </Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}
