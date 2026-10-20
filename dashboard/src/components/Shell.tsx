import {
  Activity,
  Check,
  ChevronDown,
  ChevronRight,
  Clock,
  Cpu,
  ExternalLink,
  Globe,
  LayoutDashboard,
  List,
  Monitor as MonitorIcon,
  Moon,
  RefreshCw,
  Sun,
  Terminal,
  User,
  type LucideIcon,
} from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  providerHref,
  routeHref,
  VIEW_NAMES,
  type Route,
  type View,
} from "../lib/route";
import { healthState } from "../lib/stats";
import type { Dashboard } from "../lib/types";
import type { Theme } from "../lib/useData";
import { providerName } from "../lib/words";
import { Ago } from "./ui";

function ProviderIcon({ provider }: { provider: string | undefined }) {
  return provider === "codex" ? (
    <Cpu className="icon" aria-hidden="true" />
  ) : (
    <Terminal className="icon" aria-hidden="true" />
  );
}

function host(url: string): string {
  return url.replace(/^https?:\/\//, "").replace(/\/$/, "");
}

/**
 * The provider of this gateway, and a menu of the other providers'
 * dashboards when the gateway lists any. Choosing one opens that
 * dashboard on the same view: each provider is its own process.
 */
function ProviderMenu({ data, route }: { data: Dashboard; route: Route }) {
  const [open, setOpen] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const outside = (e: MouseEvent) => {
      if (
        !(e.target instanceof Node) ||
        boxRef.current?.contains(e.target) !== true
      )
        setOpen(false);
    };
    const escape = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("click", outside);
    document.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("click", outside);
      document.removeEventListener("keydown", escape);
    };
  }, [open]);

  const provider = data.gateway.provider;
  const name = providerName(provider);
  const health = healthState(data);
  const links = (data.gateway.dashboards ?? []).flatMap((d) => {
    const href = providerHref(d.url, routeHref(route));
    return href === null ? [] : [{ ...d, href }];
  });
  const face = (
    <>
      <ProviderIcon provider={provider} />
      <span className="label">{name}</span>
      <span className={`status s-${health.tone}`}>
        <span className="dot" />
        <span className="kind">{health.word}</span>
      </span>
    </>
  );
  if (links.length === 0) {
    return (
      <div className="provider">
        <div className="provider-button static">{face}</div>
      </div>
    );
  }
  return (
    <div className="provider" ref={boxRef}>
      <button
        type="button"
        className="provider-button"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => {
          setOpen(!open);
        }}
      >
        {face}
        <ChevronDown className="icon sm" aria-hidden="true" />
      </button>
      {open ? (
        <div className="menu" role="menu" aria-label="Provider">
          <a
            className="menu-item"
            role="menuitemradio"
            aria-checked="true"
            href={routeHref(route)}
            onClick={() => {
              setOpen(false);
            }}
          >
            <ProviderIcon provider={provider} />
            <span className="grow">
              <span className="name">{name}</span>
              <span className="host mono">{host(location.origin)}</span>
            </span>
            <Check className="icon sm" aria-hidden="true" />
          </a>
          {links.map((d) => {
            return (
              <a
                key={d.provider}
                className="menu-item"
                role="menuitemradio"
                aria-checked="false"
                href={d.href}
                title={`Opens ${d.url}`}
              >
                <ProviderIcon provider={d.provider} />
                <span className="grow">
                  <span className="name">{providerName(d.provider)}</span>
                  <span className="host mono">{host(d.url)}</span>
                </span>
                <ExternalLink className="icon sm faint" aria-hidden="true" />
              </a>
            );
          })}
          <div className="menu-note">
            Each provider serves its own dashboard. Choosing one opens it, on
            the same page.
          </div>
        </div>
      ) : null}
    </div>
  );
}

function NavItem({
  view,
  current,
  icon: Icon,
  count,
}: {
  view: View;
  current: View;
  icon: LucideIcon;
  count?: number;
}) {
  return (
    <a
      href={routeHref({ view })}
      aria-current={view === current ? "page" : undefined}
    >
      <Icon className="icon" aria-hidden="true" />
      {VIEW_NAMES[view]}
      {count === undefined ? null : <span className="count">{count}</span>}
    </a>
  );
}

export function Sidebar({
  data,
  route,
}: {
  data: Dashboard | null;
  route: Route;
}) {
  const v = route.view;
  return (
    <aside className="sidebar" aria-label="Navigation">
      <div className="brand">redcoast</div>
      {data === null ? null : <ProviderMenu data={data} route={route} />}
      <nav className="nav" aria-label="Pages">
        <div className="nav-group">Traffic</div>
        <NavItem view="overview" current={v} icon={LayoutDashboard} />
        <NavItem
          view="clients"
          current={v}
          icon={Terminal}
          {...(data === null
            ? {}
            : {
                count: data.machines.filter((m) => m.revoked_at === undefined)
                  .length,
              })}
        />
        <NavItem
          view="sessions"
          current={v}
          icon={List}
          {...(data === null ? {} : { count: data.sessions.length })}
        />
        <div className="nav-group">Upstream</div>
        <NavItem
          view="accounts"
          current={v}
          icon={User}
          {...(data === null ? {} : { count: data.accounts.length })}
        />
        <NavItem view="exits" current={v} icon={Globe} />
      </nav>
      <nav className="nav nav-bottom" aria-label="Gateway">
        <NavItem view="monitor" current={v} icon={Activity} />
      </nav>
    </aside>
  );
}

const THEMES: { theme: Theme; label: string; icon: LucideIcon }[] = [
  { theme: "system", label: "System theme", icon: MonitorIcon },
  { theme: "light", label: "Light theme", icon: Sun },
  { theme: "dark", label: "Dark theme", icon: Moon },
];

export function Topbar({
  data,
  crumbs,
  now,
  theme,
  setTheme,
  reload,
}: {
  data: Dashboard | null;
  crumbs: ReactNode[];
  now: Date;
  theme: Theme;
  setTheme: (t: Theme) => void;
  reload: () => void;
}) {
  return (
    <header className="topbar">
      <ol className="crumbs" aria-label="Breadcrumb">
        <li>Gateway</li>
        {crumbs.map((c, i) => (
          // eslint-disable-next-line @eslint-react/no-array-index-key -- the trail's places are its order
          <li key={i}>
            <ChevronRight className="icon sm" aria-hidden="true" />
            {c}
          </li>
        ))}
      </ol>
      <div className="tools">
        {data === null ? null : (
          <span className="item">
            <Clock className="icon sm" aria-hidden="true" />
            Updated <Ago iso={data.at} now={now} />
          </span>
        )}
        <div className="seg" role="group" aria-label="Theme">
          {THEMES.map(({ theme: t, label, icon: Icon }) => (
            <button
              key={t}
              type="button"
              aria-pressed={theme === t}
              aria-label={label}
              title={label}
              onClick={() => {
                setTheme(t);
              }}
            >
              <Icon className="icon sm" aria-hidden="true" />
            </button>
          ))}
        </div>
        <button type="button" className="btn" onClick={reload}>
          <RefreshCw className="icon sm" aria-hidden="true" />
          Refresh
        </button>
      </div>
    </header>
  );
}
