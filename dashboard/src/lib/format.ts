export type Zone = "America/Los_Angeles" | "UTC";

export const LA: Zone = "America/Los_Angeles";

const formats = new Map<Zone, Intl.DateTimeFormat>();

function format(zone: Zone): Intl.DateTimeFormat {
  let f = formats.get(zone);
  if (f === undefined) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hourCycle: "h23",
      timeZoneName: "short",
    });
    formats.set(zone, f);
  }
  return f;
}

/** `2026-10-01 09:15:03 PDT`, or `10-01 09:15` with short. */
export function formatTime(iso: string, zone: Zone, short = false): string {
  const all = format(zone).formatToParts(new Date(iso));
  const get = (type: Intl.DateTimeFormatPartTypes) =>
    all.find((p) => p.type === type)?.value ?? "";
  if (short)
    return `${get("month")}-${get("day")} ${get("hour")}:${get("minute")}`;
  return `${get("year")}-${get("month")}-${get("day")} ${get("hour")}:${get("minute")}:${get("second")} ${get("timeZoneName")}`;
}

/** The hour and minute of iso in UTC, `09:00`. */
export function formatClock(iso: string): string {
  return iso.slice(11, 16);
}

/** The hour starting at iso, in UTC, `09:00–10:00 UTC`. */
export function formatHourSpan(iso: string): string {
  const end = new Date(Date.parse(iso) + 3_600_000).toISOString();
  return `${formatClock(iso)}–${formatClock(end)} UTC`;
}

// A non-breaking space keeps a number on the line of its unit.
const NB = "\u00a0";

/** A span of time in its two largest units: `45 s`, `3 min`, `2 h 10 min`, `3 d 4 h`. */
export function formatSpan(ms: number): string {
  const m = Math.max(0, Math.floor(ms / 60_000));
  if (ms < 60_000) return `${String(Math.max(0, Math.floor(ms / 1000)))}${NB}s`;
  if (m < 60) return `${String(m)}${NB}min`;
  if (m < 1440) {
    const rest = m % 60;
    return rest === 0
      ? `${String(Math.floor(m / 60))}${NB}h`
      : `${String(Math.floor(m / 60))}${NB}h ${String(rest)}${NB}min`;
  }
  const hours = Math.floor((m % 1440) / 60);
  return hours === 0
    ? `${String(Math.floor(m / 1440))}${NB}d`
    : `${String(Math.floor(m / 1440))}${NB}d ${String(hours)}${NB}h`;
}

/** A request's duration: `850 ms`, `15.0 s`, `1 min 05 s`. */
export function formatMs(ms: number): string {
  if (ms < 1000) return `${String(Math.round(ms))}${NB}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}${NB}s`;
  const s = Math.round(ms / 1000);
  return `${String(Math.floor(s / 60))}${NB}min ${String(s % 60).padStart(2, "0")}${NB}s`;
}

/** How long ago iso was at now, `7 min ago`. */
export function formatAgo(iso: string, now: Date): string {
  return `${formatSpan(now.getTime() - Date.parse(iso))} ago`;
}

/** How long until iso from now, `in 2 h 36 min`, or `due now` once it passed. */
export function formatUntil(iso: string, now: Date): string {
  const ms = Date.parse(iso) - now.getTime();
  return ms <= 0 ? "due now" : `in ${formatSpan(ms)}`;
}

/** A token count in thousands or millions: `512`, `1.8 k`, `845 k`, `1.2 M`. */
export function formatTokens(n: number): string {
  const units: [number, string][] = [
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "k"],
  ];
  // A count that would round up to the next unit is written in it.
  for (const [size, unit] of units)
    if (n >= size * 0.9995) {
      const v = n / size;
      return `${v < 9.95 ? v.toFixed(1) : String(Math.round(v))}${NB}${unit}`;
    }
  return String(Math.round(n));
}

/** Bytes in binary units with one decimal: `512 B`, `1.5 KiB`, `2.0 GiB`. */
export function formatBytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return unit === 0
    ? `${String(n)}${NB}B`
    : `${value.toFixed(1)}${NB}${units[unit] ?? ""}`;
}

/** A fraction as a percentage with digits decimals, `42%` or `1.2%`. */
export function formatPercent(fraction: number, digits = 0): string {
  return `${(fraction * 100).toFixed(digits)}%`;
}

/** A count with thousands separators, `6,405`. */
export function formatCount(n: number): string {
  return Math.round(n).toLocaleString("en-US");
}
