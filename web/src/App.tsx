import { Routes, Route, Navigate } from "react-router-dom";
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
import AiEndpoints from "@/pages/AiEndpoints";
import AiEndpointDetail from "@/pages/AiEndpointDetail";
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
import CustomDomainsOverview from "@/pages/CustomDomainsOverview";

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<RequireAuth><Layout /></RequireAuth>}>
        <Route path="/" element={<Home />} />
        <Route path="/tunnels" element={<Tunnels />} />
        <Route path="/services" element={<Services />} />
        <Route path="/ai/endpoints" element={<AiEndpoints />} />
        <Route path="/ai/endpoints/:id" element={<AiEndpointDetail />} />
        <Route path="/cache" element={<PromptCache />} />
        <Route path="/guardrails" element={<Guardrails />} />
        <Route path="/inspector" element={<InspectorIndex />} />
        <Route path="/inspector/:serviceId/:requestId?" element={<RequestInspector />} />
        <Route path="/cost" element={<CostBudgets />} />
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
        <Route path="/services/:id/domains" element={<ServiceDetail />} />
        <Route path="/settings/retention" element={<Retention />} />
        <Route path="/settings/database" element={<DatabaseBackend />} />
        {/* P1-14: in-app OpenAPI viewer, framed inside the dashboard chrome. */}
        <Route path="/openapi" element={<OpenApiViewer />} />
        <Route path="/settings/custom-domains" element={<CustomDomainsOverview />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
