import { useState, useEffect } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Moon, Sun, Waypoints, KeyRound, Users, UserCircle, LogOut, Boxes, ShieldCheck,
  ServerCog, Globe, Sparkles, DollarSign, Database, ShieldAlert, Search, ScrollText,
  Webhook as WebhookIcon, Bot, LayoutDashboard,
} from "lucide-react";
import { apiFetch } from "@/lib/api";
import { Button, cx } from "@/components/ds";
import { useTheme } from "@/components/theme-provider";
import { useAuth } from "@/auth/useAuth";
import type { Service } from "@/lib/contract";
import { CommandPalette } from "@/components/CommandPalette";

/* Brand mark — geometric tunnel-and-arrow glyph (currentColor, never Signal Teal). */
function BurrowMark({ size = 20 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="5.5" width="12" height="13" rx="2.5" />
      <path d="M9 12h11.5" />
      <path d="M17 9l3.5 3-3.5 3" />
    </svg>
  );
}

export function Layout() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const { theme, toggleTheme } = useTheme();
  const { user } = useAuth();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [paletteKey, setPaletteKey] = useState(0);
  const openPalette = () => { setPaletteKey((k) => k + 1); setPaletteOpen(true); };
  async function logout() {
    try { await apiFetch("/auth/logout", { method: "POST" }); } catch { /* ignore */ }
    qc.clear();
    nav("/login", { replace: true });
  }
  // AI GATEWAY group used to be gated on "≥1 service has access_mode === api_key"
  // (P0-2). That keeps the group hidden on a fresh stack — even for admins —
  // because the dashboard is needed to MINT an API-key gated service in the
  // first place. The group is now visible to every admin (the only role that
  // can configure those resources anyway). Non-admins still see the group
  // when at least one http service is connected, so a user with only the
  // ai:configure:own permission still finds the entry point.
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const isAdmin = user?.role === "admin";
  const servicesList = Array.isArray(services.data) ? services.data : [];
  const firstHttpServiceId = servicesList.find((s) => s.type === "http")?.id;
  const hasAiEndpoints = isAdmin
    || servicesList.some((s) => s.type === "http" && s.connected);
  const navItem = ({ isActive }: { isActive: boolean }) => cx("nav-item", isActive && "is-active");
  const avatarInitial = (user?.email?.[0] ?? "U").toUpperCase();

  // ⌘K / Ctrl+K global shortcut to open the command palette.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        openPalette();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);
  return (
    <div className="app-shell" style={{ display: "flex", minHeight: "100vh", position: "relative", background: "var(--background)", color: "var(--foreground)" }}>
      <a className="skip-link" href="#main">Skip to content</a>
      <nav className="sidebar" aria-label="Main">
        <div className="sidebar-brand">
          <BurrowMark />
          <span className="wordmark">Burrow</span>
        </div>

        <div className="sidebar-nav">
          {/* ⌘K search affordance */}
          <button
            className="nav-item"
            onClick={openPalette}
            aria-label="Search"
            style={{ width: "100%", textAlign: "left", background: "none", border: "none", cursor: "pointer" }}
          >
            <span className="nav-icon"><Search size={16} /></span>
            <span className="nav-label">Search</span>
            <span className="nav-count"><span className="kbd-token">⌘K</span></span>
          </button>

          <NavLink to="/" end className={navItem}>
            <span className="nav-icon"><LayoutDashboard size={16} /></span>
            <span className="nav-label">Home</span>
          </NavLink>

          <div className="nav-group">
            <div className="nav-group-title">Tunneling</div>
            <NavLink to="/clients" className={navItem}>
              <span className="nav-icon"><Boxes size={16} /></span>
              <span className="nav-label">Clients</span>
            </NavLink>
            <NavLink to="/tunnels" className={navItem}>
              <span className="nav-icon"><Waypoints size={16} /></span>
              <span className="nav-label">Tunnels</span>
            </NavLink>
            <NavLink to="/services" className={navItem}>
              <span className="nav-icon"><Globe size={16} /></span>
              <span className="nav-label">Services</span>
            </NavLink>
            <NavLink to="/tokens" className={navItem}>
              <span className="nav-icon"><KeyRound size={16} /></span>
              <span className="nav-label">Tokens</span>
            </NavLink>
          </div>

          {hasAiEndpoints && (
            <div className="nav-group">
              <div className="nav-group-title">AI GATEWAY</div>
              <NavLink to="/ai/endpoints" className={navItem}>
                <span className="nav-icon"><Sparkles size={16} /></span>
                <span className="nav-label">AI endpoints</span>
              </NavLink>
              <NavLink to="/cost" className={navItem}>
                <span className="nav-icon"><DollarSign size={16} /></span>
                <span className="nav-label">Cost & budgets</span>
              </NavLink>
              <NavLink to="/cache" className={navItem}>
                <span className="nav-icon"><Database size={16} /></span>
                <span className="nav-label">Prompt cache</span>
              </NavLink>
              <NavLink to="/guardrails" className={navItem}>
                <span className="nav-icon"><ShieldAlert size={16} /></span>
                <span className="nav-label">Guardrails</span>
              </NavLink>
              {firstHttpServiceId && (
                <NavLink to={`/inspector/${firstHttpServiceId}`} className={navItem}>
                  <span className="nav-icon"><Search size={16} /></span>
                  <span className="nav-label">Request inspector</span>
                </NavLink>
              )}
            </div>
          )}

          {user?.role === "admin" && (
            <div className="nav-group">
              <div className="nav-group-title">Access control</div>
              <NavLink to="/users" className={navItem}>
                <span className="nav-icon"><Users size={16} /></span>
                <span className="nav-label">Users</span>
              </NavLink>
              <NavLink to="/roles" className={navItem}>
                <span className="nav-icon"><ShieldCheck size={16} /></span>
                <span className="nav-label">Roles</span>
              </NavLink>

            </div>
          )}

          {user?.role === "admin" && (
            <div className="nav-group">
              <div className="nav-group-title">Administration</div>
              <NavLink to="/settings" end className={navItem}>
                <span className="nav-icon"><ServerCog size={16} /></span>
                <span className="nav-label">Settings</span>
              </NavLink>
              <NavLink to="/audit" className={navItem}>
                <span className="nav-icon"><ScrollText size={16} /></span>
                <span className="nav-label">Audit</span>
              </NavLink>
              <NavLink to="/webhooks" className={navItem}>
                <span className="nav-icon"><WebhookIcon size={16} /></span>
                <span className="nav-label">Webhooks</span>
              </NavLink>
              {/* P2-10: Backup, Connection logs, OpenAPI moved off the top-level
                  sidebar. They remain reachable from /settings cards so Settings
                  is the single canonical configuration hub. */}
            </div>
          )}

          <div className="nav-group">
            <div className="nav-group-title">Account</div>
            <NavLink to="/account" end className={navItem}>
              <span className="nav-icon"><UserCircle size={16} /></span>
              <span className="nav-label">Account</span>
            </NavLink>
            <NavLink to="/account/automation" className={navItem}>
              <span className="nav-icon"><Bot size={16} /></span>
              <span className="nav-label">Automation</span>
            </NavLink>
          </div>
        </div>

        <div className="sidebar-footer">
          <div className="row row-center gap-2">
            <div className="user-chip">
              <span className="avatar">{avatarInitial}</span>
              <span className="user-meta">
                {user?.email && <span className="user-email">{user.email}</span>}
                {user?.role && <span className="user-role">{user.role.toUpperCase()}</span>}
              </span>
            </div>
            <button
              className="theme-toggle"
              onClick={toggleTheme}
              aria-label={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
              title={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
            >
              {theme === "dark" ? <Sun size={15} /> : <Moon size={15} />}
            </button>
          </div>
          <Button variant="secondary" size="sm" icon={<LogOut size={14} />} onClick={logout}>
            Log out
          </Button>
        </div>
      </nav>

      <main className="shell-main" id="main" tabIndex={-1}>
        <div className="shell-content"><Outlet /></div>
      </main>

      <CommandPalette
        key={paletteKey}
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
        isAdmin={isAdmin}
        hasAiEndpoints={hasAiEndpoints}
        firstHttpServiceId={firstHttpServiceId}
      />
    </div>
  );
}
