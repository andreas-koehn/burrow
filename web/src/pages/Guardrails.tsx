import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { ChevronDown, ChevronRight, Pencil, Trash2 } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import {
  Button, Dialog, Disclosure, FormField, Input, PageHeader, Segmented, Select, SkeletonRows, SubmitHint, Switch, TableEmptyRow,
} from "@/components/ds";
import { RedactionRuleDialog } from "@/components/RedactionRuleDialog";
import { CustomPatternDialog } from "@/components/CustomPatternDialog";
import { DIRECTION_LABEL, MAX_CUSTOM_PATTERNS } from "@/lib/customPatterns";
import { P } from "@/auth/permissions";
import { useCan } from "@/auth/useCan";
import type {
  AiModel, GuardrailCustomPattern, GuardrailJudgeSettings, GuardrailPattern, GuardrailSettings,
  GuardrailSettingsResponse, RedactionRule, RedactionSettings,
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

// What a rule does and what it applies to, in words; the wire's values are not shown.
const RULE_ACTION: Record<RedactionRule["action"], string> = {
  mask: "Replaces the match with a placeholder",
  drop: "Refuses the request",
  hash: "Replaces the match with its hash",
};
const RULE_SCOPE: Record<RedactionRule["scope"], string> = {
  both: "Requests and responses",
  request_body: "Requests",
  response_body: "Responses",
  tool_arguments: "Tool arguments",
  tool_result: "Tool results",
};

/** A rule's name, with its pattern behind it: a regular expression is for whoever asks for it. */
function RuleName({ rule }: { rule: RedactionRule }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      {rule.name}
      <Disclosure
        label={<>{open ? "Hide pattern" : "Show pattern"}{" "}<span className="visually-hidden">of {rule.name}</span></>}
        open={open}
        onOpenChange={setOpen}
      >
        <code className="mono small">{rule.pattern}</code>
      </Disclosure>
    </>
  );
}

function RulesTable({ name, rules, onDelete }: { name: string; rules: RedactionRule[]; onDelete?: (r: RedactionRule) => void }) {
  const cols = onDelete ? 4 : 3;
  return (
    <div className="table-wrap">
      <table className="data rules-table" aria-label={name}>
        <thead>
          <tr>
            <th>Name</th><th>What it does</th><th>Applies to</th>
            {onDelete && <th className="col-actions"></th>}
          </tr>
        </thead>
        <tbody>
          {rules.length === 0
            ? <TableEmptyRow colSpan={cols} title="No rules yet." />
            : rules.map((r) => (
                <tr key={r.id}>
                  <td className="cell-wrap"><RuleName rule={r} /></td>
                  <td>{RULE_ACTION[r.action] ?? r.action}</td>
                  <td>{RULE_SCOPE[r.scope] ?? r.scope}</td>
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

const MAX_POLICY = 4000;
const MIN_TIMEOUT_MS = 500;
const MAX_TIMEOUT_MS = 30000;

const ANSWER_ACTIONS = [
  { value: "flag" as const, label: "Flag", help: "Deliver the answer and record it." },
  { value: "block" as const, label: "Block", help: "Withhold it. A streamed answer stops at the match; what was already sent stays sent." },
];
const CHECK_OPTIONS = [
  { value: "requests" as const, label: "Requests" },
  { value: "requests_and_answers" as const, label: "Requests and answers" },
];
const ON_FAILURE_OPTIONS = [
  { value: "allow" as const, label: "Let the request through" },
  { value: "refuse" as const, label: "Refuse the request" },
];

/** A section's controls: read-only, whole, for a reader who may not change the settings. */
function Controls({ canWrite, children }: { canWrite: boolean; children: ReactNode }) {
  return <fieldset className="plain-fieldset" disabled={!canWrite}>{children}</fieldset>;
}

function JudgeSection({ draft, onChange, models, canWrite, saving, onSave }: {
  draft: GuardrailJudgeSettings;
  onChange: (next: GuardrailJudgeSettings) => void;
  models: AiModel[] | undefined;
  canWrite: boolean;
  saving: boolean;
  onSave: () => void;
}) {
  // The time limit is typed as text, so a half-typed number is not forced back.
  const [timeoutText, setTimeoutText] = useState(String(draft.timeout_ms));
  const [timeoutLeft, setTimeoutLeft] = useState(false);
  const limit = Number(timeoutText);
  const timeoutValid = timeoutText.trim() !== "" && Number.isInteger(limit) && limit >= MIN_TIMEOUT_MS && limit <= MAX_TIMEOUT_MS;
  const policyLength = [...draft.policy].length;
  const policyValid = policyLength <= MAX_POLICY;

  const names = (models ?? []).map((m) => m.name);
  // A chosen model that no longer exists stays visible so it can be replaced.
  const options = [
    ...names.map((n) => ({ value: n, label: n })),
    ...(draft.model && !names.includes(draft.model) && models ? [{ value: draft.model, label: `${draft.model} (not found)` }] : []),
  ];
  const noModels = models !== undefined && models.length === 0;
  const missing: string[] = draft.enabled && draft.model === "" ? ["Choose a model."] : [];
  const blocked = saving || !timeoutValid || !policyValid || missing.length > 0;

  return (
    <>
      <Controls canWrite={canWrite}>
        <div className="row row-center gap-2">
          <Switch
            aria-label="Ask a model"
            checked={draft.enabled}
            onChange={(v) => onChange({ ...draft, enabled: v })}
          />
          <span>Ask a model whether a request is allowed</span>
        </div>
        {noModels ? (
          <p className="muted">
            There is no model to ask yet.{" "}
            <Link className="link-inline" to="/gateway/models?new=1">Create a model</Link>
          </p>
        ) : (
          <FormField label="Model" htmlFor="judge-model" w="md">
            <Select
              id="judge-model"
              placeholder="Choose a model"
              options={options}
              value={draft.model}
              onChange={(v) => onChange({ ...draft, model: v })}
            />
          </FormField>
        )}
        <FormField
          label="Policy"
          htmlFor="judge-policy"
          descId="judge-policy-desc"
          help="What the model should allow and refuse, in plain words."
          error={policyValid ? undefined : `Up to ${MAX_POLICY} characters.`}
        >
          <textarea
            id="judge-policy"
            className="input resizable"
            rows={5}
            aria-describedby="judge-policy-desc judge-policy-count"
            value={draft.policy}
            onChange={(e) => onChange({ ...draft, policy: e.target.value })}
          />
          <span id="judge-policy-count" className="muted small" aria-live="polite">{policyLength} of {MAX_POLICY}</span>
        </FormField>
        <h3>What to check</h3>
        <Segmented
          aria-label="What to check"
          options={CHECK_OPTIONS}
          value={draft.check}
          onChange={(v) => onChange({ ...draft, check: v })}
        />
        <p className="muted small">Streamed answers are not judged.</p>
        <FormField
          label="Time limit (ms)"
          htmlFor="judge-timeout"
          descId="judge-timeout-desc"
          help="Default 3000."
          error={timeoutLeft && !timeoutValid ? `Use a number from ${MIN_TIMEOUT_MS} to ${MAX_TIMEOUT_MS}.` : undefined}
          w="sm"
        >
          <Input
            id="judge-timeout"
            type="number"
            min={MIN_TIMEOUT_MS}
            max={MAX_TIMEOUT_MS}
            step={100}
            invalid={timeoutLeft && !timeoutValid}
            aria-describedby="judge-timeout-desc"
            value={timeoutText}
            onChange={(e) => {
              setTimeoutText(e.target.value);
              const n = Number(e.target.value);
              if (e.target.value.trim() !== "" && Number.isInteger(n)) onChange({ ...draft, timeout_ms: n });
            }}
            onBlur={() => setTimeoutLeft(true)}
          />
        </FormField>
        <h3>When the judge cannot answer</h3>
        <Segmented
          aria-label="When the judge cannot answer"
          options={ON_FAILURE_OPTIONS}
          value={draft.on_failure}
          onChange={(v) => onChange({ ...draft, on_failure: v })}
        />
        <dl className="choice-help muted small">
          <div><dt>Let the request through</dt><dd>Keeps apps running while the judge is down; each case is recorded.</dd></div>
          <div><dt>Refuse the request</dt><dd>Nothing unjudged gets through.</dd></div>
        </dl>
      </Controls>
      <p className="muted small">Each judge call is a request of that model and costs like one.</p>
      {canWrite && (
        <div className="actions">
          <SubmitHint id="judge-hint" missing={missing} />
          <Button
            variant="primary" size="sm" disabled={blocked}
            aria-describedby={missing.length > 0 ? "judge-hint" : undefined}
            onClick={onSave}
          >
            {saving ? "Saving…" : "Save judge settings"}
          </Button>
        </div>
      )}
    </>
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
  // Each section edits its own draft; a save sends the stored settings with that section's fields replaced.
  const [guardDraft, setGuardDraft] = useState<Pick<GuardrailSettings, "enabled" | "action"> | null>(null);
  const [answersDraft, setAnswersDraft] = useState<GuardrailSettings["answers"] | null>(null);
  const [judgeDraft, setJudgeDraft] = useState<GuardrailJudgeSettings | null>(null);
  const canWrite = useCan(P.aiConfigureAny);
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  const [presidioUrl, setPresidioUrl] = useState("");

  useEffect(() => {
    if (redactSettings.data && !redactDraft) {
      setRedactDraft(redactSettings.data);
      setPresidioUrl(redactSettings.data.presidio_url ?? "");
    }
  }, [redactSettings.data, redactDraft]);
  useEffect(() => {
    const g = guardSettings.data?.global;
    if (!g || guardDraft) return;
    setGuardDraft({ enabled: g.enabled, action: g.action });
    setAnswersDraft(g.answers);
    setJudgeDraft(g.judge);
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
  // Read-modify-write on the cached settings: the fields a section does not edit are sent as they were read.
  const saveGuard = useMutation({
    mutationFn: async ({ patch }: { patch: Partial<GuardrailSettings>; done?: string }) => {
      const stored = qc.getQueryData<GuardrailSettingsResponse>(["guardrails", "settings"])?.global
        ?? guardSettings.data?.global;
      if (!stored) throw new ApiError(0, "The settings are not loaded yet.");
      const next = { ...stored, ...patch };
      await apiFetch<void>("/guardrails/settings", { method: "PUT", body: JSON.stringify(next) });
      qc.setQueryData<GuardrailSettingsResponse>(["guardrails", "settings"], (old) => (old ? { ...old, global: next } : old));
    },
    onSuccess: (_v, { done }) => {
      if (done) toast.success(done);
      qc.invalidateQueries({ queryKey: ["guardrails", "settings"] });
    },
  });
  const failToast = (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Couldn't save guardrails.");
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

  const [patternDialog, setPatternDialog] = useState<"new" | GuardrailCustomPattern | null>(null);
  const [deletePattern, setDeletePattern] = useState<GuardrailCustomPattern | null>(null);
  const customPatterns = guardSettings.data?.global.custom_patterns ?? [];
  const customForRequests = customPatterns.filter((p) => p.direction !== "answer");
  const requestPatterns = (patterns.data ?? []).filter((p) => p.direction !== "answer");
  const answerPatterns = (patterns.data ?? []).filter((p) => p.direction === "answer");

  if (!redactDraft || !guardDraft || !answersDraft || !judgeDraft || !rules.data) {
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
            disabled={!canWrite}
            onChange={(v) => setRedactDraft({ ...redactDraft, enabled: v })}
          />
          <span>Enable redaction on request/response bodies</span>
        </div>
        <h3>Built-in rules</h3>
        <RulesTable name="Built-in rules" rules={rules.data.built_in} />
        <div className="row row-center gap-2 subsection-head">
          <h3>Custom rules</h3>
          {canWrite && <Button variant="secondary" size="sm" onClick={() => setRuleOpen(true)}>New rule</Button>}
        </div>
        <RulesTable name="Custom rules" rules={rules.data.custom} onDelete={canWrite ? setDeleteRule : undefined} />
        {canWrite && (
          <div className="actions">
            <Button variant="primary" size="sm" disabled={saveRedact.isPending} onClick={() => saveRedact.mutate(redactDraft)}>
              {saveRedact.isPending ? "Saving…" : "Save regex settings"}
            </Button>
          </div>
        )}
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
        title="Requests"
        subtitle={guardDraft.enabled ? `${requestPatterns.length + customForRequests.length} pattern${requestPatterns.length + customForRequests.length === 1 ? "" : "s"}` : "Disabled"}
      >
        <Controls canWrite={canWrite}>
          <div className="row row-center gap-2">
            <Switch
              aria-label="Enable injection guardrails"
              aria-describedby="injection-help"
              checked={guardDraft.enabled}
              onChange={(v) => setGuardDraft({ ...guardDraft, enabled: v })}
            />
            <span>Inspect prompts for injection patterns</span>
          </div>
          <p id="injection-help" className="muted small">
            Applies to all model traffic, unless a service turns on its own guardrails. Answers and the judge have
            switches of their own: turning this off leaves them as they are.
          </p>
          <FormField label="On detection" htmlFor="injection-action" w="md">
            <Select
              id="injection-action"
              value={guardDraft.action}
              onChange={(v) => setGuardDraft({ ...guardDraft, action: v as GuardrailSettings["action"] })}
              options={ACTION_OPTIONS}
            />
          </FormField>
        </Controls>
        <PatternList patterns={requestPatterns} />
        <h3>Your patterns</h3>
        {customForRequests.length === 0
          ? <p className="muted small">None apply to requests. Add one under Custom patterns.</p>
          : (
            <ul className="mono small" aria-label="Your patterns">
              {customForRequests.map((p) => <li key={p.id}>{p.description ? `${p.id} · ${p.description}` : p.id}</li>)}
            </ul>
          )}
        {canWrite && (
          <div className="actions">
            <Button
              variant="primary" size="sm" disabled={saveGuard.isPending}
              onClick={() => saveGuard.mutate({ patch: guardDraft, done: "Prompt-injection settings saved." }, { onError: failToast })}
            >
              {saveGuard.isPending ? "Saving…" : "Save guardrails"}
            </Button>
          </div>
        )}
      </Accordion>

      <Accordion
        id="answers"
        title="Answers"
        subtitle={answersDraft.enabled ? (answersDraft.action === "block" ? "Block" : "Flag") : "Off"}
      >
        <Controls canWrite={canWrite}>
          <div className="row row-center gap-2">
            <Switch
              aria-label="Inspect answers"
              checked={answersDraft.enabled}
              onChange={(v) => setAnswersDraft({ ...answersDraft, enabled: v })}
            />
            <span>Inspect answers</span>
          </div>
          <h3 id="answers-action-label">When a pattern matches</h3>
          <Segmented
            aria-label="When a pattern matches"
            options={ANSWER_ACTIONS.map((o) => ({ value: o.value, label: o.label }))}
            value={answersDraft.action}
            onChange={(v) => setAnswersDraft({ ...answersDraft, action: v })}
          />
          <dl className="choice-help muted small">
            {ANSWER_ACTIONS.map((o) => (
              <div key={o.value}><dt>{o.label}</dt><dd>{o.help}</dd></div>
            ))}
          </dl>
        </Controls>
        <h3>Bundled answer patterns</h3>
        <ul className="mono small" aria-label="Bundled answer patterns">
          {answerPatterns.map((p) => <li key={p.id}>{p.id} · {p.description}</li>)}
        </ul>
        {canWrite && (
          <div className="actions">
            <Button
              variant="primary" size="sm" disabled={saveGuard.isPending}
              onClick={() => saveGuard.mutate({ patch: { answers: answersDraft }, done: "Answer settings saved." }, { onError: failToast })}
            >
              {saveGuard.isPending ? "Saving…" : "Save answer settings"}
            </Button>
          </div>
        )}
      </Accordion>

      <Accordion
        id="custom-patterns"
        title="Custom patterns"
        subtitle={`${customPatterns.length} of ${MAX_CUSTOM_PATTERNS}`}
      >
        <div className="row row-center gap-2 subsection-head">
          <span>{customPatterns.length} of {MAX_CUSTOM_PATTERNS}</span>
          {canWrite && (
            <Button
              variant="secondary" size="sm"
              disabled={customPatterns.length >= MAX_CUSTOM_PATTERNS}
              onClick={() => setPatternDialog("new")}
            >
              Add pattern
            </Button>
          )}
        </div>
        {canWrite && customPatterns.length >= MAX_CUSTOM_PATTERNS && (
          <p className="muted small" role="status">At most {MAX_CUSTOM_PATTERNS} patterns. Delete one to add another.</p>
        )}
        <div className="table-wrap">
          <table className="data rules-table" aria-label="Custom patterns">
            <thead>
              <tr>
                <th>ID</th><th>Applies to</th><th>Description</th><th>Pattern</th>
                <th className="col-actions"></th>
              </tr>
            </thead>
            <tbody>
              {customPatterns.length === 0
                ? <TableEmptyRow colSpan={5} title="No custom patterns yet." />
                : customPatterns.map((p) => (
                    <tr key={p.id}>
                      <td className="cell-wrap"><code className="mono">{p.id}</code></td>
                      <td>{DIRECTION_LABEL[p.direction] ?? p.direction}</td>
                      <td className="cell-wrap">{p.description}</td>
                      <td className="cell-wrap"><code className="mono small">{p.pattern}</code></td>
                      <td className="col-actions">
                        {canWrite && (
                          <>
                            <button type="button" className="icon-btn" aria-label={`Edit pattern ${p.id}`} onClick={() => setPatternDialog(p)}>
                              <Pencil size={13} />
                            </button>
                            <button type="button" className="icon-btn" aria-label={`Delete pattern ${p.id}`} onClick={() => setDeletePattern(p)}>
                              <Trash2 size={13} />
                            </button>
                          </>
                        )}
                      </td>
                    </tr>
                  ))}
            </tbody>
          </table>
        </div>
      </Accordion>

      <Accordion
        id="judge"
        title="Judge"
        subtitle={judgeDraft.enabled ? (judgeDraft.model || "No model") : "Off"}
      >
        <JudgeSection
          draft={judgeDraft}
          onChange={setJudgeDraft}
          models={models.data}
          canWrite={canWrite}
          saving={saveGuard.isPending}
          onSave={() => saveGuard.mutate({ patch: { judge: judgeDraft }, done: "Judge settings saved." }, { onError: failToast })}
        />
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

      {patternDialog !== null && (
        <CustomPatternDialog
          pattern={patternDialog === "new" ? undefined : patternDialog}
          takenIds={[...customPatterns.map((p) => p.id), ...(patterns.data ?? []).map((p) => p.id)]}
          onClose={() => setPatternDialog(null)}
          onSave={async (next) => {
            const list = patternDialog === "new"
              ? [...customPatterns, next]
              : customPatterns.map((p) => (p.id === patternDialog.id ? next : p));
            await saveGuard.mutateAsync({ patch: { custom_patterns: list }, done: "Pattern saved." });
            setPatternDialog(null);
          }}
        />
      )}
      <Dialog
        open={deletePattern !== null}
        onOpenChange={(o) => { if (!o) setDeletePattern(null); }}
        title="Delete pattern?"
        size="sm"
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeletePattern(null)}>Cancel</Button>
            <Button
              variant="destructive"
              disabled={saveGuard.isPending}
              onClick={() => {
                if (deletePattern) {
                  saveGuard.mutate(
                    { patch: { custom_patterns: customPatterns.filter((p) => p.id !== deletePattern.id) }, done: "Pattern deleted." },
                    { onError: failToast },
                  );
                }
                setDeletePattern(null);
              }}
            >
              Delete
            </Button>
          </>
        }
      >
        <p>Delete <code className="mono">{deletePattern?.id}</code>? It will no longer be checked.</p>
      </Dialog>

      <Toaster />
    </div>
  );
}
