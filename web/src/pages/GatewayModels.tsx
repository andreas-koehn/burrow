import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Layers, MoreHorizontal } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, DropdownMenu, EmptyState, ErrorNotice, PageHeader, SkeletonRows } from "@/components/ds";
import { ConnectCard } from "@/components/ConnectCard";
import { GatewayAddresses } from "@/components/GatewayAddresses";
import { ModelDialog } from "@/components/ModelDialog";
import { useAuth } from "@/auth/useAuth";
import type { AiModel, AiProvider, Dialect, GatewayInfo } from "@/lib/contract";

/** Where a model goes in one format: its first target, and how many stand behind it. */
function FormatCell({ model, dialect }: { model: AiModel; dialect: Dialect }) {
  const targets = model.targets.filter((t) => t.dialect === dialect);
  const first = targets[0];
  if (!first) return <span className="muted">not served</span>;
  return (
    <>
      <span className="mono">{`${first.provider}/${first.model}`}</span>
      {targets.length > 1 && <span className="muted small">{` +${targets.length - 1} more`}</span>}
    </>
  );
}

export default function GatewayModels() {
  const qc = useQueryClient();
  const { user } = useAuth();
  // The relay also lets a role with ai:configure:any write models; the session
  // only tells the role, so everyone but an admin gets the read-only page.
  const canWrite = user?.role === "admin";

  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  // Same key and fetch as the Providers page.
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const gateway = useQuery({
    queryKey: ["ai", "gateway"],
    queryFn: () => apiFetch<GatewayInfo>("/ai/gateway"),
    retry: false,
  });

  // undefined: closed. null: a new model.
  const [editing, setEditing] = useState<AiModel | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<AiModel | null>(null);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: (name: string) => apiFetch<void>(`/ai/models/${encodeURIComponent(name)}`, { method: "DELETE" }),
    onSuccess: async () => {
      setDeleting(null);
      toast.success("Model deleted.");
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["ai", "models"] }),
        qc.invalidateQueries({ queryKey: ["ai", "providers"] }),
      ]);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setDeleteErr("Couldn't delete the model.");
      else if (e.status === 403) setDeleteErr("You don't have permission to delete models.");
      else setDeleteErr(e.message);
    },
  });

  const list = Array.isArray(models.data) ? models.data : [];
  const endpoints = gateway.data?.endpoints ?? [];
  const noProviders = Array.isArray(providers.data) && providers.data.length === 0;
  const newModel = canWrite
    ? <Button variant="primary" size="sm" onClick={() => setEditing(null)}>New model</Button>
    : undefined;

  return (
    <div className="gateway-models-page">
      <PageHeader
        title="Models"
        subtitle="Names clients ask for, and where each one is sent."
        actions={newModel}
      />
      <GatewayAddresses endpoints={endpoints} />

      {models.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void models.refetch()}>Retry</Button>}
        >
          Couldn't load models:{" "}
          {models.error instanceof ApiError ? models.error.message : "Unknown error"}
        </ErrorNotice>
      ) : models.isLoading || providers.isLoading ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={3} />
        </div>
      ) : list.length === 0 && noProviders ? (
        <EmptyState
          icon={<Layers size={18} />}
          title="Add a provider first"
          action={<Link className="btn btn-primary btn-sm" to="/gateway/providers">Go to Providers</Link>}
        >
          A model sends its requests to a provider. Once there is one, give it a name clients can ask for.
        </EmptyState>
      ) : list.length === 0 ? (
        <EmptyState icon={<Layers size={18} />} title="No models yet" action={newModel}>
          {canWrite
            ? "A model is a name such as burrow-medium, and for each API format the provider and model it is sent to."
            : "An administrator can create one: a name such as burrow-medium, and where each API format sends it."}
        </EmptyState>
      ) : (
        <>
          <div className="table-wrap">
            <table className="data" aria-label="Models">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>OpenAI format</th>
                  <th>Anthropic format</th>
                  <th>Status</th>
                  {canWrite && <th className="col-actions" aria-label="Actions"></th>}
                </tr>
              </thead>
              <tbody>
                {list.map((m) => (
                  <tr key={m.name}>
                    <td className="col-name">
                      <div className="mono">{m.name}</div>
                      {m.description && <div className="muted small">{m.description}</div>}
                    </td>
                    <td><FormatCell model={m} dialect="openai" /></td>
                    <td><FormatCell model={m} dialect="anthropic" /></td>
                    <td>
                      <Badge kind={m.enabled ? "status-connected" : "status-offline"}>
                        {m.enabled ? "enabled" : "disabled"}
                      </Badge>
                    </td>
                    {canWrite && (
                      <td className="col-actions">
                        <DropdownMenu
                          trigger={
                            <button type="button" className="icon-btn" aria-label={`More actions for ${m.name}`}>
                              <MoreHorizontal size={14} aria-hidden="true" />
                            </button>
                          }
                          items={[
                            { label: "Edit", onSelect: () => setEditing(m) },
                            { label: "Delete", danger: true, onSelect: () => { setDeleteErr(null); setDeleting(m); } },
                          ]}
                        />
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <ConnectCard
            endpoints={endpoints}
            models={list.filter((m) => m.enabled).map((m) => ({ name: m.name, dialects: m.dialects }))}
          />
        </>
      )}

      {canWrite && (
        <ModelDialog
          open={editing !== undefined}
          onOpenChange={(o) => { if (!o) setEditing(undefined); }}
          model={editing ?? undefined}
        />
      )}

      {canWrite && (
        <Dialog
          open={deleting !== null}
          onOpenChange={(o) => { if (!o && !remove.isPending) setDeleting(null); }}
          title={`Delete ${deleting?.name ?? ""}?`}
          footer={
            <>
              <Button variant="secondary" disabled={remove.isPending} onClick={() => setDeleting(null)}>Cancel</Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => { if (deleting) remove.mutate(deleting.name); }}>
                {remove.isPending ? "Deleting…" : "Delete model"}
              </Button>
            </>
          }
        >
          <p className="muted">Clients that ask for this model will get an error.</p>
          <p className="muted small">Gateway keys that list it keep the name and are not changed.</p>
          {deleteErr && <ErrorNotice>{deleteErr}</ErrorNotice>}
        </Dialog>
      )}
      <Toaster />
    </div>
  );
}
