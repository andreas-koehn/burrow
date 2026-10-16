import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { apiFetch, ApiError } from "@/lib/api";
import { P } from "@/auth/permissions";
import { useCan } from "@/auth/useCan";
import { CopyableCode } from "@/components/CopyableCode";
import { RelativeTime } from "@/components/RelativeTime";
import { Badge, ErrorNotice, PageHeader, SkeletonRows, Tabs } from "@/components/ds";
import { mcpSnippets } from "@/lib/mcpSnippets";
import { firstSentence } from "@/lib/firstSentence";
import type { AuditEvent, McpStatus, McpTool } from "@/lib/contract";

const KIND_WORD: Record<McpTool["kind"], string> = { read: "Read", write: "Write", delete: "Delete" };
const RESULT_WORD: Record<AuditEvent["result"], string> = { ok: "ok", denied: "denied", error: "error", dry_run: "dry run" };
const RESULT_BADGE: Record<AuditEvent["result"], string> = {
  ok: "status-connected",
  denied: "status-idle",
  error: "status-suspended",
  dry_run: "status-idle",
};

function message(e: unknown): string {
  return e instanceof ApiError ? e.message : "Unknown error";
}

function ToolsTable({ tools, operations }: { tools: McpTool[]; operations?: number }) {
  return (
    <div className="stack-md">
      <div className="table-wrap">
        <table className="data" aria-label="MCP tools">
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">What it does</th>
              <th scope="col">Kind</th>
              <th scope="col">Needs</th>
            </tr>
          </thead>
          <tbody>
            {tools.map((t) => (
              <tr key={t.name}>
                <td className="mono">{t.name}</td>
                <td className="cell-wrap">{firstSentence(t.description)}</td>
                <td>{KIND_WORD[t.kind] ?? t.kind}</td>
                <td className="mono cell-wrap">{t.permissions?.length ? t.permissions.join(" or ") : "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {operations !== undefined && (
        <p className="muted small">
          The three tools at the end reach {operations} operations in all; each token sees only those it may call.
        </p>
      )}
    </div>
  );
}

function RecentCalls() {
  const calls = useQuery({
    queryKey: ["audit", "mcp-calls"],
    queryFn: () => apiFetch<AuditEvent[]>("/audit/events?action=mcp.tool.call&limit=10"),
    retry: false,
  });
  return (
    <section className="account-section" aria-labelledby="mcp-calls">
      <div className="section-head"><div className="left"><h2 id="mcp-calls">Recent calls</h2></div></div>
      {calls.error ? (
        <ErrorNotice>Couldn't load the recent calls: {message(calls.error)}</ErrorNotice>
      ) : !calls.data ? (
        <SkeletonRows n={2} />
      ) : calls.data.length === 0 ? (
        <p className="muted">No calls yet.</p>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Recent MCP calls">
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Who</th>
                <th scope="col">Tool</th>
                <th scope="col">Result</th>
              </tr>
            </thead>
            <tbody>
              {calls.data.map((e) => (
                <tr key={e.id}>
                  <td><RelativeTime value={e.ts} /></td>
                  <td>{e.actor_email}</td>
                  <td className="mono">{e.subject_label}</td>
                  <td><Badge kind={RESULT_BADGE[e.result]}>{RESULT_WORD[e.result] ?? e.result}</Badge></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function McpSettings() {
  const seesCalls = useCan(P.auditRead);
  const makesTokens = useCan([P.automationTokensManageOwn, P.automationTokensManageAny]);
  const status = useQuery({ queryKey: ["mcp", "status"], queryFn: () => apiFetch<McpStatus>("/mcp/status"), retry: false });
  const tools = useQuery({ queryKey: ["mcp", "tools"], queryFn: () => apiFetch<McpTool[]>("/mcp/tools"), retry: false });
  const [tab, setTab] = useState("claude-code");

  // Until the status is known, or when it failed, the path is the one every relay serves.
  const url = window.location.origin + (status.data?.endpoint ?? "/mcp/burrow");
  const snippets = mcpSnippets(url);

  return (
    <div className="settings-page">
      <PageHeader
        title="MCP"
        subtitle="Let an agent manage this relay. An MCP client such as Claude Code, Cursor or VS Code connects to the address below with an automation token and gets the tools that token may use."
      />

      <section className="account-section" aria-labelledby="mcp-endpoint">
        <div className="section-head"><div className="left"><h2 id="mcp-endpoint">Address</h2></div></div>
        <CopyableCode text={url} label="Copy address" wrap />
      </section>

      <section className="account-section" aria-labelledby="mcp-connect">
        <div className="section-head"><div className="left"><h2 id="mcp-connect">Connect a client</h2></div></div>
        <div className="stack-md">
          <Tabs
            value={tab}
            onChange={setTab}
            tabs={[
              { value: "claude-code", label: "Claude Code", content: <CopyableCode text={snippets.claudeCode} label="Copy Claude Code command" wrap /> },
              {
                value: "cursor",
                label: "Cursor",
                content: (
                  <>
                    <p className="muted small mono">~/.cursor/mcp.json</p>
                    <CopyableCode text={snippets.cursor} label="Copy Cursor configuration" />
                  </>
                ),
              },
              {
                value: "vscode",
                label: "VS Code",
                content: (
                  <>
                    <p className="muted small mono">.vscode/mcp.json</p>
                    <CopyableCode text={snippets.vscode} label="Copy VS Code configuration" />
                  </>
                ),
              },
            ]}
          />
          <p className="muted small">
            Put your token into BURROW_TOKEN first.{" "}
            {makesTokens ? (
              <Link className="link-inline" to="/settings/automation?new=1&preset=read-only">Create a read-only token</Link>
            ) : (
              "Making one needs the permission automation:tokens:manage:own; ask an admin for a token."
            )}
          </p>
          {makesTokens && (
            <p className="muted small">A read-only token cannot list services or budgets; use an operator token for those.</p>
          )}
        </div>
      </section>

      <section className="account-section" aria-labelledby="mcp-tools">
        <div className="section-head"><div className="left"><h2 id="mcp-tools">Tools</h2></div></div>
        <div className="stack-md">
          {tools.error ? (
            <ErrorNotice>Couldn't load the tools: {message(tools.error)}</ErrorNotice>
          ) : !tools.data ? (
            <SkeletonRows n={3} />
          ) : (
            <ToolsTable tools={tools.data} operations={status.data?.operations} />
          )}
          {status.error && (
            <ErrorNotice>Couldn't load the MCP status, so the number of operations is missing: {message(status.error)}</ErrorNotice>
          )}
        </div>
      </section>

      {seesCalls && <RecentCalls />}
    </div>
  );
}
