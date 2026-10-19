import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plug } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, EmptyState, ErrorNotice, PageHeader, SkeletonRows, Switch } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import { Toaster } from "@/components/ui/sonner";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { grantAccess, resourceLabel } from "@/lib/oauth";
import type { OAuthGrant } from "@/lib/contract";

export default function ConnectedApps() {
  const qc = useQueryClient();
  // ?all=1 and revoking another user's grant: oauth:clients:manage.
  const canAll = useCan(P.oauthClientsManage);
  const [all, setAll] = useState(false);
  const everyone = canAll && all;
  const [revoking, setRevoking] = useState<OAuthGrant | null>(null);
  const [revokeErr, setRevokeErr] = useState<string | null>(null);

  const grants = useQuery({
    queryKey: ["oauth", "grants", everyone],
    queryFn: () => apiFetch<OAuthGrant[]>(`/oauth/grants${everyone ? "?all=1" : ""}`),
    retry: false,
  });
  const list = grants.data ?? [];

  const revoke = useMutation({
    mutationFn: (g: OAuthGrant) => apiFetch(`/oauth/grants/${encodeURIComponent(g.id)}`, { method: "DELETE" }),
    onSuccess: (_d, g) => {
      void qc.invalidateQueries({ queryKey: ["oauth", "grants"] });
      setRevoking(null);
      toast.success(`${g.client_name} can no longer connect`);
    },
    onError: (e) => setRevokeErr(e instanceof ApiError ? e.message : "Couldn't revoke the app."),
  });

  return (
    <div className="account-page settings-page">
      <PageHeader
        title="Connected apps"
        subtitle="Apps that sign in to this relay through OAuth, such as an MCP client. Revoking one stops it at once."
        actions={canAll ? (
          <label htmlFor="apps-all" className="checkbox-row">
            <Switch id="apps-all" aria-label="All users" checked={all} onChange={setAll} />
            <span>All users</span>
          </label>
        ) : undefined}
      />

      {grants.error ? (
        <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void grants.refetch()}>Retry</Button>}>
          Couldn&apos;t load connected apps: {grants.error instanceof ApiError ? grants.error.message : "Unknown error"}
        </ErrorNotice>
      ) : grants.isLoading ? (
        <div className="table-wrap skel-pad"><SkeletonRows n={3} /></div>
      ) : list.length === 0 ? (
        <EmptyState icon={<Plug size={18} />} title="No connected apps">
          {everyone
            ? "No app is connected to this relay."
            : "When you approve an app that asks to use your account, it is listed here."}
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Connected apps">
            <thead>
              <tr>
                <th>App</th>
                {everyone && <th>User</th>}
                <th>Reaches</th>
                <th>May</th>
                <th>Connected</th>
                <th>Last used</th>
                <th className="col-actions">Actions</th>
              </tr>
            </thead>
            <tbody>
              {list.map((g) => (
                <tr key={g.id}>
                  <td className="col-name">{g.client_name}</td>
                  {everyone && <td>{g.user_email}</td>}
                  <td>{resourceLabel(g.resource)}</td>
                  <td>{grantAccess(g)}</td>
                  <td className="col-created"><RelativeTime value={g.created_at} /></td>
                  <td className="col-created">{g.last_used ? <RelativeTime value={g.last_used} /> : <span className="muted">Never</span>}</td>
                  <td className="col-actions">
                    <Button
                      variant="destructive"
                      size="sm"
                      aria-label={`Revoke ${g.client_name}`}
                      onClick={() => { setRevokeErr(null); setRevoking(g); }}
                    >
                      Revoke
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={revoking !== null}
        onOpenChange={(o) => { if (!revoke.isPending && !o) setRevoking(null); }}
        title={`Revoke ${revoking?.client_name ?? ""}`}
        description="The app is signed out of this relay at once and has to ask for access again."
        footer={
          <>
            <Button variant="secondary" disabled={revoke.isPending} onClick={() => setRevoking(null)}>Cancel</Button>
            <Button variant="destructive" disabled={revoke.isPending} onClick={() => revoking && revoke.mutate(revoking)}>
              {revoke.isPending ? "Revoking…" : "Revoke"}
            </Button>
          </>
        }
      >
        {revoking && everyone && revoking.user_id !== "" && (
          <p className="muted">This connection belongs to {revoking.user_email}.</p>
        )}
        {revokeErr && <ErrorNotice>{revokeErr}</ErrorNotice>}
      </Dialog>
      <Toaster />
    </div>
  );
}
