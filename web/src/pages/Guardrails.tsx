import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown, ChevronRight, Trash2 } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import {
  Button, Dialog, FormField, Input, PageHeader, Select, SkeletonRows, Switch,
} from "@/components/ds";
import { RedactionRuleDialog } from "@/components/RedactionRuleDialog";
import type {
  GuardrailPattern, GuardrailSettings, GuardrailSettingsResponse,
  RedactionRule, RedactionSettings,
} from "@/lib/contract";

interface RulesResponse {
  built_in: RedactionRule[];
  custom: RedactionRule[];
}

function Accordion({ id, title, subtitle, children, defaultOpen = false }: {
  id: string; title: string; subtitle?: string; children: ReactNode; defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div className="accordion-section">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-body`}
        onClick={() => setOpen((o) => !o)}
        className="accordion-trigger"
      >
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        <span>{title}</span>
        {/* P2-8 — subtitle gives an at-a-glance hint of "configured vs not"
            without forcing the user to expand each section. */}
        {subtitle && <span className="muted small">· {subtitle}</span>}
      </button>
      {open && (
        <div id={`${id}-body`} role="region" className="accordion-body">
          {children}
        </div>
      )}
    </div>
  );
}

function RulesTable({ name, rules, onDelete }: { name: string; rules: RedactionRule[]; onDelete?: (r: RedactionRule) => void }) {
  const cols = onDelete ? 5 : 4;
  return (
    <div className="table-wrap">
      <table className="data" aria-label={name}>
        <thead>
          <tr>
            <th>Name</th><th>Pattern</th><th>Action</th><th>Scope</th>
            {onDelete && <th className="col-actions"></th>}
          </tr>
        </thead>
        <tbody>
          {rules.length === 0
            ? <tr><td colSpan={cols} className="muted">No rules yet.</td></tr>
            : rules.map((r) => (
                <tr key={r.id}>
                  <td>{r.name}</td>
                  <td className="mono small">{r.pattern}</td>
                  <td>{r.action}</td>
                  <td>{r.scope}</td>
                  {onDelete && (
                    <td className="col-actions">
                      <button type="button" className="icon-btn" aria-label={`Delete rule ${r.name}`} onClick={() => onDelete(r)}>
                        <Trash2 size={13} />
                      </button>
                    </td>
                  )}
                </tr>
              ))}
        </tbody>
      </table>
    </div>
  );
}

function PatternList({ patterns }: { patterns: GuardrailPattern[] }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button
        type="button"
        className="link"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        {open ? "Hide pattern list" : "View pattern list"}
      </button>
      {open && (
        <ul className="mono small">
          {patterns.map((p) => <li key={p.id}>{p.description}</li>)}
        </ul>
      )}
    </div>
  );
}

const ACTION_OPTIONS = [
  { value: "log_only", label: "Log only" },
  { value: "refuse_403", label: "Refuse (HTTP 403)" },
  { value: "refuse_safe", label: "Refuse with safe response" },
];

export default function Guardrails() {
  const qc = useQueryClient();
  const rules = useQuery({
    queryKey: ["redaction", "rules"],
    queryFn: () => apiFetch<RulesResponse>("/redaction/rules"),
    retry: false,
  });
  const redactSettings = useQuery({
    queryKey: ["redaction", "settings"],
    queryFn: () => apiFetch<RedactionSettings>("/redaction/settings"),
    retry: false,
  });
  const guardSettings = useQuery({
    queryKey: ["guardrails", "settings"],
    queryFn: () => apiFetch<GuardrailSettingsResponse>("/guardrails/settings"),
    retry: false,
  });
  const patterns = useQuery({
    queryKey: ["guardrails", "patterns"],
    queryFn: () => apiFetch<GuardrailPattern[]>("/guardrails/patterns"),
    retry: false,
  });

  const [redactDraft, setRedactDraft] = useState<RedactionSettings | null>(null);
  const [guardDraft, setGuardDraft] = useState<GuardrailSettings | null>(null);
  const [presidioUrl, setPresidioUrl] = useState("");

  useEffect(() => {
    if (redactSettings.data && !redactDraft) {
      setRedactDraft(redactSettings.data);
      setPresidioUrl(redactSettings.data.presidio_url ?? "");
    }
  }, [redactSettings.data, redactDraft]);
  useEffect(() => {
    if (guardSettings.data && !guardDraft) setGuardDraft(guardSettings.data.global);
  }, [guardSettings.data, guardDraft]);

  const saveRedact = useMutation({
    mutationFn: (next: RedactionSettings) =>
      apiFetch<void>("/redaction/settings", { method: "PUT", body: JSON.stringify(next) }),
    onSuccess: () => {
      toast.success("Regex redaction saved.");
      qc.invalidateQueries({ queryKey: ["redaction", "settings"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't save redaction settings."),
  });
  const saveGuard = useMutation({
    mutationFn: (next: GuardrailSettings) =>
      apiFetch<void>("/guardrails/settings", { method: "PUT", body: JSON.stringify(next) }),
    onSuccess: () => {
      toast.success("Prompt-injection settings saved.");
      qc.invalidateQueries({ queryKey: ["guardrails", "settings"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't save guardrails."),
  });
  const testPresidio = useMutation({
    mutationFn: (url: string) =>
      apiFetch<{ matches: { rule: string; count: number }[] }>(
        "/redaction/preview",
        { method: "POST", body: JSON.stringify({ presidio_url: url, sample: "email@example.com" }) },
      ),
    onSuccess: (res) => toast.message(`Presidio responded: ${JSON.stringify(res)}`),
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't reach Presidio."),
  });
  const [ruleOpen, setRuleOpen] = useState(false);
  const [deleteRule, setDeleteRule] = useState<RedactionRule | null>(null);
  const removeRule = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/redaction/rules/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Rule deleted.");
      qc.invalidateQueries({ queryKey: ["redaction", "rules"] });
    },
    onError: (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Couldn't delete the rule."),
  });

  if (!redactDraft || !guardDraft || !rules.data) {
    return (
      <div className="guardrails-page">
        <PageHeader title="Guardrails & redaction" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  return (
    <div className="guardrails-page">
      <PageHeader
        title="Guardrails & redaction"
        subtitle="Filter outbound prompts, redact PII, and block prompt-injection attempts."
      />

      <Accordion
        id="regex"
        title="Regex redaction"
        subtitle={(() => {
          const total = rules.data.built_in.length + rules.data.custom.length;
          if (!redactDraft.enabled) return "Disabled";
          if (total === 0) return "0 rules configured";
          return `${total} rule${total === 1 ? "" : "s"}`;
        })()}
      >
        <div className="row row-center gap-2">
          <Switch
            aria-label="Enable redaction"
            checked={redactDraft.enabled}
            onChange={(v) => setRedactDraft({ ...redactDraft, enabled: v })}
          />
          <span>Enable redaction on request/response bodies</span>
        </div>
        <h3>Built-in rules</h3>
        <RulesTable name="Built-in rules" rules={rules.data.built_in} />
        <div className="row row-center gap-2 subsection-head">
          <h3>Custom rules</h3>
          <Button variant="secondary" size="sm" onClick={() => setRuleOpen(true)}>New rule</Button>
        </div>
        <RulesTable name="Custom rules" rules={rules.data.custom} onDelete={setDeleteRule} />
        <div className="actions">
          <Button variant="primary" size="sm" disabled={saveRedact.isPending} onClick={() => saveRedact.mutate(redactDraft)}>
            {saveRedact.isPending ? "Saving…" : "Save regex settings"}
          </Button>
        </div>
      </Accordion>

      <Accordion
        id="presidio"
        title="Presidio (Microsoft sidecar)"
        subtitle={presidioUrl ? "Configured" : "Not configured"}
      >
        <p className="muted">
          Runs Microsoft Presidio (Apache-2.0) as a sidecar process Burrow shells out to. Off by
          default — you install Presidio yourself.
        </p>
        <FormField label="Presidio URL" htmlFor="presidio-url" w="md">
          <Input
            id="presidio-url"
            className="mono"
            placeholder="http://localhost:5000"
            value={presidioUrl}
            onChange={(e) => setPresidioUrl(e.target.value)}
          />
        </FormField>
        <div className="actions">
          <Button variant="secondary" size="sm" onClick={() => testPresidio.mutate(presidioUrl)}>
            Test connection
          </Button>
        </div>
      </Accordion>

      <Accordion
        id="injection"
        title="Prompt-injection guardrails"
        subtitle={guardDraft.enabled ? `${(patterns.data ?? []).length} pattern${(patterns.data ?? []).length === 1 ? "" : "s"}` : "Disabled"}
      >
        <div className="row row-center gap-2">
          <Switch
            aria-label="Enable injection guardrails"
            checked={guardDraft.enabled}
            onChange={(v) => setGuardDraft({ ...guardDraft, enabled: v })}
          />
          <span>Inspect prompts for injection patterns</span>
        </div>
        <FormField label="On detection" htmlFor="injection-action" w="md">
          <Select
            id="injection-action"
            value={guardDraft.action}
            onChange={(v) => setGuardDraft({ ...guardDraft, action: v as GuardrailSettings["action"] })}
            options={ACTION_OPTIONS}
          />
        </FormField>
        <PatternList patterns={patterns.data ?? []} />
        <div className="actions">
          <Button variant="primary" size="sm" disabled={saveGuard.isPending} onClick={() => saveGuard.mutate(guardDraft)}>
            {saveGuard.isPending ? "Saving…" : "Save guardrails"}
          </Button>
        </div>
      </Accordion>

      <RedactionRuleDialog open={ruleOpen} onClose={() => setRuleOpen(false)} />
      <Dialog
        open={deleteRule !== null}
        onOpenChange={(o) => { if (!o) setDeleteRule(null); }}
        title="Delete rule?"
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteRule(null)}>Cancel</Button>
            <Button
              variant="destructive"
              disabled={removeRule.isPending}
              onClick={() => { if (deleteRule) removeRule.mutate(deleteRule.id); setDeleteRule(null); }}
            >
              Delete
            </Button>
          </>
        }
      >
        <p>Delete <code className="mono">{deleteRule?.name}</code>? Matching content will no longer be redacted.</p>
      </Dialog>

      <Toaster />
    </div>
  );
}
