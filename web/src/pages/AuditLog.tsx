import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { apiFetch, downloadFile, ApiError } from "@/lib/api";
import { Badge, Button, Input, PageHeader, SkeletonRows } from "@/components/ds";
import type { AuditEvent } from "@/lib/contract";
import { formatTimestampWithTooltip } from "@/lib/format";

const RESULT_BADGE: Record<AuditEvent["result"], string> = {
  ok: "status-connected",
  denied: "status-idle",
  error: "status-suspended",
};

function Row({ e }: { e: AuditEvent }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <tr className="clickable" onClick={() => setOpen((o) => !o)}>
        <td>{open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}</td>
        {(() => {
          const t = formatTimestampWithTooltip(e.ts);
          return <td className="small"><time dateTime={t.iso} title={t.iso}>{t.display}</time></td>;
        })()}
        <td className="mono small">{e.actor_email}</td>
        <td className="mono">{e.action}</td>
        <td className="mono">{e.subject_label}</td>
        <td><Badge kind={RESULT_BADGE[e.result]}>{e.result}</Badge></td>
        <td className="mono small">{e.source_ip}</td>
      </tr>
      {open && (
        <tr>
          <td colSpan={7} className="audit-detail">
            <pre className="mono small">{JSON.stringify(e.payload, null, 2)}</pre>
            <p className="muted mono small">
              prev_hash: {e.prev_hash} · hash: {e.hash}
            </p>
          </td>
        </tr>
      )}
    </>
  );
}

export default function AuditLog() {
  const [q, setQ] = useState("");
  const events = useQuery({
    queryKey: ["audit", "events", q],
    queryFn: () =>
      apiFetch<AuditEvent[]>(`/audit/events?limit=200${q ? `&q=${encodeURIComponent(q)}` : ""}`),
    retry: false,
  });
  const verify = useMutation({
    mutationFn: () =>
      apiFetch<{ ok: boolean; first_id: string; last_id: string }>(
        "/audit/verify",
        { method: "POST", body: "{}" },
      ),
  });

  return (
    <div className="audit-page">
      <PageHeader
        title="Audit log"
        subtitle={<>Hash-chained — each entry includes the SHA-256 of the previous one. Click <strong>Verify chain</strong> to confirm integrity.</>}
        actions={<>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => { void downloadFile("/audit/export?format=ndjson", "audit-log.ndjson"); }}
          >
            Export
          </Button>
          <Button
            variant="secondary"
            size="sm"
            disabled={verify.isPending}
            onClick={() => verify.mutate()}
          >
            Verify chain
          </Button>
        </>}
      />

      {verify.data && (
        verify.data.ok ? (
          <p className="notice-inline ok" role="status">
            Chain valid from {verify.data.first_id} to {verify.data.last_id}.
          </p>
        ) : (
          <p className="notice-inline error" role="alert">
            Chain integrity check failed.
          </p>
        )
      )}
      {verify.error && (
        <p className="notice-inline error" role="alert">
          {verify.error instanceof ApiError ? verify.error.message : "Verify failed."}
        </p>
      )}

      <div className="toolbar-row">
        <Input
          type="search"
          role="searchbox"
          aria-label="Search audit events"
          placeholder="search actor / action / subject"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>

      {!events.data ? (
        <SkeletonRows n={6} />
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Audit events">
            <thead>
              <tr>
                <th aria-hidden="true"></th>
                <th>When</th>
                <th>Actor</th>
                <th>Action</th>
                <th>Subject</th>
                <th>Result</th>
                <th>Source IP</th>
              </tr>
            </thead>
            <tbody>
              {events.data.map((e) => <Row key={e.id} e={e} />)}
            </tbody>
          </table>
        </div>
      )}
      <Toaster />
    </div>
  );
}
