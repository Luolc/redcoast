import { useCallback, useEffect, useState } from "react";
import { poller, stopped } from "./poll";
import { parseRoute, type Route } from "./route";
import { parseDashboard, type Dashboard } from "./types";

export interface Loaded {
  data: Dashboard | null;
  // Set when the last fetch failed; `data` keeps the answer before it.
  fetchError: string | null;
  // Fetches again now, outside the schedule.
  reload: () => void;
}

// A load still unanswered after this many refresh intervals is given up,
// so that the next one can run.
const LOAD_TIMEOUT_INTERVALS = 3;

/** Fetches url now and every everyMs. */
export function useData(url: string, everyMs: number): Loaded {
  const [state, setState] = useState<Omit<Loaded, "reload">>({
    data: null,
    fetchError: null,
  });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    // Refresh starts this effect again, which stops the load in flight, so
    // an older answer never lands after a newer one.
    const loads = poller(async (signal) => {
      try {
        const response = await fetch(url, { cache: "no-store", signal });
        if (!response.ok) throw new Error(`HTTP ${String(response.status)}`);
        const data = parseDashboard(await response.text());
        if (!signal.aborted) setState({ data, fetchError: null });
      } catch (e) {
        if (!stopped(signal))
          setState((l) => ({ ...l, fetchError: String(e) }));
      }
    }, LOAD_TIMEOUT_INTERVALS * everyMs);
    loads.run();
    const timer = setInterval(loads.run, everyMs);
    return () => {
      loads.stop();
      clearInterval(timer);
    };
  }, [url, everyMs, tick]);
  const reload = useCallback(() => {
    setTick((t) => t + 1);
  }, []);
  return { ...state, reload };
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

/** The route in the address's fragment, following its changes. */
export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(location.hash));
  useEffect(() => {
    const follow = () => {
      setRoute(parseRoute(location.hash));
    };
    window.addEventListener("hashchange", follow);
    return () => {
      window.removeEventListener("hashchange", follow);
    };
  }, []);
  return route;
}

export type Theme = "system" | "light" | "dark";

const THEME_KEY = "redcoast-theme";

function storedTheme(): Theme {
  try {
    const t = localStorage.getItem(THEME_KEY);
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

/** The chosen theme, kept in local storage and set on the root element. */
export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState(storedTheme);
  useEffect(() => {
    if (theme === "system") delete document.documentElement.dataset["theme"];
    else document.documentElement.dataset["theme"] = theme;
    try {
      if (theme === "system") localStorage.removeItem(THEME_KEY);
      else localStorage.setItem(THEME_KEY, theme);
    } catch {
      // Storage may be off; the choice then lasts for this page only.
    }
  }, [theme]);
  return [theme, setTheme];
}
