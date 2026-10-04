import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, EmptyState, ErrorNotice, Input, PageHeader, Select, SkeletonRows } from "@/components/ds";
import type { ProvisioningKey, ProvisioningPending } from "@/lib/contract";

interface CreatedProvisioningKey {
  key: ProvisioningKey;
  plaintext: string;
}

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
}

const EXPIRY_OPTIONS = [
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
  { value: "7d", label: "7 days" },
  { value: "never", label: "Never" },
];
const SCOPE_OPTIONS = [
  { value: "single", label: "Single (one client)" },
  { value: "multi", label: "Multi (fleet)" },
];

export default function ProvisioningKeys() {
  const qc = useQueryClient();
  const keys = useQuery({
    queryKey: ["provisioning", "keys"],
    queryFn: () => apiFetch<ProvisioningKey[]>("/provisioning/keys"),
    retry: false,
  });
  const pending = useQuery({
    queryKey: ["provisioning", "pending"],
    queryFn: () => apiFetch<ProvisioningPending[]>("/provisioning/pending"),
    retry: false,
  });

  const [addOpen, setAddOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("never");
  const [scope, setScope] = useState<"single" | "multi">("multi");
  const [defaultRole, setDefaultRole] = useState("user");
  const [created, setCreated] = useState<CreatedProvisioningKey | null>(null);

  const mint = useMutation({
    mutationFn: () =>
      apiFetch<CreatedProvisioningKey>("/provisioning/keys", {
        method: "POST",
        body: JSON.stringify({ name, scope, default_role: defaultRole, expires_at: expiry === "never" ? null : expiry }),
      }),
    onSuccess: (res) => {
      setCreated(res);
      qc.invalidateQueries({ queryKey: ["provisioning", "keys"] });
      setAddOpen(false);
      setName("");
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't mint provisioning key."),
  });

  const approve = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/provisioning/pending/${id}/approve`, { method: "POST", body: "{}" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provisioning", "pending"] }),
  });
  const reject = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/provisioning/pending/${id}/reject`, { method: "POST", body: "{}" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provisioning", "pending"] }),
  });
  const remove = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/provisioning/keys/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provisioning", "keys"] }),
  });

  return (
    <div className="provisioning-page">
      <PageHeader
        title="Provisioning keys"
        subtitle="Mint long-lived keys that newly-launched clients use to enrol with this relay."
        actions={<Button variant="primary" size="sm" onClick={() => setAddOpen(true)}>New provisioning key</Button>}
      />

      <section className="card">
        <h2>Active provisioning keys</h2>
        {keys.isLoading ? (
          <SkeletonRows n={2} />
        ) : keys.isError ? (
          <ErrorNotice>Couldn't load provisioning keys.</ErrorNotice>
        ) : keys.data && keys.data.length === 0 ? (
          <EmptyState title="No provisioning keys yet">
            Mint long-lived keys that newly-launched clients use to enrol with this relay.
          </EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="data" aria-label="Active provisioning keys">
              <thead>
                <tr><th>Name</th><th>Prefix</th><th>Scope</th><th>Role</th><th>Created</th><th>Last used</th><th className="col-actions"></th></tr>
              </thead>
              <tbody>
                {(keys.data ?? []).map((k) => (
                  <tr key={k.id}>
                    <td>{k.name}</td>
                    <td className="mono small">{k.prefix}</td>
                    <td><Badge nodot kind={`scope-${k.scope}`}>{k.scope}</Badge></td>
                    <td>{k.default_role}</td>
                    <td className="mono small">{k.created_at}</td>
                    <td className="mono small">{k.last_used ?? "—"}</td>
                    <td className="col-actions">
                      <Button variant="destructive" size="sm" onClick={() => remove.mutate(k.id)}>Revoke</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="card">
        <h2>Pending approvals</h2>
        {pending.isLoading ? (
          <SkeletonRows n={2} />
        ) : pending.isError ? (
          <ErrorNotice>Couldn't load pending approvals.</ErrorNotice>
        ) : pending.data && pending.data.length === 0 ? (
          <EmptyState title="No pending approvals">
            Clients that present a provisioning key will appear here for your review.
          </EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="data" aria-label="Pending approvals">
              <thead>
                <tr><th>Hostname</th><th>OS / Arch</th><th>Remote IP</th><th>Provisioning key</th><th>First seen</th><th className="col-actions"></th></tr>
              </thead>
              <tbody>
                {(pending.data ?? []).map((p) => (
                  <tr key={p.id}>
                    <td className="mono">{p.hostname}</td>
                    <td className="mono small">{p.os}/{p.arch}</td>
                    <td className="mono small">{p.remote_ip}</td>
                    <td className="mono small">{p.provisioning_key_id}</td>
                    <td className="mono small">{p.first_seen}</td>
                    <td className="col-actions">
                      <Button variant="primary" size="sm" onClick={() => approve.mutate(p.id)}>Approve</Button>
                      {" "}
                      <Button variant="ghost" size="sm" onClick={() => reject.mutate(p.id)}>Reject</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <Dialog
        open={addOpen}
        onOpenChange={setAddOpen}
        title="New provisioning key"
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={mint.isPending} onClick={() => mint.mutate()}>
              Create
            </Button>
          </>
        }
      >
        <div className="field">
          <label htmlFor="pk-name">Name</label>
          <Input id="pk-name" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="pk-expiry">Expiry</label>
          <Select id="pk-expiry" value={expiry} onChange={setExpiry} options={EXPIRY_OPTIONS} />
        </div>
        <div className="field">
          <label htmlFor="pk-scope">Scope</label>
          <Select id="pk-scope" value={scope} onChange={(v) => setScope(v as "single" | "multi")} options={SCOPE_OPTIONS} />
        </div>
        <div className="field">
          <label htmlFor="pk-role">Default role</label>
          <Input id="pk-role" value={defaultRole} onChange={(e) => setDefaultRole(e.target.value)} />
        </div>
      </Dialog>

      <Dialog
        open={created !== null}
        onOpenChange={(o) => { if (!o) setCreated(null); }}
        title="New provisioning key"
        footer={<Button variant="primary" onClick={() => setCreated(null)}>I've saved it</Button>}
      >
        <p>Save this key now — you won't see it again.</p>
        <div className="row row-center gap-2">
          <code className="mono small">{created?.plaintext}</code>
          <button type="button" className="icon-btn" aria-label="Copy provisioning key" onClick={() => { if (created) { copy(created.plaintext); toast.success("Copied."); } }}>
            <Copy size={13} />
          </button>
        </div>
        <h4>Install on a new client</h4>
        <pre className="mono small">{`burrow connect --server <endpoint> --provisioning-key ${created?.plaintext ?? ""}`}</pre>
      </Dialog>
      <Toaster />
    </div>
  );
}
