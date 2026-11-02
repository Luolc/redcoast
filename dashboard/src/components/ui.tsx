import {
  CircleCheck,
  CircleSlash,
  CircleX,
  Info,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import type { ReactNode } from "react";
import {
  formatAgo,
  formatPercent,
  formatTime,
  formatUntil,
  LA,
} from "../lib/format";
import { expired, pace, windowTier, type State, type Tone } from "../lib/stats";
import type { Quota } from "../lib/types";

const TONE_ICONS: Record<Tone, LucideIcon> = {
  ok: CircleCheck,
  warn: TriangleAlert,
  bad: CircleX,
  info: Info,
  off: CircleSlash,
};

/** A state as its icon and its word, so it never rests on color alone. */
export function Status({
  state,
  pill = false,
  icon,
}: {
  state: State;
  pill?: boolean;
  icon?: LucideIcon;
}) {
  const Icon = icon ?? TONE_ICONS[state.tone];
  return (
    <span className={`status s-${state.tone}${pill ? " pill" : ""}`}>
      <Icon className="icon sm solid" aria-hidden="true" />
      {state.word}
    </span>
  );
}

/** A time as text, with the time itself in UTC and Los Angeles on hover. */
export function Time({ iso, children }: { iso: string; children: ReactNode }) {
  return (
    <time
      dateTime={iso}
      title={`${formatTime(iso, "UTC")} · ${formatTime(iso, LA)}`}
    >
      {children}
    </time>
  );
}

export function Ago({ iso, now }: { iso: string; now: Date }) {
  return <Time iso={iso}>{formatAgo(iso, now)}</Time>;
}

export function PageHead({ title, sub }: { title: string; sub: ReactNode }) {
  return (
    <div className="page-head">
      <h1>{title}</h1>
      <div className="sub">{sub}</div>
    </div>
  );
}

export function Section({
  title,
  note,
  action,
  id,
  children,
}: {
  title: string;
  note?: ReactNode;
  action?: ReactNode; // at the right of the head
  id?: string;
  children: ReactNode;
}) {
  return (
    <section className="section" id={id}>
      <div className="section-head">
        <h2>{title}</h2>
        {note === undefined ? null : <span className="note">{note}</span>}
        {action === undefined ? null : <span className="right">{action}</span>}
      </div>
      {children}
    </section>
  );
}

/** A dashed box that says why there is nothing to show. */
export function Empty({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="empty">
      <p className="serif">{title}</p>
      {children === undefined ? null : <p>{children}</p>}
    </div>
  );
}

/** A small bar of part's share of whole. */
export function Share({ part, whole }: { part: number; whole: number }) {
  const fraction = whole <= 0 ? 0 : part / whole;
  return (
    <span className="share" title={formatPercent(fraction, 1)}>
      <i style={{ width: `${(Math.max(0.02, fraction) * 100).toFixed(1)}%` }} />
    </span>
  );
}

/**
 * One quota window: a bar with the soft limit as a tick and an even pace
 * through the window as a hollow mark, then the reset and the pace. Its
 * ink follows how selection reads the window (windowTier); a reading whose
 * reset has passed shows as no reading, as selection takes it.
 */
export function Meter({
  quota,
  window,
  soft,
  hard,
  paused,
  now,
}: {
  quota: Quota | undefined;
  window: string;
  soft: number;
  hard: number;
  paused: boolean;
  now: Date;
}) {
  if (quota === undefined || expired(quota, now))
    return <span className="muted small">No reading</span>;
  const u = quota.utilization;
  const tier = windowTier(quota, soft, hard);
  const tone: Tone = paused
    ? "off"
    : {
        open: "ok" as const,
        soft: "warn" as const,
        exhausted: "bad" as const,
      }[tier];
  const p = paused ? null : pace(quota, window, now);
  return (
    <div className={`meter s-${tone}`}>
      <div className="track">
        <div
          className="fill"
          style={{ width: `${String(Math.min(u, 1) * 100)}%` }}
        />
        <div
          className="soft"
          style={{ left: `${String(soft * 100)}%` }}
          title={`Soft limit ${formatPercent(soft)}`}
        />
        {p === null ? null : (
          <div
            className="even"
            style={{ left: `${(Math.min(p.elapsed, 1) * 100).toFixed(1)}%` }}
            title={`Even pace: ${formatPercent(p.elapsed)} of the window has passed`}
          />
        )}
      </div>
      <span className="pct">{formatPercent(u)}</span>
      <span className="when">
        <span>
          {quota.reset_at === undefined ? (
            "reset unknown"
          ) : (
            <Time iso={quota.reset_at}>
              resets {formatUntil(quota.reset_at, now)}
            </Time>
          )}
        </span>
        {quota.status === "rejected" ? (
          <span className="s-bad">rejected by the upstream</span>
        ) : null}
        {p === null ? null : <PaceText by={p.by} />}
      </span>
    </div>
  );
}

/** Points of the window used beyond an even pace; marked from 5 over. */
function PaceText({ by }: { by: number }) {
  const n = by > 0 ? `+${String(by)}` : by < 0 ? `−${String(-by)}` : "0";
  if (by >= 5)
    return (
      <span
        className="pace over"
        title={`${String(by)} points more used than an even pace through the window: at this rate it runs out before it resets`}
      >
        <TriangleAlert className="icon sm solid" aria-hidden="true" />
        Pace {n}
      </span>
    );
  return (
    <span
      className="pace"
      title={
        by <= -5
          ? `${String(-by)} points less used than an even pace: at this rate it lasts until it resets`
          : "About on an even pace through the window"
      }
    >
      Pace {n}
    </span>
  );
}
