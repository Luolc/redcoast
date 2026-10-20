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
