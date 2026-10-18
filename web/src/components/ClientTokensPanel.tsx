import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, Copy } from "lucide-react";
import { Link } from "react-router-dom";
import { apiFetch } from "@/lib/api";
import { formatDate } from "@/lib/format";
import { RelativeTime } from "@/components/RelativeTime";
import { Button, FormField, FormFieldGroup, Input, Select, Dialog, TableEmptyRow } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

interface Token { id: string; name: string; last_used: string | null; created_at: string; /** null when the token never expires. */ expires_at?: string | null; }

// The relay accepts an expiry up to 365 days ahead.
const EXPIRY_CHOICES = [
  { value: "0", label: "Never" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "365 days" },
];
const DAY_MS = 86_400_000;
// Stay 5 minutes inside the limit so a relay clock slightly behind ours does not refuse it.
const EXPIRY_MARGIN_MS = 5 * 60_000;

const isExpired = (t: Token) => !!t.expires_at && Date.parse(t.expires_at) <= Date.now();

/** The Tokens tab of Clients: create, show once and revoke the tokens a client connects with. */
export function ClientTokensPanel() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["tokens"], queryFn: () => apiFetch<Token[]>("/tokens"), staleTime: 30_000 });
  const [name, setName] = useState("");
  const [days, setDays] = useState("0");
  const [plaintext, setPlaintext] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<{ id: string; name: string } | null>(null);
  const create = useMutation({
    mutationFn: () => apiFetch<{ name: string; token: string }>("/tokens", { method: "POST", body: JSON.stringify(Number(days) > 0 ? { name, expires_at: new Date(Date.now() + Number(days) * DAY_MS - EXPIRY_MARGIN_MS).toISOString() } : { name }) }),
    onSuccess: (r) => { setPlaintext(r.token); setName(""); setDays("0"); qc.invalidateQueries({ queryKey: ["tokens"] }); },
    onError: (e: Error) => toast.error(e.message ? `Failed to create token: ${e.message}` : "Failed to create token"),
  });
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch(`/tokens/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tokens"] }),
    onError: () => toast.error("Failed to revoke token"),
  });
  return (
    <div className="tokens-page">
      <p className="muted small">
        A client token lets a machine running <code>burrow</code> connect to this relay. Apps that call a model use a{" "}
        <Link className="link-inline" to="/gateway/keys">gateway key</Link>; apps that call one service use that{" "}
        <Link className="link-inline" to="/services">service</Link>'s API key; scripts use an{" "}
        <Link className="link-inline" to="/settings/automation">automation token</Link>.
      </p>

      <form
        className="tokens-form"
        onSubmit={(e) => { e.preventDefault(); if (name) create.mutate(); }}
      >
        <FormFieldGroup>
          <FormField label="Token name" htmlFor="token-name" w="md">
            <Input id="token-name" placeholder="e.g. laptop" value={name} onChange={(e) => setName(e.target.value)} />
          </FormField>
          <FormField label="Expires" htmlFor="token-expiry" w="sm">
            <Select id="token-expiry" options={EXPIRY_CHOICES} value={days} onChange={setDays} />
          </FormField>
        </FormFieldGroup>
        <div className="actions">
          <Button type="submit" variant="primary" disabled={!name || create.isPending}>Create</Button>
        </div>
      </form>

      <p className="muted small">This list includes tokens minted here and via Connect a client.</p>

      <div className="table-wrap">
        <table className="data" aria-label="Tokens">
          <thead>
            <tr>
              <th>Name</th>
              <th>Created</th>
              <th>Last used</th>
              <th>Expires</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {data && data.length === 0
              ? <TableEmptyRow colSpan={5} title="No tokens yet.">Create one above, or use Connect a client.</TableEmptyRow>
              : (data ?? []).map((t) => (
              <tr key={t.id} className={isExpired(t) ? "is-expired" : undefined}>
                <td className="col-name">{t.name}</td>
                <td className="col-created"><RelativeTime value={t.created_at} /></td>
                <td className={t.last_used ? "col-lastused" : "col-lastused null"}>
                  <RelativeTime value={t.last_used} empty="never" />
                </td>
                <td className="col-expires">
                  {t.expires_at ? `${isExpired(t) ? "Expired" : "Expires"} ${formatDate(t.expires_at)}` : "Never"}
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
