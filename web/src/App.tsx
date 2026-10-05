import { Routes, Route, Navigate, useParams } from "react-router-dom";
import { RequireAuth } from "@/auth/RequireAuth";
import { Layout } from "@/components/Layout";
import Login from "@/pages/Login";
import Home from "@/pages/Home";
import Tunnels from "@/pages/Tunnels";
import Services from "@/pages/Services";
import Tokens from "@/pages/Tokens";
import Account from "@/pages/Account";
import Users from "@/pages/Users";
import Roles from "@/pages/Roles";
import Settings from "@/pages/Settings";
import Clients from "@/pages/Clients";
import ClientDetail from "@/pages/ClientDetail";
import ConnectClient from "@/pages/ConnectClient";
import Providers from "@/pages/Providers";
import ProviderDetail from "@/pages/ProviderDetail";
import PromptCache from "@/pages/PromptCache";
import Guardrails from "@/pages/Guardrails";
import RequestInspector from "@/pages/RequestInspector";
import InspectorIndex from "@/pages/InspectorIndex";
import CostBudgets from "@/pages/CostBudgets";
import AuditLog from "@/pages/AuditLog";
import Webhooks from "@/pages/Webhooks";

import AutomationTokens from "@/pages/AutomationTokens";
import BackupRestore from "@/pages/BackupRestore";
import ConnectionLogs from "@/pages/ConnectionLogs";
import ServiceDetail from "@/pages/ServiceDetail";
import Retention from "@/pages/Retention";
import DatabaseBackend from "@/pages/DatabaseBackend";
import OpenApiViewer from "@/pages/OpenApiViewer";
import GatewayOverview from "@/pages/GatewayOverview";
import { RedirectTo } from "@/lib/redirects";
import { NOT_YET_MOVED, OLD_ROUTES } from "@/lib/moved-routes";

// Custom domains need host routing, which is off; old links land on the service.
function ServiceDomainsRedirect() {
  const { id } = useParams<{ id: string }>();
  return <Navigate to={`/services/${id}`} replace />;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<RequireAuth><Layout /></RequireAuth>}>
        <Route path="/" element={<Home />} />
        <Route path="/tunnels" element={<Tunnels />} />
        <Route path="/services" element={<Services />} />
        {/* /ai/ is the gateway's data plane and never reaches the SPA; its pages live under /gateway/. */}
        <Route path="/gateway" element={<GatewayOverview />} />
        <Route path="/gateway/providers" element={<Providers />} />
        <Route path="/gateway/providers/:slug" element={<ProviderDetail />} />
        <Route path="/gateway/cache" element={<PromptCache />} />
        <Route path="/gateway/guardrails" element={<Guardrails />} />
        <Route path="/gateway/requests" element={<InspectorIndex />} />
        <Route path="/gateway/requests/:serviceId/:requestId?" element={<RequestInspector />} />
        <Route path="/gateway/cost" element={<CostBudgets />} />
        <Route path="/audit" element={<AuditLog />} />
        <Route path="/webhooks" element={<Webhooks />} />

        <Route path="/account/automation" element={<AutomationTokens />} />
        <Route path="/settings/backups" element={<BackupRestore />} />
        <Route path="/tokens" element={<Tokens />} />
        <Route path="/clients" element={<Clients />} />
        <Route path="/clients/connect" element={<ConnectClient />} />
        <Route path="/clients/:id" element={<ClientDetail />} />
        <Route path="/account" element={<Account />} />
        <Route path="/users" element={<Users />} />
        <Route path="/roles" element={<Roles />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="/connection-logs" element={<ConnectionLogs />} />
        <Route path="/services/:id" element={<ServiceDetail />} />
        <Route path="/services/:id/domains" element={<ServiceDomainsRedirect />} />
        <Route path="/settings/retention" element={<Retention />} />
        <Route path="/settings/database" element={<DatabaseBackend />} />
        {/* P1-14: in-app OpenAPI viewer, framed inside the dashboard chrome. */}
        <Route path="/openapi" element={<OpenApiViewer />} />
        <Route path="/settings/custom-domains" element={<Navigate to="/settings" replace />} />
        {/* Old bookmarks: every moved path lands on its new home, params, query and hash intact. */}
        {OLD_ROUTES.map((r) => <Route key={r.from} path={r.from} element={<RedirectTo to={r.to} />} />)}
        {/* Temporary (W03, W05): navigation entries whose page still lives at its old address. */}
        {NOT_YET_MOVED.map((r) => <Route key={r.from} path={r.from} element={<RedirectTo to={r.to} />} />)}
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
