import { Link } from "react-router-dom";
import { ArrowLeft, LogOut, Moon, Search, Sun } from "lucide-react";
import { cx } from "@/components/ds";
import { shortcutLabel } from "@/lib/platform";
import { NAVIGATIONS, activeEntry, attentionTarget, type NavEntry, type Navigation } from "@/lib/navigation";
import { lastWorkspace } from "@/lib/workspace";
import { WorkspaceSwitcher } from "./WorkspaceSwitcher";

export interface SidebarProps {
  /** The navigation of the workspace the URL is in, already filtered for this user. */
  navigation: Navigation;
  /** The workspaces this user may switch between. */
  workspaces: Navigation[];
  /** Shortcuts shown above the user chip. */
  footer: NavEntry[];
  pathname: string;
  collapsed: boolean;
  counts: { services?: number; clientsOnline?: number };
  /** Pages where an open relay notice is resolved; their entries carry a dot. */
  attention: string[];
  user: { email: string; isAdmin: boolean; role?: string };
  theme: "light" | "dark";
  onSearch(): void;
  onToggleTheme(): void;
  onLogout(): void;
}

const PROFILE = NAVIGATIONS.settings.groups.flatMap((g) => g.entries).find((e) => e.id === "profile")!;

/** Read out with an entry that carries the dot; the dot alone is only colour. */
const ATTENTION = "needs attention";

/** What the figure at the right edge shows, and how it is read out. */
function countFor(entry: NavEntry, counts: SidebarProps["counts"]): { text: string; spoken: string } | undefined {
  const n = entry.count ? counts[entry.count] : undefined;
  if (n === undefined) return undefined;
  return entry.count === "clientsOnline" ? { text: `${n} on`, spoken: `${n} online` } : { text: String(n), spoken: String(n) };
}

export function Sidebar({
  navigation, workspaces, footer, pathname, collapsed, counts, attention, user, theme, onSearch, onToggleTheme, onLogout,
}: SidebarProps) {
  const active = activeEntry(pathname, navigation);
  const back = workspaces.find((w) => w.workspace === lastWorkspace()) ?? workspaces[0] ?? NAVIGATIONS.services;
  const themeLabel = theme === "dark" ? "Switch to light theme" : "Switch to dark theme";
  const profileLabel = user.email ? `Your profile, ${user.email}` : "Your profile";

  const link = (entry: NavEntry, current: boolean) => {
    const Icon = entry.icon;
    const count = countFor(entry, counts);
    const waiting = attentionTarget(entry, attention);
    const name = [entry.label, count?.spoken, waiting && ATTENTION].filter(Boolean).join(", ");
    return (
      <Link
        key={entry.id}
        to={waiting ?? entry.to}
        className={cx("nav-item", current && "is-active")}
        aria-current={current ? "page" : undefined}
        aria-label={name}
        title={collapsed ? name : undefined}
      >
        <span className="nav-icon"><Icon size={16} aria-hidden="true" /></span>
        {!collapsed && <span className="nav-label">{entry.label}</span>}
        {!collapsed && count && <span className="nav-count">{count.text}</span>}
        {waiting && <span className="nav-attention"><span className="visually-hidden">{ATTENTION}</span></span>}
      </Link>
    );
  };

  return (
    <aside className={cx("sidebar", collapsed && "is-collapsed")}>
      {navigation.workspace === "settings" ? (
        <div className="sidebar-brand sidebar-back">
          <Link
            to={back.home}
            className="nav-item"
            aria-label={`Back to ${back.label}`}
            title={collapsed ? `Back to ${back.label}` : undefined}
          >
            <span className="nav-icon"><ArrowLeft size={16} aria-hidden="true" /></span>
            {!collapsed && <span className="nav-label">Back to {back.label}</span>}
          </Link>
          {!collapsed && <div className="workspace-title">{navigation.label}</div>}
        </div>
      ) : (
        <div className="sidebar-brand">
          <WorkspaceSwitcher current={navigation} workspaces={workspaces} collapsed={collapsed} />
        </div>
      )}

      <nav className="sidebar-nav" aria-label={navigation.label}>
        {/* ⌘K search affordance */}
        <button
          type="button"
          className="nav-item nav-search"
          onClick={onSearch}
          aria-label="Search"
          title={collapsed ? "Search" : undefined}
        >
          <span className="nav-icon"><Search size={16} aria-hidden="true" /></span>
          {!collapsed && <span className="nav-label">Search</span>}
          {!collapsed && <span className="nav-count"><span className="kbd-token">{shortcutLabel("K")}</span></span>}
        </button>

        {navigation.groups.map((g, i) => (
          <div className="nav-group" key={g.title ?? `group-${i}`}>
            {g.title && !collapsed && <div className="nav-group-title">{g.title}</div>}
            {g.entries.map((e) => link(e, e === active))}
          </div>
        ))}
      </nav>

      <div className="sidebar-footer">
        {footer.length > 0 && <div className="sidebar-footer-nav">{footer.map((e) => link(e, false))}</div>}
        <div className="sidebar-footer-row row row-center gap-2">
          <Link className="user-chip" to={PROFILE.to} aria-label={profileLabel} title={collapsed ? profileLabel : undefined}>
            <span className="avatar">{(user.email[0] ?? "U").toUpperCase()}</span>
            {!collapsed && (
              <span className="user-meta">
                {user.email && <span className="user-email" title={user.email}>{user.email}</span>}
                {user.role && <span className="user-role">{user.role.toUpperCase()}</span>}
              </span>
            )}
          </Link>
          <button className="theme-toggle" type="button" onClick={onToggleTheme} aria-label={themeLabel} title={themeLabel}>
            {theme === "dark" ? <Sun size={15} /> : <Moon size={15} />}
          </button>
          <button type="button" className="icon-btn" onClick={onLogout} aria-label="Log out" title="Log out">
            <LogOut size={15} />
          </button>
        </div>
      </div>
    </aside>
  );
}
