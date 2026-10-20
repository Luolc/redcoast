import { useEffect, useRef, useState, type ReactNode } from "react";
import { formatClock } from "../lib/format";

/** The smallest of 1, 2, 3, 4, 5, 6, 8 or 10 times a power of ten that is at least v. */
function niceMax(v: number): number {
  const e = 10 ** Math.floor(Math.log10(Math.max(v, 1)));
  for (const m of [1, 2, 3, 4, 5, 6, 8, 10]) if (m * e >= v) return m * e;
  return 10 * e;
}

// The size used before the first measurement, and in tests.
const DEFAULT_SIZE = { width: 640, height: 133 };

function useSize() {
  const ref = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState(DEFAULT_SIZE);
  useEffect(() => {
    const el = ref.current;
    if (el === null) return;
    const observer = new ResizeObserver(([entry]) => {
      if (entry === undefined) return;
      const { width, height } = entry.contentRect;
      if (width > 0 && height > 0) setSize({ width, height });
    });
    observer.observe(el);
    return () => {
      observer.disconnect();
    };
  }, []);
  return [ref, size] as const;
}

/**
 * Bars over hours, oldest first, drawn at the container's width; hovering an
 * hour shows tip(i).
 */
export function HourBars({
  label,
  hours,
  values,
  bad = false,
  tip,
}: {
  label: string;
  hours: string[];
  values: number[];
  bad?: boolean;
  tip: (i: number) => ReactNode;
}) {
  const [ref, { width: W, height: H }] = useSize();
  const [hover, setHover] = useState<number | null>(null);
  const L = 46;
  const R = 8;
  const T = 6;
  const B = 20;
  const w = W - L - R;
  const h = H - T - B;
  const n = hours.length;
  const step = n === 0 ? w : w / n;
  const max = niceMax(Math.max(0, ...values) * 1.05);
  const y = (v: number) => T + h - (v / max) * h;
  const bw = Math.max(step - 2, 1);
  return (
    <div className="chart" ref={ref}>
      <svg
        viewBox={`0 0 ${String(W)} ${String(H)}`}
        role="img"
        aria-label={label}
      >
        {[0, 1, 2].map((i) => {
          const v = (max / 2) * i;
          return (
            <g key={i}>
              <line
                className={i === 0 ? "base" : "grid"}
                x1={L}
                x2={L + w}
                y1={y(v)}
                y2={y(v)}
              />
              <text x={L - 6} y={y(v) + 4} textAnchor="end">
                {Math.round(v).toLocaleString("en-US")}
              </text>
            </g>
          );
        })}
        {hours.map((iso, i) =>
          new Date(iso).getUTCHours() % 6 === 0 ? (
            <text
              key={iso}
              x={L + step * i + step / 2}
              y={H - 4}
              textAnchor="middle"
            >
              {formatClock(iso)}
            </text>
          ) : null,
        )}
        {values.map((v, i) => {
          if (v <= 0) return null;
          const x = L + step * i + 1;
          const top = y(v);
          const r = Math.min(3, (T + h - top) / 2, bw / 2);
          return (
            <path
              // eslint-disable-next-line @eslint-react/no-array-index-key -- one bar per hour, in a fixed order
              key={i}
              className={bad ? "bar-bad" : "bar"}
              d={`M${String(x)},${String(T + h)} V${String(top + r)} Q${String(x)},${String(top)} ${String(x + r)},${String(top)} H${String(x + bw - r)} Q${String(x + bw)},${String(top)} ${String(x + bw)},${String(top + r)} V${String(T + h)} Z`}
            />
          );
        })}
        {hours.map((iso, i) => (
          <rect
            key={iso}
            className="hit"
            x={L + step * i}
            y={T}
            width={step}
            height={h}
            onMouseEnter={() => {
              setHover(i);
            }}
            onMouseLeave={() => {
              setHover(null);
            }}
          />
        ))}
      </svg>
      {hover === null ? null : (
        <div
          className="tip"
          role="tooltip"
          style={{
            left: `${String(Math.min(L + step * hover + step, W - 200))}px`,
            top: `${String(T)}px`,
          }}
        >
          {tip(hover)}
        </div>
      )}
    </div>
  );
}

/**
 * A trend line over hours, oldest first, filling its tile's width; each
 * hour's value is in its tooltip. An hour without a value (null) breaks the
 * line.
 */
export function Spark({
  label,
  hours,
  values,
  format,
}: {
  label: string;
  hours: string[];
  values: (number | null)[];
  format: (v: number) => string;
}) {
  const W = 200;
  const H = 40;
  const n = values.length;
  if (n < 2) return null;
  const top = Math.max(0, ...values.filter((v) => v !== null));
  const max = (top > 0 ? top : 1) * 1.1;
  const x = (i: number) => (i * W) / (n - 1);
  const y = (v: number) => H - (v / max) * H;
  const runs: string[][] = [[]];
  values.forEach((v, i) => {
    if (v === null) runs.push([]);
    else runs[runs.length - 1]?.push(`${x(i).toFixed(1)},${y(v).toFixed(1)}`);
  });
  const last = values[n - 1];
  return (
    <svg
      className="tile-spark"
      viewBox={`0 0 ${String(W)} ${String(H)}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={`${label}, last 24 h`}
    >
      {hours.map((iso, i) => {
        const v = values[i];
        return (
          <rect
            key={iso}
            className="hit"
            x={x(i) - W / (n - 1) / 2}
            y={0}
            width={W / (n - 1)}
            height={H}
          >
            <title>{`${formatClock(iso)} UTC: ${v === null || v === undefined ? "none" : format(v)}`}</title>
          </rect>
        );
      })}
      {runs
        .filter((r) => r.length > 1)
        .map((r) => (
          <polyline key={r[0]} className="l" points={r.join(" ")} />
        ))}
      {last === null || last === undefined ? null : (
        <circle className="d" cx={x(n - 1)} cy={y(last)} r={2.5} />
      )}
    </svg>
  );
}

export interface Line {
  name: string;
  values: (number | null)[]; // null where the hour has no value
  dashed?: boolean;
}

/**
 * Lines over hours, oldest first, in the one chart hue: a second line is
 * told apart by its dash and its name at its end. An hour without a value
 * breaks the line.
 */
export function HourLines({
  label,
  hours,
  lines,
  formatAxis,
  tip,
}: {
  label: string;
  hours: string[];
  lines: Line[];
  formatAxis: (v: number) => string;
  tip: (i: number) => ReactNode;
}) {
  const [ref, { width: W, height: H }] = useSize();
  const [hover, setHover] = useState<number | null>(null);
  const L = 46;
  const R = 40;
  const T = 6;
  const B = 20;
  const w = W - L - R;
  const h = H - T - B;
  const n = hours.length;
  const step = n === 0 ? w : w / n;
  const all = lines.flatMap((l) => l.values.filter((v) => v !== null));
  const max = niceMax(Math.max(0, ...all) * 1.05);
  const x = (i: number) => L + step * i + step / 2;
  const y = (v: number) => T + h - (v / max) * h;
  return (
    <div className="chart" ref={ref}>
      <svg
        viewBox={`0 0 ${String(W)} ${String(H)}`}
        role="img"
        aria-label={label}
      >
        {[0, 1, 2].map((i) => {
          const v = (max / 2) * i;
          return (
            <g key={i}>
              <line
                className={i === 0 ? "base" : "grid"}
                x1={L}
                x2={L + w}
                y1={y(v)}
                y2={y(v)}
              />
              <text x={L - 6} y={y(v) + 4} textAnchor="end">
                {formatAxis(v)}
              </text>
            </g>
          );
        })}
        {hours.map((iso, i) =>
          new Date(iso).getUTCHours() % 6 === 0 ? (
            <text key={iso} x={x(i)} y={H - 4} textAnchor="middle">
              {formatClock(iso)}
            </text>
          ) : null,
        )}
        {lines.map((l) => {
          const runs: [number, number][][] = [[]];
          l.values.forEach((v, i) => {
            if (v === null) runs.push([]);
            else runs[runs.length - 1]?.push([x(i), y(v)]);
          });
          const last = l.values.findLast((v) => v !== null);
          return (
            <g key={l.name}>
              {runs.map((r) => {
                const [first] = r;
                if (first === undefined) return null;
                const key = first.join(",");
                return r.length === 1 ? (
                  <circle
                    key={key}
                    className="dot-1"
                    cx={first[0]}
                    cy={first[1]}
                    r={2}
                  />
                ) : (
                  <polyline
                    key={key}
                    className={l.dashed === true ? "line-1 dash" : "line-1"}
                    points={r
                      .map(([px, py]) => `${px.toFixed(1)},${py.toFixed(1)}`)
                      .join(" ")}
                  />
                );
              })}
              {last === undefined ? null : (
                <text className="label-1" x={L + w + 6} y={y(last) + 4}>
                  {l.name}
                </text>
              )}
            </g>
          );
        })}
        {hours.map((iso, i) => (
          <rect
            key={iso}
            className="hit"
            x={L + step * i}
            y={T}
            width={step}
            height={h}
            onMouseEnter={() => {
              setHover(i);
            }}
            onMouseLeave={() => {
              setHover(null);
            }}
          />
        ))}
      </svg>
      {hover === null ? null : (
        <div
          className="tip"
          role="tooltip"
          style={{
            left: `${String(Math.min(L + step * hover + step, W - 200))}px`,
            top: `${String(T)}px`,
          }}
        >
          {tip(hover)}
        </div>
      )}
    </div>
  );
}
