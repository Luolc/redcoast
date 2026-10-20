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
