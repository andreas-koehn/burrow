import { useId, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, Switch } from "@/components/ds";
import { MAX_DESCRIPTION_OVERRIDE_BYTES, toolRenameError } from "@/lib/toolSource";
import type { ToolRenameKey, ToolSourceTool, ToolSourceToolChanged, ToolSourceToolUpdate } from "@/lib/contract";

export interface ToolOverridesDialogProps {
  slug: string;
  tool: ToolSourceTool | null;
  onClose: () => void;
  /** The change was saved; `keys` are the active keys that already name the new served name (the relay sends them to a caller who may see other users' keys); `renamed` says the body carried a new name. */
  onSaved: (tool: ToolSourceTool, keys: ToolRenameKey[], renamed: boolean) => void;
}

/** What the operator may change about one tool: hide it, rename it, reword it, classify it. */
export function ToolOverridesDialog({ slug, tool, onClose, onSaved }: ToolOverridesDialogProps) {
  // Mounted per tool, so every opening starts from the stored values.
  return tool ? <Form key={tool.name} slug={slug} tool={tool} onClose={onClose} onSaved={onSaved} /> : null;
}

type ReadOnly = "upstream" | "yes" | "no";
const toChoice = (v: boolean | null): ReadOnly => (v === null ? "upstream" : v ? "yes" : "no");
const fromChoice = (c: ReadOnly): boolean | null => (c === "upstream" ? null : c === "yes");
const bytes = (s: string) => new TextEncoder().encode(s).length;

function Form({ slug, tool, onClose, onSaved }: Omit<ToolOverridesDialogProps, "tool"> & { tool: ToolSourceTool }) {
  const qc = useQueryClient();
  const group = useId();
  const [hidden, setHidden] = useState(tool.hidden);
  const [rename, setRename] = useState(tool.rename);
  const [description, setDescription] = useState(tool.description_override);
  const [readOnly, setReadOnly] = useState<ReadOnly>(toChoice(tool.read_only));
  const [err, setErr] = useState<string | null>(null);

  const renameProblem = toolRenameError(slug, rename);
  const descriptionProblem = bytes(description) > MAX_DESCRIPTION_OVERRIDE_BYTES
    ? `Too long: at most ${MAX_DESCRIPTION_OVERRIDE_BYTES} bytes.` : null;

  const body: ToolSourceToolUpdate = {};
  if (rename !== tool.rename) body.rename = rename;
  if (description !== tool.description_override) body.description_override = description;
  if (fromChoice(readOnly) !== tool.read_only) body.read_only = fromChoice(readOnly);
  if (hidden !== tool.hidden) body.hidden = hidden;
  const changed = Object.keys(body).length > 0;

  const save = useMutation({
    mutationFn: () =>
      apiFetch<ToolSourceToolChanged>(`/mcp/sources/${encodeURIComponent(slug)}/tools/${encodeURIComponent(tool.name)}`, {
        method: "PUT",
        body: JSON.stringify(body),
      }),
    onSuccess: async (saved) => {
      await qc.invalidateQueries({ queryKey: ["mcp", "sources"] });
      // Only a rename brings the list; an answer without it says nothing.
      const keys = Array.isArray(saved?.keys_matching_rename) ? saved.keys_matching_rename : [];
      onSaved(saved, keys, body.rename !== undefined && body.rename !== "");
      onClose();
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the changes."),
  });

  const served = (rename || tool.name);
  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o && !save.isPending) onClose(); }}
      title={`Tool · ${tool.name}`}
      description="These settings change what agents see. The server's own definition is not touched."
      footer={
        <>
          <Button variant="secondary" disabled={save.isPending} onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            disabled={!changed || renameProblem !== null || descriptionProblem !== null || save.isPending}
            onClick={() => { setErr(null); save.mutate(); }}
          >
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <div className="row row-center gap-2">
          <Switch id={`${group}-hide`} checked={hidden} onChange={setHidden} aria-label="Hide this tool" />
          <label htmlFor={`${group}-hide`}>Hide this tool</label>
          <span className="muted small">A hidden tool is not served to anyone.</span>
        </div>
        <FormField
          label="Serve under another name"
          htmlFor={`${group}-rename`}
          w="md"
          descId={`${group}-rename-desc`}
          error={renameProblem ?? undefined}
          help={<>Now served as <span className="mono">{served}</span> on <span className="mono">/mcp/{slug}</span> and <span className="mono">{slug}__{served}</span> on <span className="mono">/mcp</span>. Keys name a tool by its served name. Empty keeps its own name.</>}
        >
          <Input
            id={`${group}-rename`}
            mono
            value={rename}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            invalid={renameProblem !== null}
            aria-describedby={`${group}-rename-desc`}
            onChange={(e) => { setRename(e.target.value); setErr(null); }}
          />
        </FormField>
        <FormField
          label="Description for agents"
          htmlFor={`${group}-desc`}
          descId={`${group}-desc-help`}
          error={descriptionProblem ?? undefined}
          help="Served instead of the approved description. Empty serves the approved one."
        >
          <textarea
            id={`${group}-desc`}
            className="input resizable"
            rows={4}
            value={description}
            aria-invalid={descriptionProblem !== null || undefined}
            aria-describedby={`${group}-desc-help`}
            onChange={(e) => { setDescription(e.target.value); setErr(null); }}
          />
        </FormField>
        <fieldset className="col gap-1" style={{ border: 0, padding: 0, margin: 0 }}>
          <legend className="section-label">Classification</legend>
          {([
            ["upstream", `As the server says (${tool.upstream_read_only ? "read-only" : "can change things"})`],
            ["yes", "Read-only"],
            ["no", "Can change things"],
          ] as const).map(([value, label]) => (
            <label key={value} className="row row-center gap-2">
              <input type="radio" name={`${group}-ro`} checked={readOnly === value} onChange={() => { setReadOnly(value); setErr(null); }} />
              {label}
            </label>
          ))}
          <span className="muted small">A gateway key limited to read-only tools sees only the tools you class as read-only.</span>
        </fieldset>
      </FormFieldGroup>
      {err && <ErrorNotice>{err}</ErrorNotice>}
    </Dialog>
  );
}
