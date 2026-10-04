import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Link } from "react-router-dom";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, EmptyState, FormField, FormFieldGroup, Input, PageHeader, Select, SkeletonRows } from "@/components/ds";
import type { AutomationToken, CreatedAutomationToken } from "@/lib/contract";

const EXPIRY_OPTIONS = [
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
  { value: "never", label: "Never" },
];

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
}

export default function AutomationTokens() {
  const qc = useQueryClient();
  const tokens = useQuery({
    queryKey: ["automation", "tokens"],
    queryFn: () => apiFetch<AutomationToken[]>("/automation/tokens"),
    retry: false,
  });

  const [addOpen, setAddOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("never");
  const [created, setCreated] = useState<CreatedAutomationToken | null>(null);

  const mint = useMutation({
    mutationFn: () =>
      apiFetch<CreatedAutomationToken>("/automation/tokens", {
        method: "POST",
        body: JSON.stringify({ name, expires_at: expiry === "never" ? null : expiry }),
      }),
    onSuccess: (res) => {
      setCreated(res);
      qc.invalidateQueries({ queryKey: ["automation", "tokens"] });
      setAddOpen(false);
      setName("");
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't mint token."),
  });
  const revoke = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/automation/tokens/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["automation", "tokens"] }),
  });

  return (
    <div className="automation-page">
      <PageHeader
        title="Automation tokens"
        subtitle="Long-lived bearer tokens for CI / CLI / bots — scoped to your own permissions."
        actions={<Button variant="primary" size="sm" onClick={() => setAddOpen(true)}>New token</Button>}
      />

      <p className="muted small">These authenticate machines and scripts — not tunneling agents. To connect a machine that runs burrow, use <Link className="link-inline" to="/tokens">Client tokens</Link>.</p>

      {!tokens.data ? (
        <SkeletonRows n={2} />
      ) : tokens.data.length === 0 ? (
        <EmptyState title="No automation tokens yet">
          Mint a token to let CI jobs, the CLI, or bots authenticate on your behalf — without sharing your password.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Automation tokens">
            <thead>
              <tr><th>Name</th><th>Prefix</th><th>Role at mint</th><th>Permissions</th><th>Expires</th><th>Last used</th><th className="col-actions"></th></tr>
            </thead>
            <tbody>
              {tokens.data.map((t) => (
                <tr key={t.id}>
                  <td>{t.name}</td>
                  <td className="mono small">{t.prefix}</td>
                  <td>{t.role_at_mint}</td>
                  <td className="mono small">{t.permissions.length} grant(s)</td>
                  <td className="mono small">{t.expires_at ?? "never"}</td>
                  <td className="mono small">{t.last_used ?? "—"}</td>
                  <td className="col-actions">
                    <Button variant="destructive" size="sm" onClick={() => revoke.mutate(t.id)}>Revoke</Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={addOpen}
        onOpenChange={setAddOpen}
        title="New automation token"
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={mint.isPending} onClick={() => mint.mutate()}>Create</Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField label="Name" htmlFor="at-name" w="full">
            <Input id="at-name" value={name} onChange={(e) => setName(e.target.value)} />
          </FormField>
          <FormField label="Expiry" htmlFor="at-expiry" w="md">
            <Select id="at-expiry" value={expiry} onChange={setExpiry} options={EXPIRY_OPTIONS} />
          </FormField>
        </FormFieldGroup>
        <p className="muted">
          The token will inherit your current permissions; the server enforces this too.
        </p>
      </Dialog>

      <Dialog
        open={created !== null}
        onOpenChange={(o) => { if (!o) setCreated(null); }}
        title="New automation token"
        footer={<Button variant="primary" onClick={() => setCreated(null)}>I've saved it</Button>}
      >
        <p>Save this token now — you won't see it again.</p>
        <div className="row row-center gap-2">
          <code className="mono small">{created?.plaintext}</code>
          <button type="button" className="icon-btn" aria-label="Copy automation token" onClick={() => { if (created) { copy(created.plaintext); toast.success("Copied."); } }}>
            <Copy size={13} />
          </button>
        </div>
      </Dialog>
      <Toaster />
    </div>
  );
}
