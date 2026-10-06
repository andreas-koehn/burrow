import { useId, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, KeyRound } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Checkbox, Dialog, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, SkeletonRows } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { formatRelativeTime } from "@/lib/format";
import type { AiGatewayKey, AiModel, AiProvider, CreatedAiGatewayKey } from "@/lib/contract";

const KEYS = ["ai", "keys"] as const;

interface NewKeyDialogProps {
  onClose: () => void;
}

/**
 * Creates a key and shows it this once. The plaintext lives in this
 * component's state only: it is taken out of the answer before the mutation
 * keeps its result, and it is gone when the dialog closes.
 */
function NewKeyDialog({ onClose }: NewKeyDialogProps) {
  const qc = useQueryClient();
  const scopeId = useId();
  const [name, setName] = useState("");
  const [restricted, setRestricted] = useState(false);
  const [allowed, setAllowed] = useState<string[]>([]);
  const [secret, setSecret] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  // Same keys and fetches as the Models and the Providers page.
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  // In list order, so the request carries the entries as the dialog shows them.
  const entries = [
    ...(Array.isArray(models.data) ? models.data : []).map((m) => ({ value: m.name, label: m.name })),
    ...(Array.isArray(providers.data) ? providers.data : []).map((p) => ({ value: `${p.slug}/*`, label: `Everything from ${p.slug}` })),
  ];
  const chosen = entries.filter((e) => allowed.includes(e.value)).map((e) => e.value);

  const create = useMutation({
    mutationFn: async () => {
      const res = await apiFetch<CreatedAiGatewayKey>("/ai/keys", {
        method: "POST",
        // No list: every model.
        body: JSON.stringify(restricted ? { name: name.trim(), allowed_models: chosen } : { name: name.trim() }),
      });
      setSecret(res.key);
      // What the mutation keeps as its result must not carry the key.
      return res.id;
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: KEYS }),
    onError: (e: unknown) => setFormErr(e instanceof ApiError ? e.message : "Couldn't create the key."),
  });

  const trimmed = name.trim();
  // Nothing ticked would be sent as an empty list, which means every model.
  const ready = trimmed !== "" && (!restricted || chosen.length > 0) && !create.isPending;

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
              onClick={() => { void navigator.clipboard?.writeText(secret); toast.success("Copied."); }}
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
          <Button variant="secondary" disabled={create.isPending} onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={!ready} onClick={() => { setFormErr(null); create.mutate(); }}>
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
      <div role="radiogroup" aria-labelledby={scopeId} className="col gap-2">
        <span id={scopeId} className="muted small">Models the key may use</span>
        {([
          [false, "All models"],
          [true, "Only these models"],
        ] as const).map(([value, label]) => (
          <label key={label} className="row row-center gap-2">
            <input
              type="radio"
              name={scopeId}
              checked={restricted === value}
              onChange={() => { setRestricted(value); setFormErr(null); }}
            />
            <span>{label}</span>
          </label>
        ))}
      </div>
      {restricted && (
        <div className="col gap-2 key-models" role="group" aria-label="Allowed models">
          {models.isLoading || providers.isLoading ? (
            <p className="muted small">Loading models…</p>
          ) : entries.length === 0 ? (
            <p className="muted small">There are no models or providers to choose from yet.</p>
          ) : (
            <>
              {entries.map((e, i) => {
                const id = `${scopeId}-entry-${i}`;
                return (
                  <div key={e.value} className="row row-center gap-2">
                    <Checkbox
                      id={id}
                      checked={allowed.includes(e.value)}
                      onChange={(on) => {
                        setAllowed((a) => (on ? [...a, e.value] : a.filter((v) => v !== e.value)));
                        setFormErr(null);
                      }}
                    />
                    <label htmlFor={id}>{e.label}</label>
                  </div>
                );
              })}
              {chosen.length === 0 && <p className="muted small">Choose at least one.</p>}
              <p className="muted small">
                "Everything from" a provider allows its models by their direct address, e.g. provider/model.
              </p>
            </>
          )}
        </div>
      )}
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

export default function GatewayKeys() {
  const qc = useQueryClient();
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const keys = useQuery({
    queryKey: KEYS,
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });

  const [newOpen, setNewOpen] = useState(false);
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
  const newKey = <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>New key</Button>;

  return (
    <div className="gateway-keys-page">
      <PageHeader
        title="Gateway keys"
        subtitle="Keys for programs that call the AI gateway, for all models or for chosen ones."
        actions={newKey}
      />
      <p className="muted small page-intro">
        Gateway keys let a program call the AI gateway. For connecting a machine use{" "}
        <Link className="link-inline" to="/clients?tab=tokens">Client tokens</Link>; for scripting the management API use{" "}
        <Link className="link-inline" to="/settings/automation">Automation tokens</Link>.
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
                <th>Name</th>
                <th>Key</th>
                <th>Models</th>
                <th>Last used</th>
                <th>Status</th>
                {isAdmin && <th>Owner</th>}
                <th className="col-actions" aria-label="Actions"></th>
              </tr>
            </thead>
            <tbody>
              {list.map((k) => (
                <tr key={k.id}>
                  <td className="col-name">{k.name}</td>
                  <td className="mono small">{`${k.key_prefix}…`}</td>
                  <td>
                    {k.allowed_models.length === 0
                      ? "all"
                      : (
                        <span title={k.allowed_models.join(", ")}>
                          {k.allowed_models.length === 1 ? "1 entry" : `${k.allowed_models.length} entries`}
                        </span>
                      )}
                  </td>
                  <td className="small">
                    {k.last_used ? <span title={k.last_used}>{formatRelativeTime(k.last_used)}</span> : "never"}
                  </td>
                  <td>
                    <Badge kind={k.revoked_at ? "status-offline" : "status-connected"}>
                      {k.revoked_at ? "revoked" : "active"}
                    </Badge>
                  </td>
                  {isAdmin && (
                    <td>{k.user_id === user?.id ? "you" : <span className="mono small">{k.user_id}</span>}</td>
                  )}
                  <td className="col-actions">
                    {!k.revoked_at && (
                      <Button
                        variant="destructive"
                        size="sm"
                        aria-label={`Revoke ${k.name}`}
                        onClick={() => { setRevokeErr(null); setRevoking(k); }}
                      >
                        Revoke
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Mounted only while open: closing it drops the form and the key it showed. */}
      {newOpen && <NewKeyDialog onClose={() => setNewOpen(false)} />}

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
