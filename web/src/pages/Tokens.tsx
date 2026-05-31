import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, Copy } from "lucide-react";
import { apiFetch } from "@/lib/api";
import { formatTimestamp, formatRelativeTime } from "@/lib/format";
import { Button, FormField, FormFieldGroup, Input, Dialog, PageHeader } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

interface Token { id: string; name: string; last_used: string | null; created_at: string; }

export default function Tokens() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["tokens"], queryFn: () => apiFetch<Token[]>("/tokens"), staleTime: 30_000 });
  const [name, setName] = useState("");
  const [plaintext, setPlaintext] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<{ id: string; name: string } | null>(null);
  const create = useMutation({
    mutationFn: () => apiFetch<{ name: string; token: string }>("/tokens", { method: "POST", body: JSON.stringify({ name }) }),
    onSuccess: (r) => { setPlaintext(r.token); setName(""); qc.invalidateQueries({ queryKey: ["tokens"] }); },
    onError: () => toast.error("Failed to create token"),
  });
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch(`/tokens/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tokens"] }),
    onError: () => toast.error("Failed to revoke token"),
  });
  return (
    <div className="tokens-page">
      <PageHeader title="Client tokens" />

      <form
        className="tokens-form"
        onSubmit={(e) => { e.preventDefault(); if (name) create.mutate(); }}
      >
        <FormFieldGroup>
          <FormField label="Token name" htmlFor="token-name" w="md">
            <Input id="token-name" placeholder="e.g. laptop" value={name} onChange={(e) => setName(e.target.value)} />
          </FormField>
        </FormFieldGroup>
        <div className="actions">
          <Button type="submit" variant="primary" disabled={!name || create.isPending}>Create</Button>
        </div>
      </form>

      <div className="table-wrap">
        <table className="data" aria-label="Tokens">
          <thead>
            <tr>
              <th>Name</th>
              <th>Created</th>
              <th>Last used</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {(data ?? []).map((t) => (
              <tr key={t.id}>
                <td className="col-name">{t.name}</td>
                <td className="col-created">{formatTimestamp(t.created_at)}</td>
                <td
                  className={t.last_used ? "col-lastused" : "col-lastused null"}
                  // P1-13 — relative time as the primary value, absolute
                  // timestamp on hover so power users keep the precision.
                  title={t.last_used ? formatTimestamp(t.last_used) : undefined}
                >
                  {t.last_used ? formatRelativeTime(t.last_used) : "never"}
                </td>
                <td className="col-actions">
                  <Button
                    variant="destructive"
                    size="sm"
                    aria-label={`Revoke token ${t.name}`}
                    onClick={() => setRevokeTarget({ id: t.id, name: t.name })}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <Dialog
        open={plaintext !== null}
        onOpenChange={(o) => !o && setPlaintext(null)}
        title="Copy your token now"
        description={<>This is shown once. Use it with <code>burrow connect --token …</code></>}
        footer={<Button variant="primary" onClick={() => setPlaintext(null)}>Done</Button>}
      >
        <div className="reveal-once" role="status" aria-live="polite">
          <div className="top">
            <span className="icon"><Check size={14} strokeWidth={2.5} /></span>
            <strong>Token minted.</strong>
            <span className="muted small">
              Store it before closing this dialog.
            </span>
          </div>
          <div className="key-row">
            <span className="v mono">{plaintext}</span>
            <button
              type="button"
              className="icon-btn"
              aria-label="Copy token"
              onClick={() => { if (plaintext) { void navigator.clipboard?.writeText(plaintext); toast.success("Copied."); } }}
            >
              <Copy size={13} />
            </button>
          </div>
        </div>
      </Dialog>
      <Dialog
        open={revokeTarget !== null}
        onOpenChange={(o) => { if (!o) setRevokeTarget(null); }}
        title="Revoke token?"
        footer={
          <>
            <Button variant="secondary" onClick={() => setRevokeTarget(null)}>Cancel</Button>
            <Button
              variant="destructive"
              disabled={revoke.isPending}
              onClick={() => {
                if (revokeTarget) {
                  revoke.mutate(revokeTarget.id);
                  setRevokeTarget(null);
                }
              }}
            >
              Revoke
            </Button>
          </>
        }
      >
        <p>
          Revoke <code className="mono">{revokeTarget?.name}</code>?
          Clients using this token will be disconnected on the next reconnect.
        </p>
      </Dialog>
      <Toaster />
    </div>
  );
}
