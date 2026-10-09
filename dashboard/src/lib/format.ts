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

/** A duration in its two largest units: `45s`, `3m 05s`, `2h 10m`, `3d 4h`. */
export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const pad = (n: number) => String(n).padStart(2, "0");
  if (s < 60) return `${String(s)}s`;
  if (s < 3600) return `${String(Math.floor(s / 60))}m ${pad(s % 60)}s`;
  if (s < 86400)
    return `${String(Math.floor(s / 3600))}h ${pad(Math.floor((s % 3600) / 60))}m`;
  return `${String(Math.floor(s / 86400))}d ${String(Math.floor((s % 86400) / 3600))}h`;
}

/** How long ago iso was at now, `3m 05s 前`. */
export function formatAge(iso: string, now: Date): string {
  return `${formatDuration(now.getTime() - Date.parse(iso))} 前`;
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
    ? `${String(n)} B`
    : `${value.toFixed(1)} ${units[unit] ?? ""}`;
}

/** A fraction as a whole percentage, `42%`. */
export function formatPercent(fraction: number): string {
  return `${String(Math.round(fraction * 100))}%`;
}
