import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Link } from "react-router-dom";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, Disclosure, EmptyState, FormField, FormFieldGroup, Input, PageHeader, Segmented, Select, SkeletonRows, SubmitHint, firstMissing } from "@/components/ds";
import { PermissionPicker } from "@/components/PermissionPicker";
import { usePermissions } from "@/auth/useCan";
import { RelativeTime } from "@/components/RelativeTime";
import type { AutomationPreset, AutomationToken, CreatedAutomationToken } from "@/lib/contract";

const EXPIRY_OPTIONS = [
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
  { value: "never", label: "Never" },
];

/** The name of the preset whose list equals `perms` as a set, else null. */
export function presetOf(perms: string[], presets: AutomationPreset[]): string | null {
  const want = new Set(perms);
  const hit = presets.find((p) => new Set(p.permissions).size === want.size && p.permissions.every((x) => want.has(x)));
  return hit ? hit.name : null;
}

const PRESET_LABELS: Record<string, string> = { "read-only": "Read-only", operator: "Operator", full: "Full", custom: "Custom" };

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
}

function tokenAccess(perms: string[], presets: AutomationPreset[] | undefined): string {
  // A relay from before presets cannot name one.
  if (!presets) return `${perms.length} grant(s)`;
  const name = presetOf(perms, presets);
  return name ? PRESET_LABELS[name] ?? name : `Custom · ${perms.length}`;
}

export default function AutomationTokens() {
  const qc = useQueryClient();
  const tokens = useQuery({
    queryKey: ["automation", "tokens"],
    queryFn: () => apiFetch<AutomationToken[]>("/automation/tokens"),
    retry: false,
  });

  const presetsQ = useQuery({
    queryKey: ["automation", "presets"],
    queryFn: () => apiFetch<AutomationPreset[]>("/automation/presets"),
    retry: false,
  });
  const presets = presetsQ.data;
  // A relay from before presets answers 404: the dialog then posts no list and the token inherits.
  const hasPresets = presets !== undefined;
  // Only a 404 means an older relay; any other failure must not turn into a token that inherits everything.
  const olderRelay = presetsQ.error instanceof ApiError && presetsQ.error.status === 404;
  const held = usePermissions();

  const [addOpen, setAddOpen] = useState(false);
  // What the reader picked; until then the dialog shows the read-only preset.
  const [picked, setPicked] = useState<{ choice: string; perms: string[] } | null>(null);
  const [permsOpen, setPermsOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState("never");
  const readOnly = presets?.find((p) => p.name === "read-only");
  const choice = picked?.choice ?? "read-only";
  const perms = picked?.perms ?? readOnly?.permissions ?? [];
  const chosenPreset = presets?.find((p) => p.name === choice);
  const [created, setCreated] = useState<CreatedAutomationToken | null>(null);

  function closeAdd() {
    setAddOpen(false);
    setName("");
    setExpiry("never");
    setPicked(null);
    setPermsOpen(false);
  }

  const mint = useMutation({
    mutationFn: () =>
      apiFetch<CreatedAutomationToken>("/automation/tokens", {
        method: "POST",
        body: JSON.stringify({
          name,
          expires_at: expiry === "never" ? null : expiry,
          ...(hasPresets ? { permissions: perms } : {}),
        }),
      }),
    onSuccess: (res) => {
      setCreated(res);
      qc.invalidateQueries({ queryKey: ["automation", "tokens"] });
      closeAdd();
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't mint token."),
  });
  const revoke = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/automation/tokens/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["automation", "tokens"] }),
  });

  const missing = [name.trim() === "" && "Enter a name.", presetsQ.isPending && "Loading the permission presets…",
    presetsQ.isError && !olderRelay && "The permission presets could not be loaded. Close the dialog and try again."];
  const hint = firstMissing(missing);

  return (
    <div className="automation-page settings-page">
      <PageHeader
        title="Automation tokens"
        subtitle="Long-lived bearer tokens for scripts and MCP clients. Each holds the permissions you give it, never more than you hold."
        actions={<Button variant="primary" size="sm" onClick={() => setAddOpen(true)}>New token</Button>}
      />

      <p className="muted small">These authenticate scripts, not clients. To connect a machine that runs burrow, use <Link className="link-inline" to="/clients?tab=tokens">Client tokens</Link>. Apps that call a model use a <Link className="link-inline" to="/gateway/keys">gateway key</Link>.</p>

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
                  <td className="mono">{t.prefix}</td>
                  <td>{t.role_at_mint}</td>
                  <td>{tokenAccess(t.permissions, presets)}</td>
                  {/* A day to come is written out; one that has passed says how long ago. */}
                  <td><RelativeTime value={t.expires_at} empty="never" /></td>
                  <td><RelativeTime value={t.last_used} /></td>
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
        onOpenChange={(o) => { if (o) setAddOpen(true); else closeAdd(); }}
        title="New automation token"
        footer={
          <>
            <SubmitHint id="at-hint" missing={missing} />
            <Button variant="secondary" onClick={closeAdd}>Cancel</Button>
            <Button
              variant="primary"
              disabled={hint !== undefined || mint.isPending}
              aria-describedby={hint !== undefined ? "at-hint" : undefined}
              onClick={() => mint.mutate()}
            >
              Create
            </Button>
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
        {hasPresets ? (
          <>
            <div className="form-field">
              <span className="muted small">Access</span>
              <Segmented
                aria-label="Access"
                value={choice}
                onChange={(c) => {
                  const p = presets?.find((x) => x.name === c);
                  setPicked({ choice: c, perms: p ? p.permissions : perms });
                }}
                options={[...presets.map((p) => ({ value: p.name as string, label: PRESET_LABELS[p.name] ?? p.name })), { value: "custom", label: "Custom" }]}
              />
            </div>
            <p className="muted small">
              {perms.length === 0
                ? "This token can only read its own profile."
                : chosenPreset?.description ?? "Choose the permissions below."}
            </p>
            <Disclosure label={`Permissions (${perms.length})`} open={permsOpen} onOpenChange={setPermsOpen}>
              <PermissionPicker
                idPrefix="at-perm"
                held={held}
                value={perms}
                onChange={(next) => setPicked({ choice: presetOf(next, presets) ?? "custom", perms: next })}
              />
            </Disclosure>
          </>
        ) : olderRelay ? (
          <p className="muted">
            The token will inherit your current permissions; the server enforces this too.
          </p>
        ) : null}
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
        <p><Link className="link-inline" to="/settings/mcp">Use it in an MCP client</Link></p>
      </Dialog>
      <Toaster />
    </div>
  );
}
