import type { Dashboard } from "../lib/types";
import { Cell, Section, Table, Time } from "./ui";

export function Days({ data }: { data: Dashboard }) {
  return (
    <Section
      title="每日指标：最近 7 天"
      note={
        <ul className="list-disc space-y-0.5 pl-5">
          <li>按 UTC 日分，日期是 UTC 的。</li>
          <li>
            limited：traffic 里 result 为 limited 的行数，推理与 CONNECT 分开。
          </li>
          <li>
            隧道峰值：真正打开过的隧道 (result 为
            ok、idle_timeout、max_duration) 在 [开始, 开始 + 时长)
            上的最大重叠。隧道结束时才写入，当天还开着的不算。
          </li>
          <li>会话峰值：会话从创建到结束 (未结束的算到现在) 的最大重叠。</li>
          <li>
            泄漏 (确认)：当天被网关按 7
            天空闲期限结束、客户端没有自己结束的会话，按结束那天计，所以最长滞后
            7 天才看得到。
          </li>
        </ul>
      }
    >
      <Table
        head={[
          "日期 (UTC)",
          "limited 推理",
          "limited CONNECT",
          "隧道峰值",
          "会话峰值",
          "泄漏 (确认)",
        ]}
        empty="没有数据"
      >
        {[...data.days].reverse().map((d) => (
          <tr key={d.day} className="border-b border-border last:border-0">
            <Cell>
              <span className="font-mono">{d.day.slice(0, 10)}</span>
            </Cell>
            <Cell numeric>{d.limited_inference}</Cell>
            <Cell numeric>{d.limited_connect}</Cell>
            <Cell numeric>{d.tunnel_peak}</Cell>
            <Cell numeric>{d.session_peak}</Cell>
            <Cell numeric>{d.leaked}</Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}

export function History({ data }: { data: Dashboard }) {
  return (
    <Section title="换绑历史：最近 50 条">
      <Table
        head={["会话", "账号", "开始", "结束", "原因"]}
        empty="没有换绑记录"
      >
        {data.history.map((h) => (
          <tr
            key={`${h.session}\t${h.from}\t${h.alias}`}
            className="border-b border-border last:border-0"
          >
            <Cell>
              <code className="font-mono">{h.session}</code>
            </Cell>
            <Cell>{h.alias}</Cell>
            <Cell>
              <Time iso={h.from} short />
            </Cell>
            <Cell>
              {h.to === undefined ? "仍绑定" : <Time iso={h.to} short />}
            </Cell>
            <Cell>
              <code className="font-mono">{h.reason}</code>
            </Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}
