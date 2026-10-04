import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, FormField, FormFieldGroup, Input, Select } from "@/components/ds";
import type { RedactionRule } from "@/lib/contract";

const ACTION_OPTIONS = [
  { value: "mask", label: "Mask" },
  { value: "drop", label: "Drop" },
  { value: "hash", label: "Hash" },
];
const SCOPE_OPTIONS = [
  { value: "both", label: "Request and response" },
  { value: "request_body", label: "Request body" },
  { value: "response_body", label: "Response body" },
];

export interface RedactionRuleDialogProps {
  open: boolean;
  onClose: () => void;
}

export function RedactionRuleDialog({ open, onClose }: RedactionRuleDialogProps) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [pattern, setPattern] = useState("");
  const [action, setAction] = useState<RedactionRule["action"]>("mask");
  const [scope, setScope] = useState<RedactionRule["scope"]>("both");
  const [err, setErr] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () =>
      apiFetch<RedactionRule>("/redaction/rules", {
        method: "POST",
        body: JSON.stringify({ name, pattern, action, scope }),
      }),
    // The list refreshes even when the dialog was closed mid-request.
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["redaction", "rules"] }); },
  });

  // Closing is always allowed. reset() detaches a pending POST, so the
  // per-call callbacks in submit() never fire into a closed or reopened form.
  function close() {
    create.reset();
    setName(""); setPattern(""); setAction("mask"); setScope("both"); setErr(null);
    onClose();
  }

  const canSubmit = !!name.trim() && !!pattern && !create.isPending;
  function submit() {
    if (!canSubmit) return;
    setErr(null);
    create.mutate(undefined, {
      onSuccess: (rule) => {
        toast.success(`Rule ${rule.name} added.`);
        close();
      },
      onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't add the rule."),
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title="New redaction rule"
      description="Matches are redacted before the body leaves or enters this relay."
      footer={
        <>
          <Button variant="secondary" onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            disabled={!canSubmit}
            onClick={submit}
          >
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </>
      }
    >
      <form onSubmit={(e) => { e.preventDefault(); submit(); }}>
        <FormFieldGroup>
          <FormField label="Name" htmlFor="rr-name">
            <Input id="rr-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. internal-id" />
          </FormField>
          <FormField label="Pattern" htmlFor="rr-pattern" help="Go regular expression (RE2 syntax).">
            <Input id="rr-pattern" className="mono" spellCheck={false} value={pattern} onChange={(e) => setPattern(e.target.value)} placeholder="e.g. \bID-\d{6}\b" />
          </FormField>
          <FormField label="Action" htmlFor="rr-action">
            <Select id="rr-action" options={ACTION_OPTIONS} value={action} onChange={(v) => setAction(v as RedactionRule["action"])} />
          </FormField>
          <FormField label="Scope" htmlFor="rr-scope">
            <Select id="rr-scope" options={SCOPE_OPTIONS} value={scope} onChange={(v) => setScope(v as RedactionRule["scope"])} />
          </FormField>
        </FormFieldGroup>
        {err && <p role="alert" className="notice-inline error">{err}</p>}
        <button type="submit" hidden />
      </form>
    </Dialog>
  );
}
