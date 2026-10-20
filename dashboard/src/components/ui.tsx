import {
  CircleCheck,
  CircleSlash,
  CircleX,
  Clock,
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
import { pace, type State, type Tone } from "../lib/stats";
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
  id,
  children,
}: {
  title: string;
  note?: ReactNode;
  id?: string;
  children: ReactNode;
}) {
  return (
    <section className="section" id={id}>
      <div className="section-head">
        <h2>{title}</h2>
        {note === undefined ? null : <span className="note">{note}</span>}
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

// A reading older than this is shown as stale: the account served nothing since.
const STALE_MS = 30 * 60_000;

/**
 * One quota window: a bar with the soft limit as a tick and an even pace
 * through the window as a hollow mark, then the reset and the pace.
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
  if (quota === undefined)
    return <span className="muted small">No reading</span>;
  const u = quota.utilization;
  const tone: Tone = paused
    ? "off"
    : u >= hard
      ? "bad"
      : u >= soft
        ? "warn"
        : "ok";
  const p = paused ? null : pace(quota, window, now);
  const stale = now.getTime() - Date.parse(quota.observed_at) > STALE_MS;
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
          ) : Date.parse(quota.reset_at) <= now.getTime() ? (
            <Time iso={quota.reset_at}>reset passed</Time>
          ) : (
            <Time iso={quota.reset_at}>
              resets {formatUntil(quota.reset_at, now)}
            </Time>
          )}
        </span>
        {p === null ? null : <PaceText by={p.by} />}
        {stale ? (
          <span className="stale s-warn">
            <Clock className="icon sm" aria-hidden="true" /> read{" "}
            <Ago iso={quota.observed_at} now={now} />
          </span>
        ) : null}
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
