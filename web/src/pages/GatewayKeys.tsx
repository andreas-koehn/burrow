import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, KeyRound } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, Disclosure, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, SkeletonRows, SubmitHint, firstMissing } from "@/components/ds";
import { copyText } from "@/lib/clipboard";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { RelativeTime } from "@/components/RelativeTime";
import { EditGatewayKeyDialog } from "@/components/EditGatewayKeyDialog";
import { KeyModelsField, useModelEntries } from "@/components/KeyModelsField";
import { KeyToolsField } from "@/components/KeyToolsField";
import { SystemTextField } from "@/components/SystemTextField";
import { MAX_SYSTEM_TEXT_BYTES, byteLength } from "@/lib/systemText";
import type { AiGatewayKey, CreatedAiGatewayKey } from "@/lib/contract";

const KEYS = ["ai", "keys"] as const;

interface NewKeyDialogProps {
  onClose: () => void;
  /**
   * Slug of a provider the key starts limited to: "Everything from <slug>" is
   * ticked, and every model that sends its requests to that provider alone.
   */
  provider?: string;
}

/**
 * Creates a key and shows it this once. The plaintext lives in this
 * component's state only: it is taken out of the answer before the mutation
 * keeps its result, and it is gone when the dialog closes.
 */
function NewKeyDialog({ onClose, provider }: NewKeyDialogProps) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [restricted, setRestricted] = useState(provider !== undefined);
  // What the reader ticked; null until they tick or untick something.
  const [picked, setPicked] = useState<string[] | null>(provider !== undefined ? null : []);
  const [tools, setTools] = useState<string[]>([]);
  const [toolsReadOnly, setToolsReadOnly] = useState(false);
  const [toolsOpen, setToolsOpen] = useState(false);
  const [systemText, setSystemText] = useState("");
  // Closed, so the dialog's default state keeps to a name and one choice.
  const [textOpen, setTextOpen] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  const { entries, models: modelList, loading: modelsLoading } = useModelEntries();
  // Opened for a provider, the key also allows the models that provider serves alone: "Everything
  // from" a provider allows its catalog ids, not the names of models, and a model made a step
  // earlier in the guided flow would otherwise be refused with this key.
  const servedAlone = modelList
    .filter((m) => m.targets.length > 0 && m.targets.every((t) => t.provider === provider))
    .map((m) => m.name);
  const allowed = picked ?? (provider !== undefined ? [...servedAlone, `${provider}/*`] : []);
  const chosen = entries.filter((e) => allowed.includes(e.value)).map((e) => e.value);

  const create = useMutation({
    mutationFn: async () => {
      const res = await apiFetch<CreatedAiGatewayKey>("/ai/keys", {
        method: "POST",
        // No list: every model.
        body: JSON.stringify({
          name: name.trim(),
          ...(restricted ? { allowed_models: chosen } : {}),
          // No list means no tools; only a key that gets some sends them.
          ...(tools.length > 0 ? { allowed_tools: tools } : {}),
          ...(tools.length > 0 && toolsReadOnly ? { tools_read_only: true } : {}),
          ...(systemText !== "" ? { system_text: systemText } : {}),
        }),
      });
      setSecret(res.key);
      // What the mutation keeps as its result must not carry the key.
      return res.id;
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: KEYS }),
    onError: (e: unknown) => setFormErr(e instanceof ApiError ? e.message : "Couldn't create the key."),
  });

  const trimmed = name.trim();
  // Beside the button while it is disabled. Nothing ticked would be sent as an
  // empty list, which means every model.
  const missing = [
    trimmed === "" && "Enter a name.",
    restricted && chosen.length === 0 && "Choose at least one model or provider.",
    byteLength(systemText) > MAX_SYSTEM_TEXT_BYTES && `Shorten the system text to ${MAX_SYSTEM_TEXT_BYTES} bytes.`,
  ];
  const hint = firstMissing(missing);

  if (secret !== null) {
    return (
      <Dialog
        open
        onOpenChange={(o) => { if (!o) onClose(); }}
        title="New gateway key"
        footer={<Button variant="primary" onClick={onClose}>Done</Button>}
      >
        <FormField label="Your new key" htmlFor="gateway-key-secret" help="Copy it now. It is not shown again.">
          <div className="row row-center gap-2">
            <Input
              id="gateway-key-secret"
              className="fill-rest"
              mono
              readOnly
              value={secret}
              autoComplete="off"
              spellCheck={false}
              onFocus={(e) => e.currentTarget.select()}
            />
            <button
              type="button"
              className="icon-btn"
              aria-label="Copy key"
              onClick={() => void copyText(secret)}
            >
              <Copy size={13} aria-hidden="true" />
            </button>
          </div>
        </FormField>
        <p className="muted small">
          The relay stores only a hash of it. If it is lost, revoke this key and create a new one.
        </p>
      </Dialog>
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o && !create.isPending) onClose(); }}
      title="New gateway key"
      description="A key a program sends to the AI gateway. It is shown once, right after it is created."
      footer={
        <>
          <SubmitHint id="gateway-key-hint" missing={missing} />
          <Button variant="secondary" disabled={create.isPending} onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            disabled={hint !== undefined || create.isPending}
            aria-describedby={hint !== undefined ? "gateway-key-hint" : undefined}
            onClick={() => { setFormErr(null); create.mutate(); }}
          >
            {create.isPending ? "Creating…" : "Create key"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Name" htmlFor="gateway-key-name" help="Where the key is used, e.g. laptop or ci.">
          <Input
            id="gateway-key-name"
            value={name}
            maxLength={120}
            autoComplete="off"
            onChange={(e) => { setName(e.target.value); setFormErr(null); }}
          />
        </FormField>
      </FormFieldGroup>
      <Disclosure label="System text" open={textOpen || byteLength(systemText) > MAX_SYSTEM_TEXT_BYTES} onOpenChange={setTextOpen}>
        <SystemTextField
          id="gateway-key-system-text"
          value={systemText}
          onChange={(v) => { setSystemText(v); setFormErr(null); }}
          help="Sent before the app's own system text for every request made with this key."
        />
      </Disclosure>
      <KeyModelsField
        restricted={restricted}
        onRestrictedChange={(r) => { setRestricted(r); setFormErr(null); }}
        entries={entries}
        loading={modelsLoading}
        allowed={allowed}
        onAllowedChange={(v) => { setPicked(v); setFormErr(null); }}
      />
      {/* Closed, like the system text: the default state keeps to a name and one choice. */}
      <Disclosure label="Tools" open={toolsOpen || tools.length > 0} onOpenChange={setToolsOpen}>
        <KeyToolsField
          idPrefix="gateway-key"
          value={tools}
          onChange={(v) => { setTools(v); setFormErr(null); }}
          readOnly={toolsReadOnly}
          onReadOnlyChange={setToolsReadOnly}
        />
      </Disclosure>
      {!(toolsOpen || tools.length > 0) && <p className="muted small">No tools unless you add them.</p>}
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

export default function GatewayKeys() {
  const qc = useQueryClient();
  const { user } = useAuth();
  // POST, PUT and DELETE /ai/keys: ai:configure:own or :any. The list holds other users' keys for ai:read:any.
  const canWrite = useCan([P.aiConfigureOwn, P.aiConfigureAny]);
  // The relay lets only a key's owner or a holder of ai:configure:any change or revoke it;
  // for anyone else the key answers 404.
  const configuresAny = useCan(P.aiConfigureAny);
  const mayChange = (k: AiGatewayKey) => canWrite && (configuresAny || (user !== undefined && user !== null && user.id === k.user_id));
  const seesOwners = useCan([P.aiReadAny, P.aiConfigureAny]);
  const keys = useQuery({
    queryKey: KEYS,
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });

  const [newOpen, setNewOpen] = useState(false);
  // /gateway/keys?new=1&provider=<slug> opens "New gateway key" limited to that
  // provider. The parameters stay while the dialog is open and go when it closes.
  const [searchParams, setSearchParams] = useSearchParams();
  const linked = searchParams.get("new") === "1";
  function closeNew() {
    setNewOpen(false);
    if (searchParams.has("new") || searchParams.has("provider")) {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        next.delete("new");
        next.delete("provider");
        return next;
      }, { replace: true });
    }
  }
  const [editing, setEditing] = useState<AiGatewayKey | null>(null);
  const [revoking, setRevoking] = useState<AiGatewayKey | null>(null);
  const [revokeErr, setRevokeErr] = useState<string | null>(null);
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/ai/keys/${encodeURIComponent(id)}`, { method: "DELETE" }),
    onSuccess: async () => {
      setRevoking(null);
      toast.success("Key revoked.");
      await qc.invalidateQueries({ queryKey: KEYS });
    },
    onError: (e: unknown) => setRevokeErr(e instanceof ApiError ? e.message : "Couldn't revoke the key."),
  });

  const list = Array.isArray(keys.data) ? keys.data : [];
  const newKey = canWrite ? <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>New key</Button> : undefined;

  return (
    <div className="gateway-keys-page">
      <PageHeader
        title="Gateway keys"
        subtitle="Keys for programs that call the AI gateway, for all models or for chosen ones."
        actions={newKey}
      />
      <p className="muted small page-intro">
        An app sends a gateway key to the AI gateway. To connect a machine, use a{" "}
        <Link className="link-inline" to="/clients?tab=tokens">client token</Link>; to script the management API, use an{" "}
        <Link className="link-inline" to="/settings/automation">automation token</Link>.
      </p>

      {keys.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void keys.refetch()}>Retry</Button>}
        >
          Couldn't load gateway keys:{" "}
          {keys.error instanceof ApiError ? keys.error.message : "Unknown error"}
        </ErrorNotice>
      ) : keys.isLoading ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={2} />
        </div>
      ) : list.length === 0 ? (
        <EmptyState icon={<KeyRound size={18} />} title="No gateway keys yet" action={newKey}>
          Create a key for each program or machine that calls the gateway, so each can be revoked on its own.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Gateway keys">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Key</th>
                <th scope="col">Models</th>
                <th scope="col">Tools</th>
                <th scope="col">Last used</th>
                <th scope="col">Status</th>
                {seesOwners && <th scope="col">Owner</th>}
                <th scope="col" className="col-actions" aria-label="Actions"></th>
              </tr>
            </thead>
            <tbody>
              {list.map((k) => (
                <tr key={k.id}>
                  <td className="col-name">
                    {k.name}
                    {k.system_text && <div className="muted small">System text</div>}
                  </td>
                  <td className="mono">{`${k.key_prefix}…`}</td>
                  <td>
                    {k.allowed_models.length === 0
                      ? "all"
                      : (
                        // The names are in the tooltip for a pointer and in hidden text
                        // for a screen reader: a title alone reaches neither keyboard nor touch.
                        <span title={k.allowed_models.join(", ")}>
                          {k.allowed_models.length === 1 ? "1 entry" : `${k.allowed_models.length} entries`}
                          <span className="visually-hidden">: {k.allowed_models.join(", ")}</span>
                        </span>
                      )}
                  </td>
                  <td>
                    {(k.allowed_tools ?? []).length === 0
                      ? "none"
                      : (
                        <span title={(k.allowed_tools ?? []).join(", ")}>
                          {(k.allowed_tools ?? []).length === 1 ? "1 entry" : `${(k.allowed_tools ?? []).length} entries`}
                          {k.tools_read_only && " · read-only"}
                          <span className="visually-hidden">: {(k.allowed_tools ?? []).join(", ")}</span>
                        </span>
                      )}
                  </td>
                  <td>
                    <RelativeTime value={k.last_used} empty="never" />
                  </td>
                  <td>
                    <Badge kind={k.revoked_at ? "status-offline" : "status-connected"}>
                      {k.revoked_at ? "revoked" : "active"}
                    </Badge>
                  </td>
                  {seesOwners && (
                    <td>{k.user_id === user?.id ? "you" : <span className="mono small">{k.user_id}</span>}</td>
                  )}
                  <td className="col-actions">
                    <div className="row row-center gap-2" style={{ justifyContent: "flex-end" }}>
                    {mayChange(k) && (
                      <Button
                        variant="secondary"
                        size="sm"
                        aria-label={`Edit ${k.name}`}
                        onClick={() => setEditing(k)}
                      >
                        Edit
                      </Button>
                    )}
                    {mayChange(k) && !k.revoked_at && (
                      <Button
                        variant="destructive"
                        size="sm"
                        aria-label={`Revoke ${k.name}`}
                        onClick={() => { setRevokeErr(null); setRevoking(k); }}
                      >
                        Revoke
                      </Button>
                    )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Mounted only while open: closing it drops the form and the key it showed. */}
      {canWrite && (newOpen || linked) && (
        <NewKeyDialog onClose={closeNew} provider={linked ? searchParams.get("provider") ?? undefined : undefined} />
      )}

      {canWrite && editing !== null && (
        <EditGatewayKeyDialog open onOpenChange={(o) => { if (!o) setEditing(null); }} gatewayKey={editing} />
      )}

      <Dialog
        open={revoking !== null}
        onOpenChange={(o) => { if (!o && !revoke.isPending) setRevoking(null); }}
        title={`Revoke ${revoking?.name ?? ""}?`}
        footer={
          <>
            <Button variant="secondary" disabled={revoke.isPending} onClick={() => setRevoking(null)}>Cancel</Button>
            <Button variant="destructive" disabled={revoke.isPending} onClick={() => { if (revoking) revoke.mutate(revoking.id); }}>
              {revoke.isPending ? "Revoking…" : "Revoke key"}
            </Button>
          </>
        }
      >
        <p className="muted">Programs that use this key stop working immediately. This cannot be undone.</p>
        {revokeErr && <ErrorNotice>{revokeErr}</ErrorNotice>}
      </Dialog>
      <Toaster />
    </div>
  );
}
