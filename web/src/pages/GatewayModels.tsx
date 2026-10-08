import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Layers, MoreHorizontal } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, DropdownMenu, EmptyState, ErrorNotice, PageHeader, SkeletonRows } from "@/components/ds";
import { AttemptLookup } from "@/components/AttemptLookup";
import { ConnectCard } from "@/components/ConnectCard";
import { GatewayAddresses } from "@/components/GatewayAddresses";
import { ModelDialog } from "@/components/ModelDialog";
import { useAuth } from "@/auth/useAuth";
import { pairsWords } from "@/lib/translation";
import type { AiModel, AiProvider, Dialect, GatewayInfo } from "@/lib/contract";

const FORMAT_NAME: Record<Dialect, string> = { openai: "OpenAI format", anthropic: "Anthropic format" };

/**
 * Where a model goes in one format: its targets in the order they are tried,
 * which one is answering right now and which cannot be tried. A format without
 * a target says whether it is answered through translation, and in which
 * direction, or not at all. Every state is said in words, never by colour alone.
 */
function FormatCell({ model, dialect }: { model: AiModel; dialect: Dialect }) {
  const targets = model.targets.filter((t) => t.dialect === dialect);
  // The mode first: a stored target whose provider speaks another format now
  // is still listed, while the format is answered through translation.
  const pairs = model.translation_pairs?.[dialect] ?? [];
  const translated = model.dialect_modes?.[dialect] === "translated" && pairs.length > 0;
  const mode = translated && (
    <span className="format-mode">
      <Badge kind="status-idle">translated</Badge>
      <span className="muted small">{pairsWords(pairs)}</span>
    </span>
  );
  if (targets.length === 0) return mode || <span className="muted">not served</span>;
  // A disabled model serves nothing; the Status column says why.
  const serving = model.enabled ? model.serving?.[dialect] : null;
  const servingAt = serving
    ? targets.findIndex((t) => t.available && t.provider === serving.provider && t.model === serving.model)
    : -1;
  return (
    <>
      {mode}
      <ol className="target-chain" aria-label={`${FORMAT_NAME[dialect]} targets of ${model.name}, in the order they are tried`}>
        {targets.map((t, i) => (
          <li key={`${t.provider}/${t.model}`}>
            {i > 0 && <span className="muted" aria-hidden="true">→</span>}
            <span className={t.available ? "mono" : "mono target-unavailable"}>{`${t.provider}/${t.model}`}</span>
            {i === servingAt ? (
              <Badge kind="status-connected">
                <span aria-hidden="true">serving</span>
                <span className="visually-hidden">serving now</span>
              </Badge>
            ) : !t.available && <Badge kind="status-idle">unavailable</Badge>}
          </li>
        ))}
      </ol>
      {model.enabled && servingAt < 0 && !translated && <Badge kind="status-offline">no target available</Badge>}
    </>
  );
}

/** How the Responses API (what Codex uses) is served on the OpenAI endpoint, in words. */
function responsesText(model: AiModel): string | null {
  // A disabled model serves nothing; the Status column says why.
  if (!model.enabled || !model.responses_mode) return null;
  if (model.responses_mode === "native") return "Responses API: served natively";
  const pairs = model.translation_pairs?.responses ?? [];
  if (model.responses_mode === "translated" && pairs.length > 0) return `Responses API: translated (${pairsWords(pairs)})`;
  return "Responses API: not served";
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
                  <th scope="col">Name</th>
                  <th scope="col">OpenAI format</th>
                  <th scope="col">Anthropic format</th>
                  <th scope="col">Status</th>
                  {canWrite && <th scope="col" className="col-actions" aria-label="Actions"></th>}
                </tr>
              </thead>
              <tbody>
                {list.map((m) => (
                  <tr key={m.name}>
                    <td className="col-name">
                      <div className="mono">{m.name}</div>
                      {m.description && <div className="muted small">{m.description}</div>}
                    </td>
                    <td>
                      <FormatCell model={m} dialect="openai" />
                      {responsesText(m) && <div className="muted small format-responses">{responsesText(m)}</div>}
                    </td>
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
            canCreate={canWrite}
            endpoints={endpoints}
            models={list.filter((m) => m.enabled)}
          />
          {/* The relay gives the attempt log to admins only. */}
          {canWrite && <AttemptLookup />}
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
