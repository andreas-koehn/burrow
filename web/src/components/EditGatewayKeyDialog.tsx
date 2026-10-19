import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, SubmitHint, firstMissing } from "@/components/ds";
import { KeyModelsField, useModelEntries } from "@/components/KeyModelsField";
import { KeyToolsField } from "@/components/KeyToolsField";
import { sameList } from "@/lib/toolAllow";
import { SystemTextField } from "@/components/SystemTextField";
import { MAX_SYSTEM_TEXT_BYTES, byteLength } from "@/lib/systemText";
import type { AiGatewayKey } from "@/lib/contract";

export interface EditGatewayKeyDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  gatewayKey: AiGatewayKey;
}

function EditForm({ onOpenChange, gatewayKey }: Omit<EditGatewayKeyDialogProps, "open">) {
  const qc = useQueryClient();
  const [name, setName] = useState(gatewayKey.name);
  const [text, setText] = useState(gatewayKey.system_text ?? "");
  const [restricted, setRestricted] = useState(gatewayKey.allowed_models.length > 0);
  const [models, setModels] = useState(gatewayKey.allowed_models);
  const [tools, setTools] = useState(gatewayKey.allowed_tools ?? []);
  const [toolsReadOnly, setToolsReadOnly] = useState(gatewayKey.tools_read_only ?? false);
  const { entries, loading: modelsLoading } = useModelEntries(gatewayKey.allowed_models);
  const [formErr, setFormErr] = useState<string | null>(null);

  const trimmed = name.trim();
  const nameChanged = trimmed !== gatewayKey.name;
  const textChanged = text !== (gatewayKey.system_text ?? "");
  // An empty list is every model; "All models" sends it, and so does a key left unrestricted.
  const newModels = restricted ? models : [];
  const modelsChanged = !sameList(newModels, gatewayKey.allowed_models);
  const toolsChanged = !sameList(tools, gatewayKey.allowed_tools ?? []);
  const readOnlyChanged = toolsReadOnly !== (gatewayKey.tools_read_only ?? false);

  const save = useMutation({
    mutationFn: () => {
      // Only what changed: a field left out stays as it is.
      const body: { name?: string; system_text?: string; allowed_models?: string[]; allowed_tools?: string[]; tools_read_only?: boolean } = {};
      if (nameChanged) body.name = trimmed;
      if (textChanged) body.system_text = text;
      if (modelsChanged) body.allowed_models = newModels;
      if (toolsChanged) body.allowed_tools = tools;
      if (readOnlyChanged) body.tools_read_only = toolsReadOnly;
      return apiFetch<AiGatewayKey>(`/ai/keys/${encodeURIComponent(gatewayKey.id)}`, { method: "PUT", body: JSON.stringify(body) });
    },
    onSuccess: async () => {
      toast.success("Key saved.");
      onOpenChange(false);
      await qc.invalidateQueries({ queryKey: ["ai", "keys"] });
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't save the key.");
      else if (e.status === 404) setFormErr("This key no longer exists.");
      else if (e.status === 403) setFormErr("You don't have permission to change this key.");
      else setFormErr(e.message);
    },
  });

  const missing = [
    trimmed === "" && "Enter a name.",
    byteLength(text) > MAX_SYSTEM_TEXT_BYTES && `Shorten the system text to ${MAX_SYSTEM_TEXT_BYTES} bytes.`,
    restricted && models.length === 0 && "Choose at least one model or provider.",
    !nameChanged && !textChanged && !modelsChanged && !toolsChanged && !readOnlyChanged && "No changes to save.",
  ];
  const hint = firstMissing(missing);

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!save.isPending) onOpenChange(o); }}
      title={`Edit ${gatewayKey.name}`}
      footer={
        <>
          <SubmitHint id="edit-key-hint" missing={missing} />
          <Button variant="secondary" disabled={save.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={hint !== undefined || save.isPending}
            aria-describedby={hint !== undefined ? "edit-key-hint" : undefined}
            onClick={() => { setFormErr(null); save.mutate(); }}
          >
            {save.isPending ? "Saving…" : "Save changes"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Name" htmlFor="edit-key-name" help="Where the key is used, e.g. laptop or ci.">
          <Input
            id="edit-key-name"
            value={name}
            maxLength={120}
            autoComplete="off"
            onChange={(e) => { setName(e.target.value); setFormErr(null); }}
          />
        </FormField>
        <SystemTextField
          id="edit-key-system-text"
          value={text}
          onChange={(v) => { setText(v); setFormErr(null); }}
          help="Sent before the app's own system text for every request made with this key."
        />
      </FormFieldGroup>
      <KeyModelsField
        restricted={restricted}
        onRestrictedChange={(r) => { setRestricted(r); setFormErr(null); }}
        entries={entries}
        loading={modelsLoading}
        allowed={models}
        onAllowedChange={(v) => { setModels(v); setFormErr(null); }}
      />
      <KeyToolsField
        idPrefix="edit-key"
        value={tools}
        onChange={(v) => { setTools(v); setFormErr(null); }}
        readOnly={toolsReadOnly}
        onReadOnlyChange={(v) => { setToolsReadOnly(v); setFormErr(null); }}
      />
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

/** Renames a gateway key, sets its system text and edits its model and tool lists. The form starts fresh every time it opens. */
export function EditGatewayKeyDialog({ open, onOpenChange, gatewayKey }: EditGatewayKeyDialogProps) {
  if (!open) return null;
  return <EditForm onOpenChange={onOpenChange} gatewayKey={gatewayKey} />;
}
