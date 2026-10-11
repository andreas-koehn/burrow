import { useState, useEffect } from "react";
import { Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useTheme } from "@/components/theme-provider";
import { useAuth } from "@/auth/useAuth";
import type { Service } from "@/lib/contract";
import { CommandPalette } from "@/components/CommandPalette";
import { Sidebar } from "@/components/shell/Sidebar";
import { TopBar } from "@/components/shell/TopBar";
import { FOOTER_ENTRIES, breadcrumbFor, navigationFor, workspacesFor } from "@/lib/navigation";
import { lastWorkspace, rememberWorkspace, workspaceFor } from "@/lib/workspace";
import { clientsOnline, useKnownClients } from "@/lib/useKnownClients";
import { useRelayNotices } from "@/lib/useRelayNotices";

const COLLAPSED_KEY = "burrow.sidebarCollapsed";

/** Below this width the open sidebar leaves the page too little room to read. */
const NARROW_PX = 640;

/**
 * Whether the sidebar starts collapsed: as the reader last left it; without
 * such a choice, collapsed on a narrow screen (a phone), open otherwise.
 */
function storedCollapsed(): boolean {
  let stored: string | null = null;
  try { stored = localStorage.getItem(COLLAPSED_KEY); } catch { /* storage unavailable */ }
  if (stored === "1" || stored === "0") return stored === "1";
  return typeof window !== "undefined" && window.innerWidth > 0 && window.innerWidth < NARROW_PX;
}

export function Layout() {
  const nav = useNavigate();
  const { pathname } = useLocation();
  const qc = useQueryClient();
  const { theme, toggleTheme } = useTheme();
  const { user } = useAuth();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [paletteKey, setPaletteKey] = useState(0);
  const [collapsed, setCollapsed] = useState(storedCollapsed);
  const openPalette = () => { setPaletteKey((k) => k + 1); setPaletteOpen(true); };
  const toggleSidebar = () => {
    const next = !collapsed;
    setCollapsed(next);
    try { localStorage.setItem(COLLAPSED_KEY, next ? "1" : "0"); } catch { /* storage unavailable: not remembered */ }
  };
  async function logout() {
    try { await apiFetch("/auth/logout", { method: "POST" }); } catch { /* ignore */ }
    qc.clear();
    nav("/login", { replace: true });
  }
  // The AI Gateway used to be gated on "≥1 service has access_mode === api_key"
  // (P0-2). That kept it hidden on a fresh stack — even for admins — because
  // the dashboard is needed to MINT an API-key gated service in the first
  // place. It is now visible to every admin (the only role that can configure
  // those resources anyway). Non-admins still see it when at least one http
  // service is connected, so a user with only the ai:configure:own permission
  // still finds the entry point.
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const isAdmin = user?.role === "admin";
  // Empty for non-admins, and no request is made for them.
  const relayNotices = useRelayNotices();
  // The rows of the Clients page, from the request that page makes too. The
  // endpoint is admin-gated; nobody else is asked for, or gets, a count.
  const clients = useKnownClients({ enabled: isAdmin });
  const servicesList = Array.isArray(services.data) ? services.data : [];
  const clientsList = clients.rows ?? [];
  const hasAiGateway = isAdmin
    || servicesList.some((s) => s.type === "http" && s.connected);
  const ctx = { isAdmin, hasAiGateway };

  // The sidebar follows the URL, except on a relay-level page (Clients): that one is
  // shown in the workspace the reader came from — Services for someone the AI Gateway
  // is not shown to, whatever an earlier visit left behind.
  const last = hasAiGateway ? lastWorkspace() : "services";
  const workspace = workspaceFor(pathname, last);
  // The other exception: a /gateway/ deep link opened by someone the AI Gateway is
  // not shown to. The page renders as it always did, but the shell offers neither
  // the gateway's navigation nor a way to switch to it — it stays on Services,
  // exactly what that user sees elsewhere.
  const gatewayHidden = workspace === "gateway" && !hasAiGateway;
  const navigation = navigationFor(gatewayHidden ? "services" : workspace, ctx);

  // Names for the breadcrumb's object crumb; an id nobody has a name for shows as itself.
  const names: Record<string, string> = {};
  for (const s of servicesList) names[s.id] = s.name;
  for (const c of clientsList) for (const s of c.sessions) names[s.session_id] = c.token_name;
  const crumbs = gatewayHidden
    ? [{ label: navigation.label, to: navigation.home }]
    : breadcrumbFor(pathname, ctx, names, last);

  useEffect(() => {
    if (!gatewayHidden) rememberWorkspace(pathname);
  }, [pathname, gatewayHidden]);

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
      <Sidebar
        navigation={navigation}
        workspaces={workspacesFor(ctx)}
        // Inside Settings both shortcuts would only repeat entries of the list above them.
        footer={navigation.workspace === "settings" ? [] : FOOTER_ENTRIES.filter((e) => !e.adminOnly || isAdmin)}
        pathname={pathname}
        collapsed={collapsed}
        counts={{
          services: Array.isArray(services.data) ? servicesList.length : undefined,
          // The rows the Clients page shows as Online, so the two always agree.
          clientsOnline: clients.rows ? clientsOnline(clients.rows) : undefined,
        }}
        attention={relayNotices.map((n) => n.action.to)}
        user={{ email: user?.email ?? "", isAdmin, role: user?.role }}
        theme={theme}
        onSearch={openPalette}
        onToggleTheme={toggleTheme}
        onLogout={logout}
      />

      <div className="shell-column">
        <TopBar crumbs={crumbs} collapsed={collapsed} onToggle={toggleSidebar} />
        <main className="shell-main" id="main" tabIndex={-1}>
          <div className="shell-content"><Outlet /></div>
        </main>
      </div>

      <CommandPalette
        key={paletteKey}
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
        isAdmin={isAdmin}
        hasAiGateway={hasAiGateway}
      />
    </div>
  );
}
