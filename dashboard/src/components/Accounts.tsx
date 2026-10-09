import { formatAge, formatDuration, formatPercent } from "../lib/format";
import type { Account, Dashboard, Quota } from "../lib/types";
import { Bar, Cell, Flag, Section, Table, Time } from "./ui";

// A reading older than this is shown as stale: the account served nothing since.
const STALE_MS = 60 * 60_000;

const egressLabels: Record<string, string> = {
  ok: "一致",
  mismatch: "不一致",
  unread: "读不出 IP",
};

function Window({
  quota,
  now,
  soft,
  hard,
}: {
  quota: Quota | undefined;
  now: Date;
  soft: number;
  hard: number;
}) {
  if (quota === undefined)
    return <span className="text-muted-foreground">没有读数</span>;
  const age = now.getTime() - Date.parse(quota.observed_at);
  const reset =
    quota.reset_at === undefined
      ? null
      : Date.parse(quota.reset_at) - now.getTime();
  return (
    <div className="space-y-0.5 whitespace-nowrap">
      <div className="flex items-center gap-2">
        <Bar fraction={quota.utilization} soft={soft} hard={hard} />
        <span className="font-mono tabular-nums">
          {formatPercent(quota.utilization)}
        </span>
        <span className="text-muted-foreground">
          {quota.status === "" ? "无 status" : quota.status}
        </span>
      </div>
      <div className="text-xs text-muted-foreground">
        重置：
        {reset === null ? (
          "不明"
        ) : reset > 0 ? (
          `还有 ${formatDuration(reset)}`
        ) : (
          <Flag tone="warn">已过重置时间</Flag>
        )}
      </div>
      <div className="text-xs text-muted-foreground">
        读于{" "}
        {age > STALE_MS ? (
          <Flag tone="warn">{formatAge(quota.observed_at, now)}</Flag>
        ) : (
          formatAge(quota.observed_at, now)
        )}
        ，来源 {quota.source}
      </div>
    </div>
  );
}

function Availability({ account }: { account: Account }) {
  if (account.paused_until === undefined) {
    return <span className="text-emerald-700 dark:text-emerald-400">可用</span>;
  }
  return (
    <div>
      <Flag tone="bad">暂停</Flag>
      <div className="text-xs">
        到 <Time iso={account.paused_until} short />
      </div>
      <div className="text-xs text-muted-foreground">
        {account.pause_reason}
      </div>
    </div>
  );
}

function PlanCell({ account }: { account: Account }) {
  return (
    <div className="whitespace-nowrap">
      <div>{account.plan?.name ?? <Flag tone="bad">无档位</Flag>}</div>
      {account.next_plan === null ? null : (
        <div className="text-xs text-muted-foreground">
          <Time iso={account.next_plan.effective_from} short /> 起{" "}
          {account.next_plan.name}
        </div>
      )}
    </div>
  );
}

export function Accounts({ data, now }: { data: Dashboard; now: Date }) {
  return (
    <Section
      title="账号"
      note={`条形的颜色按网关当前阈值：soft ${formatPercent(data.soft)} 起黄色，hard ${formatPercent(data.hard)} 起红色。读数超过 1 小时标黄。会话列是绑定数 / 1 小时内活跃数，推理列是进行中 / 每账号上限。出口核验与进行中推理数是本次启动以来的内存读数。`}
    >
      <Table
        head={["账号", "档位", "可用", "出口", "5h", "7d", "会话", "推理"]}
        empty="网关当前没有账号"
      >
        {data.accounts.map((a) => (
          <tr key={a.alias} className="border-b border-border last:border-0">
            <Cell>
              <div className="font-medium">{a.alias}</div>
              <div className="text-xs text-muted-foreground">{a.email}</div>
            </Cell>
            <Cell>
              <PlanCell account={a} />
            </Cell>
            <Cell>
              <Availability account={a} />
            </Cell>
            <Cell>
              <div className="font-mono">{a.expected_egress_ip}</div>
              <div className="text-xs">
                {a.egress_check === undefined ? (
                  <span className="text-muted-foreground">未核验</span>
                ) : (
                  <>
                    {a.egress_check.outcome === "ok" ? (
                      "一致"
                    ) : (
                      <Flag tone="bad">
                        {egressLabels[a.egress_check.outcome] ??
                          a.egress_check.outcome}
                      </Flag>
                    )}
                    ，{formatAge(a.egress_check.at, now)}
                  </>
                )}
              </div>
            </Cell>
            <Cell>
              <Window
                quota={a.quota["5h"]}
                now={now}
                soft={data.soft}
                hard={data.hard}
              />
            </Cell>
            <Cell>
              <Window
                quota={a.quota["7d"]}
                now={now}
                soft={data.soft}
                hard={data.hard}
              />
            </Cell>
            <Cell numeric>
              {a.bound_sessions} / {a.active_sessions}
            </Cell>
            <Cell numeric>
              {a.in_flight} / {a.concurrency}
            </Cell>
          </tr>
        ))}
      </Table>
    </Section>
  );
}
