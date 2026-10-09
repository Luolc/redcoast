// Small runtime type guards for dashboard.json. A guard for an object takes
// one guard per field, typed against the field, so the compiler rejects a
// shape that drifts from the TypeScript type it claims to check.
export type Guard<T> = (v: unknown) => v is T;

export const isString: Guard<string> = (v) => typeof v === "string";

export const isBoolean: Guard<boolean> = (v) => typeof v === "boolean";

export const isNumber: Guard<number> = (v): v is number =>
  typeof v === "number" && Number.isFinite(v);

export function nullable<T>(guard: Guard<T>): Guard<T | null> {
  return (v): v is T | null => v === null || guard(v);
}

/** A key the Go side omits when its value is zero or empty. */
export function optional<T>(guard: Guard<T>): Guard<T | undefined> {
  return (v): v is T | undefined => v === undefined || guard(v);
}

export function arrayOf<T>(guard: Guard<T>): Guard<T[]> {
  return (v): v is T[] => Array.isArray(v) && v.every(guard);
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

export function recordOf<T>(guard: Guard<T>): Guard<Record<string, T>> {
  return (v): v is Record<string, T> =>
    isRecord(v) && Object.values(v).every(guard);
}

export function object<T extends object>(shape: {
  [K in keyof T]-?: Guard<T[K]>;
}): Guard<T> {
  const fields: [string, Guard<unknown>][] = Object.entries(shape);
  return (v): v is T =>
    isRecord(v) && fields.every(([name, guard]) => guard(v[name]));
}
