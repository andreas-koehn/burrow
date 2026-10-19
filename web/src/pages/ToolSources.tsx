import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Wrench } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, EmptyState, ErrorNotice, PageHeader, SkeletonRows } from "@/components/ds";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { LocalToolSourceDialog } from "@/components/LocalToolSourceDialog";
import { NewRemoteToolSourceDialog } from "@/components/NewRemoteToolSourceDialog";
import { sourceStatus } from "@/lib/toolSource";
import type { ToolSource } from "@/lib/contract";

export default function ToolSources() {
  // POST /mcp/sources: mcp:sources:manage.
  const canManage = useCan(P.mcpSourcesManage);
  const [remoteOpen, setRemoteOpen] = useState(false);
  const [localOpen, setLocalOpen] = useState(false);
  const sources = useQuery({
    queryKey: ["mcp", "sources"],
    queryFn: () => apiFetch<ToolSource[]>("/mcp/sources"),
    retry: false,
  });
  const list = sources.data ?? [];

  const add = canManage ? (
    <>
      <Button variant="secondary" size="sm" onClick={() => setLocalOpen(true)}>Add local source</Button>
      <Button variant="primary" size="sm" onClick={() => setRemoteOpen(true)}>Add remote source</Button>
    </>
  ) : undefined;

  return (
    <div className="ai-endpoints-page">
      <PageHeader
        title="Tools"
        subtitle="Tool sources: MCP servers whose tools the gateway serves behind gateway keys, once you have approved them."
        actions={add}
      />
      <p className="muted small" style={{ marginBottom: "var(--space-3, 12px)" }}>
        A source is a server on the internet that the relay calls, or one on your own machine that a client publishes with{" "}
        <code>burrow mcp</code>. A tool is served only after you approve its definition, and stops being served when the server changes it.
      </p>

      {sources.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void sources.refetch()}>Retry</Button>}
        >
          Couldn't load tool sources: {sources.error instanceof ApiError ? sources.error.message : "Unknown error"}
        </ErrorNotice>
      ) : sources.isLoading ? (
        <div className="table-wrap skel-pad"><SkeletonRows n={3} /></div>
      ) : list.length === 0 ? (
        <EmptyState
          icon={<Wrench size={18} />}
          title="No tool sources yet"
          action={add}
        >
          {canManage
            ? "Add an MCP server on the internet, or one that runs on one of your machines."
            : "An admin can add an MCP server on the internet, or one that runs on one of their machines."}
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Tool sources">
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>Address</th>
                <th>Status</th>
                <th>Tools</th>
              </tr>
            </thead>
            <tbody>
              {list.map((s) => {
                const status = sourceStatus(s);
                return (
                  <tr key={s.slug}>
                    <td className="col-name">
                      <div><Link to={`/gateway/tools/${encodeURIComponent(s.slug)}`}>{s.name}</Link></div>
                      <div className="mono muted small">/mcp/{s.slug}</div>
                    </td>
                    <td><Badge kind={`type-${s.kind === "remote" ? "direct" : "tunnel"}`} nodot>{s.kind === "remote" ? "Remote" : "On a machine"}</Badge></td>
                    <td>
                      {s.kind === "remote"
                        ? <span className="mono service-url-path">{s.url}</span>
                        : <span className="muted">Served by a client</span>}
                    </td>
                    <td><Badge kind={status.kind}>{status.label}</Badge></td>
                    <td className="mono">{s.tool_count}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {canManage && <NewRemoteToolSourceDialog open={remoteOpen} onOpenChange={setRemoteOpen} />}
      {canManage && <LocalToolSourceDialog open={localOpen} onOpenChange={setLocalOpen} />}
    </div>
  );
}
