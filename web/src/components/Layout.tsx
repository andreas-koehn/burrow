import { useState, useEffect } from "react";
import { Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useTheme } from "@/components/theme-provider";
import { useAuth } from "@/auth/useAuth";
import type { ClientView, Service } from "@/lib/contract";
import { CommandPalette } from "@/components/CommandPalette";
import { Sidebar } from "@/components/shell/Sidebar";
import { TopBar } from "@/components/shell/TopBar";
import { FOOTER_ENTRIES, breadcrumbFor, navigationFor, workspacesFor } from "@/lib/navigation";
import { rememberWorkspace, workspaceFor } from "@/lib/workspace";

const COLLAPSED_KEY = "burrow.sidebarCollapsed";

function storedCollapsed(): boolean {
  try { return localStorage.getItem(COLLAPSED_KEY) === "1"; } catch { return false; }
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
  // Same key and fetch as the Clients page, so the two share one cache entry.
  // The endpoint is admin-gated; nobody else gets a count.
  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    retry: false,
    enabled: isAdmin,
  });
  const servicesList = Array.isArray(services.data) ? services.data : [];
  const clientsList = Array.isArray(clients.data) ? clients.data : [];
  const hasAiGateway = isAdmin
    || servicesList.some((s) => s.type === "http" && s.connected);
  const ctx = { isAdmin, hasAiGateway };

  // The sidebar follows the URL. The one exception: a /gateway/ deep link opened
  // by someone the AI Gateway is not shown to. The page renders as it always
  // did, but the shell offers neither the gateway's navigation nor a way to
  // switch to it — it stays on Services, exactly what that user sees elsewhere.
  const workspace = workspaceFor(pathname);
  const gatewayHidden = workspace === "gateway" && !hasAiGateway;
  const navigation = navigationFor(gatewayHidden ? "services" : workspace, ctx);

  // Names for the breadcrumb's object crumb; an id nobody has a name for shows as itself.
  const names: Record<string, string> = {};
  for (const s of servicesList) names[s.id] = s.name;
  for (const c of clientsList) names[c.session_id] = c.token_name;
  const crumbs = gatewayHidden
    ? [{ label: navigation.label, to: navigation.home }]
    : breadcrumbFor(pathname, ctx, names);

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
          // Every row of /clients is a connected session, so the list length is the online count.
          clientsOnline: Array.isArray(clients.data) ? clientsList.length : undefined,
        }}
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
