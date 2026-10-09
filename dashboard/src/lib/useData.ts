import { useEffect, useState } from "react";
import { parseDashboard, type Dashboard } from "./types";

export interface Loaded {
  data: Dashboard | null;
  // Set when the last fetch failed; `data` keeps the answer before it.
  fetchError: string | null;
}

/** Fetches url now and every everyMs. */
export function useData(url: string, everyMs: number): Loaded {
  const [loaded, setLoaded] = useState<Loaded>({
    data: null,
    fetchError: null,
  });
  useEffect(() => {
    let alive = true;
    async function load() {
      try {
        const response = await fetch(url, { cache: "no-store" });
        if (!response.ok) throw new Error(`HTTP ${String(response.status)}`);
        const data = parseDashboard(await response.text());
        if (alive) setLoaded({ data, fetchError: null });
      } catch (e) {
        if (alive) setLoaded((l) => ({ ...l, fetchError: String(e) }));
      }
    }
    void load();
    const timer = setInterval(() => {
      void load();
    }, everyMs);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [url, everyMs]);
  return loaded;
}

/** The current time, updated every everyMs, for ages and countdowns. */
export function useNow(everyMs: number): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = setInterval(() => {
      setNow(new Date());
    }, everyMs);
    return () => {
      clearInterval(timer);
    };
  }, [everyMs]);
  return now;
}
