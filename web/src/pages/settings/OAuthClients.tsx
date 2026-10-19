import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, SkeletonRows } from "@/components/ds";
import { CopyButton } from "@/components/CopyableCode";
import { RelativeTime } from "@/components/RelativeTime";
import { Toaster } from "@/components/ui/sonner";
import type { OAuthClient } from "@/lib/contract";

const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback);

function AddClientDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [uris, setUris] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<OAuthClient | null>(null);
  const addresses = uris.split("\n").map((u) => u.trim()).filter((u) => u !== "");

  const create = useMutation({
    mutationFn: () => apiFetch<OAuthClient>("/oauth/clients", {
      method: "POST",
      body: JSON.stringify({ name: name.trim(), redirect_uris: addresses }),
    }),
    onSuccess: (c) => {
      void qc.invalidateQueries({ queryKey: ["oauth", "clients"] });
      setCreated(c);
    },
    onError: (e) => setError(errText(e, "Couldn't create the client.")),
  });

  function close(o: boolean) {
    if (create.isPending) return;
    onOpenChange(o);
    if (!o) { setName(""); setUris(""); setError(null); setCreated(null); }
  }

  if (created) {
    return (
      <Dialog
        open={open}
        onOpenChange={close}
        title="OAuth client created"
        description="Give the app this client ID. There is no secret."
        footer={<Button variant="primary" onClick={() => close(false)}>Done</Button>}
      >
        <p className="row row-center gap-2">
          <code className="mono">{created.client_id}</code>
          <CopyButton text={created.client_id} label="Copy client ID" />
        </p>
      </Dialog>
    );
  }

  return (
    <Dialog
      open={open}
      onOpenChange={close}
      title="Add OAuth client"
      description="For an MCP client that cannot publish its own client metadata. It is a public client: no secret."
      footer={
        <>
          <Button variant="secondary" disabled={create.isPending} onClick={() => close(false)}>Cancel</Button>
          <Button variant="primary" disabled={create.isPending || name.trim() === "" || addresses.length === 0} onClick={() => { setError(null); create.mutate(); }}>
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Name" htmlFor="oc-name" help="Shown to people on the page where they approve the app.">
          <Input id="oc-name" autoComplete="off" maxLength={120} value={name} onChange={(e) => setName(e.target.value)} />
        </FormField>
        <FormField
          label="Redirect addresses"
          htmlFor="oc-uris"
          help="One per line: https, http on a loopback address (any port matches), or an app scheme such as com.example.app:/cb."
        >
          <textarea
            id="oc-uris"
            className="input mono resizable"
            rows={3}
            spellCheck={false}
            value={uris}
            onChange={(e) => setUris(e.target.value)}
          />
        </FormField>
      </FormFieldGroup>
      {error && <ErrorNotice role="alert">{error}</ErrorNotice>}
    </Dialog>
  );
}

export default function OAuthClients() {
  const qc = useQueryClient();
  const [addOpen, setAddOpen] = useState(false);
  const [deleting, setDeleting] = useState<OAuthClient | null>(null);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const clients = useQuery({
    queryKey: ["oauth", "clients"],
    queryFn: () => apiFetch<OAuthClient[]>("/oauth/clients"),
    retry: false,
  });
  const list = clients.data ?? [];

  const remove = useMutation({
    mutationFn: (c: OAuthClient) => apiFetch(`/oauth/clients/${encodeURIComponent(c.client_id)}`, { method: "DELETE" }),
    onSuccess: (_d, c) => {
      void qc.invalidateQueries({ queryKey: ["oauth", "clients"] });
      void qc.invalidateQueries({ queryKey: ["oauth", "grants"] });
      setDeleting(null);
      toast.success(`${c.name} deleted`);
    },
    onError: (e) => setDeleteErr(errText(e, "Couldn't delete the client.")),
  });

  const add = <Button variant="primary" size="sm" onClick={() => setAddOpen(true)}>Add OAuth client</Button>;

  return (
    <div className="account-page settings-page">
      <PageHeader
        title="OAuth clients"
        subtitle="Apps registered by hand. Most MCP clients identify themselves and need no entry here."
        actions={add}
      />
      {clients.error ? (
        <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void clients.refetch()}>Retry</Button>}>
          Couldn&apos;t load OAuth clients: {errText(clients.error, "Unknown error")}
        </ErrorNotice>
      ) : clients.isLoading ? (
        <div className="table-wrap skel-pad"><SkeletonRows n={3} /></div>
      ) : list.length === 0 ? (
        <EmptyState icon={<KeyRound size={18} />} title="No OAuth clients" action={add}>
          Register an app by its name and the addresses it returns to.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="OAuth clients">
            <thead>
              <tr><th>Name</th><th>Client ID</th><th>Redirect addresses</th><th>Created</th><th className="col-actions">Actions</th></tr>
            </thead>
            <tbody>
              {list.map((c) => (
                <tr key={c.client_id}>
                  <td className="col-name">{c.name}</td>
                  <td>
                    <span className="row row-center gap-2">
                      <code className="mono">{c.client_id}</code>
                      <CopyButton text={c.client_id} label={`Copy client ID of ${c.name}`} />
                    </span>
                  </td>
                  <td>{c.redirect_uris.map((u) => <div key={u} className="mono">{u}</div>)}</td>
                  <td className="col-created"><RelativeTime value={c.created_at} /></td>
                  <td className="col-actions">
                    <Button variant="destructive" size="sm" aria-label={`Delete ${c.name}`} onClick={() => { setDeleteErr(null); setDeleting(c); }}>
                      Delete
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <AddClientDialog open={addOpen} onOpenChange={setAddOpen} />

      <Dialog
        open={deleting !== null}
        onOpenChange={(o) => { if (!remove.isPending && !o) setDeleting(null); }}
        title={`Delete OAuth client · ${deleting?.name ?? ""}`}
        description="This cannot be undone."
        footer={
          <>
            <Button variant="secondary" disabled={remove.isPending} onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => deleting && remove.mutate(deleting)}>
              {remove.isPending ? "Deleting…" : "Delete client"}
            </Button>
          </>
        }
      >
        <p className="muted">Every connected app that signed in through this client is revoked at once.</p>
        {deleteErr && <ErrorNotice>{deleteErr}</ErrorNotice>}
      </Dialog>
      <Toaster />
    </div>
  );
}
