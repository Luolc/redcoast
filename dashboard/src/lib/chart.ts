/**
 * The unbroken stretches of a series, each as its points: a null ends one,
 * so a line is never drawn across an hour without a value.
 */
export function runs<T>(
  values: (number | null)[],
  point: (v: number, i: number) => T,
): T[][] {
  const out: T[][] = [];
  let run: T[] = [];
  values.forEach((v, i) => {
    if (v === null) {
      if (run.length > 0) out.push(run);
      run = [];
    } else run.push(point(v, i));
  });
  if (run.length > 0) out.push(run);
  return out;
}

export interface Reading {
  at: string;
  utilization: number;
}

/**
 * A quota history as the runs of a step line, each as its corners
 * [ms, utilization]: a reading holds until the next one, since the history
 * keeps a reading only when the value changed or an hour passed. A reset
 * at cut between two readings ends a run there: past its reset a reading no
 * longer holds, and the next reading starts a new run.
 */
export function steps(
  readings: Reading[],
  cut: number | null = null,
): [number, number][][] {
  const out: [number, number][][] = [];
  let run: [number, number][] = [];
  let before: { t: number; u: number } | null = null;
  for (const r of readings) {
    const t = Date.parse(r.at);
    if (before !== null && cut !== null && before.t < cut && cut <= t) {
      run.push([cut, before.u]);
      out.push(run);
      run = [];
    } else if (before !== null && before.u !== r.utilization)
      run.push([t, before.u]);
    run.push([t, r.utilization]);
    before = { t, u: r.utilization };
  }
  if (run.length > 0) out.push(run);
  return out;
}
