import { Routes, Route, Navigate, useLocation, useParams } from "react-router-dom";
import { RequireAuth } from "@/auth/RequireAuth";
import { useAuth } from "@/auth/useAuth";
import { RequireAdmin } from "@/components/RequireAdmin";
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

import AutomationTokens from "@/pages/AutomationTokens";
import BackupRestore from "@/pages/BackupRestore";
import ServiceDetail from "@/pages/ServiceDetail";
import Retention from "@/pages/Retention";
import DatabaseBackend from "@/pages/DatabaseBackend";
import OpenApiViewer from "@/pages/OpenApiViewer";
import GatewayOverview from "@/pages/GatewayOverview";
import { RedirectTo } from "@/lib/redirects";
import { OLD_ROUTES } from "@/lib/moved-routes";

// Custom domains need host routing, which is off; old links land on the service.
function ServiceDomainsRedirect() {
  const { id } = useParams<{ id: string }>();
  return <Navigate to={`/services/${id}`} replace />;
}

// Settings has no page of its own: an admin starts on the relay's settings, everyone else on their profile.
function SettingsIndex() {
  const { user } = useAuth();
  const { search, hash } = useLocation();
  if (!user) return null; // RequireAuth above has not resolved yet
  return <Navigate to={{ pathname: user.role === "admin" ? "/settings/general" : "/settings/profile", search, hash }} replace />;
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
        {/* Settings. The admin-only pages are the entries marked adminOnly in lib/navigation.ts. */}
        <Route path="/settings" element={<SettingsIndex />} />
        <Route path="/settings/general" element={<RequireAdmin><GeneralSettings /></RequireAdmin>} />
        <Route path="/settings/email" element={<RequireAdmin><EmailSettings /></RequireAdmin>} />
        <Route path="/settings/retention" element={<RequireAdmin><Retention /></RequireAdmin>} />
        <Route path="/settings/database" element={<RequireAdmin><DatabaseBackend /></RequireAdmin>} />
        <Route path="/settings/backups" element={<RequireAdmin><BackupRestore /></RequireAdmin>} />
        <Route path="/settings/users" element={<RequireAdmin><Users /></RequireAdmin>} />
        <Route path="/settings/roles" element={<RequireAdmin><Roles /></RequireAdmin>} />
        <Route path="/settings/audit" element={<RequireAdmin><AuditLog /></RequireAdmin>} />
        <Route path="/settings/webhooks" element={<RequireAdmin><Webhooks /></RequireAdmin>} />
        {/* P1-14: in-app OpenAPI viewer, framed inside the dashboard chrome. */}
        <Route path="/settings/api" element={<RequireAdmin><OpenApiViewer /></RequireAdmin>} />
        <Route path="/settings/profile" element={<Profile />} />
        <Route path="/settings/sessions" element={<Sessions />} />
        <Route path="/settings/automation" element={<AutomationTokens />} />
        {/* Old bookmarks: every moved path lands on its new home, params, query and hash intact. */}
        {OLD_ROUTES.map((r) => <Route key={r.from} path={r.from} element={<RedirectTo to={r.to} />} />)}
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
