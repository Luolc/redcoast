import type { ReactNode } from "react";
import { formatTime, LA } from "../lib/format";

export function Section({
  title,
  note,
  children,
}: {
  title: string;
  note?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="rounded-lg border border-border bg-card p-4 shadow-xs">
      <h2 className="text-base font-semibold">{title}</h2>
      {note === undefined ? null : (
        <div className="mt-1 text-sm text-muted-foreground">{note}</div>
      )}
      <div className="mt-3 overflow-x-auto">{children}</div>
    </section>
  );
}

/** A table whose first row is head; an empty body shows empty instead. */
export function Table({
  head,
  empty,
  children,
}: {
  head: string[];
  empty: string;
  children: ReactNode[];
}) {
  if (children.length === 0) {
    return <p className="text-sm text-muted-foreground">{empty}</p>;
  }
  return (
    <table className="w-full border-collapse text-sm">
      <thead>
        <tr className="border-b border-border text-left text-muted-foreground">
          {head.map((h) => (
            <th
              key={h}
              scope="col"
              className="px-2 py-1 font-medium whitespace-nowrap"
            >
              {h}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>{children}</tbody>
    </table>
  );
}

export function Cell({
  children,
  numeric = false,
}: {
  children: ReactNode;
  numeric?: boolean;
}) {
  return (
    <td
      className={
        numeric
          ? "px-2 py-1 text-right font-mono whitespace-nowrap tabular-nums"
          : "px-2 py-1 align-top"
      }
    >
      {children}
    </td>
  );
}

/** A time in Los Angeles; the title gives it in UTC. */
export function Time({ iso, short = false }: { iso: string; short?: boolean }) {
  return (
    <time
      dateTime={iso}
      title={formatTime(iso, "UTC")}
      className="font-mono whitespace-nowrap tabular-nums"
    >
      {formatTime(iso, LA, short)}
    </time>
  );
}

/** Text that stands out: warn for something to look at, bad for a fault. */
export function Flag({
  tone,
  children,
}: {
  tone: "warn" | "bad";
  children: ReactNode;
}) {
  return (
    <span
      className={
        tone === "bad"
          ? "font-medium text-red-700 dark:text-red-400"
          : "font-medium text-amber-700 dark:text-amber-400"
      }
    >
      {children}
    </span>
  );
}

/** A bar of fraction, full from 1 on: red from the hard threshold, amber from soft. */
export function Bar({
  fraction,
  soft,
  hard,
}: {
  fraction: number;
  soft: number;
  hard: number;
}) {
  const width = `${String(Math.min(100, Math.max(0, fraction * 100)))}%`;
  const color =
    fraction >= hard
      ? "bg-red-500"
      : fraction >= soft
        ? "bg-amber-500"
        : "bg-emerald-500";
  return (
    <div className="h-2 w-24 rounded-sm bg-muted" aria-hidden="true">
      <div className={`h-2 rounded-sm ${color}`} style={{ width }} />
    </div>
  );
}
