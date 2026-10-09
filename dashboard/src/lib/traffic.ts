import type { HourTraffic } from "./types";

export interface Column {
  key: string;
  alias: string; // empty for requests carried on no account
  kind: string;
}

export interface HourRow {
  hour: string;
  cells: Map<string, HourTraffic>; // by column key
}

/** Pivots the hourly rows: one row per hour, newest first, one column per account and kind. */
export function byHour(rows: HourTraffic[]): {
  columns: Column[];
  hours: HourRow[];
} {
  const columns = new Map<string, Column>();
  const hours = new Map<string, HourRow>();
  for (const r of rows) {
    const key = `${r.alias}\t${r.kind}`;
    columns.set(key, { key, alias: r.alias, kind: r.kind });
    let row = hours.get(r.hour);
    if (row === undefined) {
      row = { hour: r.hour, cells: new Map() };
      hours.set(r.hour, row);
    }
    row.cells.set(key, r);
  }
  return {
    // The key is alias, tab, kind: sorting it sorts by alias, then kind.
    columns: [...columns.values()].sort((a, b) => a.key.localeCompare(b.key)),
    hours: [...hours.values()].sort((a, b) => b.hour.localeCompare(a.hour)),
  };
}
