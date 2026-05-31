import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, Copy } from "lucide-react";
import { apiFetch } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { Button, Input, Dialog } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { useAuth } from "@/auth/useAuth";
import type { ServiceApiKey, CreatedApiKey } from "@/lib/contract";

export function ApiKeysPanel({ serviceId }: { serviceId: string }) {
  const { user } = useAuth();
  const qc = useQueryClient();
  const keysKey = ["service-api-keys", serviceId];
  const { data } = useQuery({
    queryKey: keysKey,
    queryFn: () => apiFetch<ServiceApiKey[]>(`/services/${serviceId}/api-keys`),
    staleTime: 30_000,
  });

  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  // Plaintext lives only in local state — never the query cache (shown once).
  const [plaintext, setPlaintext] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<ServiceApiKey | null>(null);

  const create = useMutation({
    mutationFn: () =>
      apiFetch<CreatedApiKey>(`/services/${serviceId}/api-keys`, {
        method: "POST",
        body: JSON.stringify({ name }),
      }),
    onSuccess: (r) => {
      setPlaintext(r.key);
      setName("");
      setCreating(false);
      qc.invalidateQueries({ queryKey: keysKey });
    },
    onError: () => toast.error("Couldn't create the API key."),
  });

  const revoke = useMutation({
    mutationFn: (id: string) =>
      apiFetch(`/services/${serviceId}/api-keys/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setRevokeTarget(null);
      qc.invalidateQueries({ queryKey: keysKey });
    },
    onError: () => toast.error("Couldn't revoke the API key."),
  });

  return (
    <div className="api-keys-panel">
      <div className="panel-head">
        <Button variant="primary" size="sm" className="ml-auto" onClick={() => setCreating(true)}>Create key</Button>
      </div>

      <div className="table-wrap">
        <table className="data" aria-label="API keys">
          <thead>
            <tr>
              <th>Name</th>
              <th>Created</th>
              <th>Last used</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {(data ?? []).map((k) => (
              <tr key={k.id}>
                <td className="col-name">{k.name}</td>
                <td className="col-created">{formatTimestamp(k.created_at)}</td>
                <td className={k.last_used ? "col-lastused" : "col-lastused null"}>
                  {k.last_used ? formatTimestamp(k.last_used) : "Never"}
                </td>
                <td className="col-actions">
                  <Button
                    variant="secondary"
                    size="sm"
                    aria-label={`Revoke key ${k.name}`}
                    onClick={() => setRevokeTarget(k)}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
            {(data ?? []).length === 0 && (
              <tr><td colSpan={4} className="muted">No keys yet.</td></tr>
            )}
          </tbody>
        </table>
      </div>

      {/* P1-5 — only show the permission hint to non-admin users. Admins
          already have services:configure; the hint just adds noise.
          Wait for useAuth to resolve (user !== undefined) so the message
          doesn't flash for admins on first render. */}
      {user !== undefined && user.role !== "admin" && (
        <p className="muted">Managing keys needs the services:configure permission.</p>
      )}

      <Dialog
        open={creating}
        onOpenChange={(o) => { if (!o) { setCreating(false); setName(""); } }}
        title="Create API key"
        description="Name it so you can recognise it later (e.g. ci, prod)."
        footer={
          <>
            <Button variant="secondary" onClick={() => { setCreating(false); setName(""); }}>Cancel</Button>
            <Button variant="primary" disabled={!name || create.isPending} onClick={() => name && create.mutate()}>
              {create.isPending ? "Creating…" : "Create"}
            </Button>
          </>
        }
      >
        <div className="field">
          <label htmlFor="api-key-name">Key name</label>
          <Input
            id="api-key-name"
            placeholder="e.g. ci"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter" && name) create.mutate(); }}
          />
        </div>
      </Dialog>

      <Dialog
        open={plaintext !== null}
        onOpenChange={(o) => { if (!o) setPlaintext(null); }}
        title="Copy your API key now"
        description="This is shown once. Send it as the configured API-key header."
        footer={<Button variant="primary" onClick={() => setPlaintext(null)}>Done</Button>}
      >
        <div className="reveal-once" role="status" aria-live="polite">
          <div className="top">
            <span className="icon"><Check size={14} strokeWidth={2.5} /></span>
            <strong>Key minted.</strong>
            <span className="muted small">
              Store it now — you won't see it again.
            </span>
          </div>
          <div className="key-row">
            <span className="v mono">{plaintext}</span>
            <button
              type="button"
              className="icon-btn"
              aria-label="Copy API key"
              onClick={() => { if (plaintext) void navigator.clipboard?.writeText(plaintext); }}
            >
              <Copy size={13} />
            </button>
          </div>
        </div>
      </Dialog>

      <Dialog
        open={revokeTarget !== null}
        onOpenChange={(o) => { if (!o) setRevokeTarget(null); }}
        title="Revoke API key"
        description={revokeTarget
          ? `“${revokeTarget.name}” will stop working immediately. This cannot be undone.`
          : ""}
        footer={
          <>
            <Button variant="secondary" onClick={() => setRevokeTarget(null)}>Cancel</Button>
            <Button
              variant="primary"
              disabled={revoke.isPending}
              onClick={() => revokeTarget && revoke.mutate(revokeTarget.id)}
            >
              {revoke.isPending ? "Revoking…" : "Revoke"}
            </Button>
          </>
        }
      >
        <p className="muted">Clients using this key will start receiving 401 responses.</p>
      </Dialog>

      <Toaster />
    </div>
  );
}
