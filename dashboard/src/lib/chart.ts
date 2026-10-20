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
 * A quota history as the corners of a step line, [ms, utilization]: a
 * reading holds until the next one, since the history keeps a reading only
 * when the value changed or an hour passed.
 */
export function steps(readings: Reading[]): [number, number][] {
  const out: [number, number][] = [];
  let before: number | null = null;
  for (const r of readings) {
    const t = Date.parse(r.at);
    if (before !== null && before !== r.utilization) out.push([t, before]);
    out.push([t, r.utilization]);
    before = r.utilization;
  }
  return out;
}
