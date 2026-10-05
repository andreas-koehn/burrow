import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { Button, PageHeader } from "@/components/ds";
import type { Session } from "@/lib/contract";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { parseUserAgent } from "@/lib/userAgent";

function ActiveSessions() {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({ queryKey: ["sessions"], queryFn: () => apiFetch<Session[]>("/sessions"), retry: false });
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch(`/sessions/${id}`, { method: "DELETE" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["sessions"] }); toast.success("Session revoked"); },
  });
  const revokeAll = useMutation({
    mutationFn: () => apiFetch("/sessions/revoke-all", { method: "POST" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["sessions"] }); toast.success("Signed out everywhere"); },
  });
  return (
    <section className="account-section" aria-labelledby="sec-sessions">
      <div className="section-head">
        <div className="left"><h2 id="sec-sessions">Active sessions</h2></div>
        <Button variant="secondary" size="sm" onClick={() => revokeAll.mutate()}>Sign out everywhere</Button>
      </div>
      {isLoading ? <p className="muted">Loading…</p> : (
        <div className="table-wrap">
          <table className="data" aria-label="Active sessions">
            <thead><tr><th>Device</th><th>Created</th><th>Expires</th><th>IP</th><th className="col-actions">Actions</th></tr></thead>
            <tbody>
              {(data ?? []).map((s) => {
                const ua = parseUserAgent(s.user_agent ?? "");
                return (
                <tr key={s.id}>
                  <td>
                    <span title={s.user_agent}>{ua.browser} · {ua.os}</span>
                    {s.current && <span className="tag"> THIS DEVICE</span>}
                  </td>
                  <td className="col-created">{formatTimestamp(s.created_at)}</td>
                  <td className="col-created">{formatTimestamp(s.expires_at)}</td>
                  <td className="col-created">{s.ip}</td>
                  <td className="col-actions">
                    {s.current ? <span className="muted">Current session</span> :
                      <Button variant="destructive" size="sm" onClick={() => revoke.mutate(s.id)}>Revoke</Button>}
                  </td>
                </tr>
              );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function Sessions() {
  return (
    <div className="account-page">
      <PageHeader title="Sessions" />
      <ActiveSessions />
      <Toaster />
    </div>
  );
}
