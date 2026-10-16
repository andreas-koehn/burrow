import { Routes, Route, Navigate, useLocation, useParams } from "react-router-dom";
import { RequireAuth } from "@/auth/RequireAuth";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { RequirePermission } from "@/components/RequirePermission";
import { Layout } from "@/components/Layout";
import Login from "@/pages/Login";
import ServicesOverview from "@/pages/ServicesOverview";
import Services from "@/pages/Services";
import Users from "@/pages/Users";
import Roles from "@/pages/Roles";
import GeneralSettings from "@/pages/settings/GeneralSettings";
import EmailSettings from "@/pages/settings/EmailSettings";
import Profile from "@/pages/settings/Profile";
import Sessions from "@/pages/settings/Sessions";
import Clients from "@/pages/Clients";
import ClientDetail from "@/pages/ClientDetail";
import ConnectClient from "@/pages/ConnectClient";
import LinkClient from "@/pages/LinkClient";
import Providers from "@/pages/Providers";
import ProviderDetail from "@/pages/ProviderDetail";
import PromptCache from "@/pages/PromptCache";
import Guardrails from "@/pages/Guardrails";
import RequestInspector from "@/pages/RequestInspector";
import Requests from "@/pages/Requests";
import Traffic from "@/pages/Traffic";
import CostBudgets from "@/pages/CostBudgets";
import AuditLog from "@/pages/AuditLog";
import Webhooks from "@/pages/Webhooks";
import McpSettings from "@/pages/settings/McpSettings";

import AutomationTokens from "@/pages/AutomationTokens";
import BackupRestore from "@/pages/BackupRestore";
import ServiceDetail from "@/pages/ServiceDetail";
import Retention from "@/pages/Retention";
import DatabaseBackend from "@/pages/DatabaseBackend";
import OpenApiViewer from "@/pages/OpenApiViewer";
import GatewayOverview from "@/pages/GatewayOverview";
import GatewayModels from "@/pages/GatewayModels";
import GatewayKeys from "@/pages/GatewayKeys";
import { allEntries } from "@/lib/navigation";
import { RedirectTo } from "@/lib/redirects";
import { OLD_ROUTES } from "@/lib/moved-routes";

// Custom domains need host routing, which is off; old links land on the service.
function ServiceDomainsRedirect() {
  const { id } = useParams<{ id: string }>();
  return <Navigate to={`/services/${id}`} replace />;
}

// What a page needs is what its navigation entry says, so the route and the sidebar cannot disagree.
const everything = { can: () => true, hasAiGateway: true };
const needsOf = (path: string) => allEntries(everything).find((x) => x.entry.to === path)?.entry.needs ?? [];

// Settings has no page of its own: whoever may change the relay's settings starts there, everyone else on their profile.
function SettingsIndex() {
  const { user } = useAuth();
  const manages = useCan(P.settingsManage);
  const { search, hash } = useLocation();
  if (!user) return null; // RequireAuth above has not resolved yet
  return <Navigate to={{ pathname: manages ? "/settings/general" : "/settings/profile", search, hash }} replace />;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      {/* Where `burrow login` sends the browser: outside the shell, behind the login. */}
      <Route path="/link" element={<RequireAuth><LinkClient /></RequireAuth>} />
      <Route element={<RequireAuth><Layout /></RequireAuth>}>
        <Route path="/" element={<ServicesOverview />} />
        <Route path="/services" element={<Services />} />
        {/* /ai/ is the gateway's data plane and never reaches the SPA; its pages live under /gateway/. */}
        <Route path="/gateway" element={<GatewayOverview />} />
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="/gateway/providers/:slug" element={<ProviderDetail />} />
        <Route path="/gateway/models" element={<GatewayModels />} />
        <Route path="/gateway/keys" element={<GatewayKeys />} />
        <Route path="/gateway/cache" element={<PromptCache />} />
        <Route path="/gateway/guardrails" element={<Guardrails />} />
        <Route path="/gateway/requests" element={<Requests />} />
        <Route path="/gateway/requests/:serviceId/:requestId?" element={<RequestInspector />} />
        <Route path="/gateway/cost" element={<CostBudgets />} />
        <Route path="/clients" element={<Clients />} />
        <Route path="/clients/connect" element={<ConnectClient />} />
        <Route path="/clients/:id" element={<ClientDetail />} />
        <Route path="/traffic" element={<Traffic />} />
        <Route path="/services/:id" element={<ServiceDetail />} />
        <Route path="/services/:id/domains" element={<ServiceDomainsRedirect />} />
        {/* Settings. A page with a `needs` in lib/navigation.ts is guarded by exactly that list. */}
        <Route path="/settings" element={<SettingsIndex />} />
        <Route path="/settings/general" element={<RequirePermission needs={needsOf("/settings/general")}><GeneralSettings /></RequirePermission>} />
        <Route path="/settings/email" element={<RequirePermission needs={needsOf("/settings/email")}><EmailSettings /></RequirePermission>} />
        <Route path="/settings/retention" element={<RequirePermission needs={needsOf("/settings/retention")}><Retention /></RequirePermission>} />
        <Route path="/settings/database" element={<RequirePermission needs={needsOf("/settings/database")}><DatabaseBackend /></RequirePermission>} />
        <Route path="/settings/backups" element={<RequirePermission needs={needsOf("/settings/backups")}><BackupRestore /></RequirePermission>} />
        <Route path="/settings/users" element={<RequirePermission needs={needsOf("/settings/users")}><Users /></RequirePermission>} />
        <Route path="/settings/roles" element={<RequirePermission needs={needsOf("/settings/roles")}><Roles /></RequirePermission>} />
        <Route path="/settings/audit" element={<RequirePermission needs={needsOf("/settings/audit")}><AuditLog /></RequirePermission>} />
        <Route path="/settings/webhooks" element={<RequirePermission needs={needsOf("/settings/webhooks")}><Webhooks /></RequirePermission>} />
        <Route path="/settings/mcp" element={<RequirePermission needs={needsOf("/settings/mcp")}><McpSettings /></RequirePermission>} />
        {/* P1-14: in-app OpenAPI viewer, framed inside the dashboard chrome. */}
        <Route path="/settings/api" element={<RequirePermission needs={needsOf("/settings/api")}><OpenApiViewer /></RequirePermission>} />
        <Route path="/settings/profile" element={<Profile />} />
        <Route path="/settings/sessions" element={<RequirePermission needs={needsOf("/settings/sessions")}><Sessions /></RequirePermission>} />
        <Route path="/settings/automation" element={<RequirePermission needs={needsOf("/settings/automation")}><AutomationTokens /></RequirePermission>} />
        {/* Old bookmarks: every moved path lands on its new home, params, query and hash intact. */}
        {OLD_ROUTES.map((r) => <Route key={r.from} path={r.from} element={<RedirectTo to={r.to} />} />)}
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
