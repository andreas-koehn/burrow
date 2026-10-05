import { useEffect, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Input, FormField, FormFieldGroup, PageHeader } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import type { RetentionSettings } from "@/lib/contract";

// Per-field range definitions (min, max). min of 0 means 0 is allowed (= ∞).
const FIELD_META: {
  key: keyof Omit<RetentionSettings, "audit_retention_note">;
  label: string;
  min: number;
  max: number;
  placeholder: string;
}[] = [
  { key: "audit_retention_days",                   label: "Audit log (days)",              min: 0, max: 3650, placeholder: "0" },
  { key: "usage_retention_days",                   label: "Usage events (days)",            min: 1, max: 3650, placeholder: "90" },
  { key: "redaction_retention_days",               label: "Redaction events (days)",        min: 1, max: 3650, placeholder: "30" },
  { key: "connection_logs_retention_days",         label: "Connection logs (days)",         min: 1, max: 3650, placeholder: "30" },
  { key: "connection_logs_rollups_retention_days", label: "Connection log rollups (days)",  min: 0, max: 3650, placeholder: "0" },
  { key: "webhook_deliveries_retention_days",      label: "Webhook deliveries (days)",      min: 1, max: 365,  placeholder: "30" },
  { key: "inspector_retention_count",              label: "Inspector ring-buffer size",     min: 1, max: 1000, placeholder: "100" },
];

function isOutOfRange(value: number, min: number, max: number): boolean {
  return isNaN(value) || value < min || value > max;
}

const BACK = { to: "/settings/general", label: "Settings" } as const;

export default function Retention() {
  const qc = useQueryClient();

  const { data } = useQuery({
    queryKey: ["settings", "retention"],
    queryFn: () => apiFetch<RetentionSettings>("/settings/retention"),
    retry: false,
  });

  // Local form state: string values per field so the input stays controlled.
  type FormState = Record<string, string>;
  const [form, setForm] = useState<FormState>({});

  // v0.5.1 P3.7 (deferred to v0.5.2): per-field touched gate so validation
  // errors do not surface on first paint or while the user is still typing.
  // A field becomes touched on blur (or on form submit — see onSubmit
  // handler below where every field is marked touched at once).
  type TouchedState = Record<string, boolean>;
  const [touched, setTouched] = useState<TouchedState>({});

  useEffect(() => {
    if (data) {
      const s: FormState = {};
      for (const { key } of FIELD_META) {
        s[key] = String(data[key]);
      }
      setForm(s);
    }
  }, [data]);

  const set = (k: string, v: string) => setForm((f) => ({ ...f, [k]: v }));
  const markTouched = (k: string) => setTouched((t) => (t[k] ? t : { ...t, [k]: true }));

  // Determine which fields are out of range. The errors map is computed for
  // ALL fields (used by hasErrors / save disable); rendering is then gated
  // per-field on `touched[key]`.
  const errors: Partial<Record<string, string>> = {};
  for (const { key, min, max, label } of FIELD_META) {
    const v = Number(form[key]);
    if (form[key] !== undefined && isOutOfRange(v, min, max)) {
      errors[key] = `${label}: must be between ${min} and ${max}`;
    }
  }
  const hasErrors = Object.keys(errors).length > 0;

  const save = useMutation({
    mutationFn: () => {
      const payload: Partial<RetentionSettings> = {};
      for (const { key } of FIELD_META) {
        (payload as Record<string, number>)[key] = Number(form[key]);
      }
      return apiFetch("/settings/retention", { method: "PUT", body: JSON.stringify(payload) });
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["settings", "retention"] });
      toast.success("Retention settings saved.");
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Save failed"),
  });

  return (
    <div className="account-page page-narrow">
      <PageHeader
        back={BACK}
        title="Retention & compliance"
        subtitle="Configure how long Burrow retains logs and ring-buffer entries."
      />

      <section className="account-section">
        <form
          className="pw-form"
          onSubmit={(e) => {
            e.preventDefault();
            // On submit, mark every field touched so any errors surface.
            const allTouched: TouchedState = {};
            for (const { key } of FIELD_META) allTouched[key] = true;
            setTouched(allTouched);
            if (!hasErrors) save.mutate();
          }}
        >
          <FormFieldGroup>
            {FIELD_META.map(({ key, label, min, max, placeholder }) => {
              // v0.5.1 P3.7: only render the inline error after the user has
              // blurred the field (or submitted the form). The `invalid`
              // styling on <Input> follows the same gate so first-paint is
              // clean even when a backend default is out of range.
              const hasError = key in errors;
              const showError = hasError && touched[key];
              const help = key === "audit_retention_days" ? data?.audit_retention_note : undefined;
              return (
                <FormField
                  key={key}
                  label={label}
                  htmlFor={`ret-${key}`}
                  w="sm"
                  help={help}
                  error={showError ? errors[key] : undefined}
                >
                  <Input
                    id={`ret-${key}`}
                    type="number"
                    mono
                    min={min}
                    max={max}
                    placeholder={placeholder}
                    value={form[key] ?? ""}
                    onChange={(e) => set(key, e.target.value)}
                    onBlur={() => markTouched(key)}
                    invalid={showError}
                    aria-invalid={showError || undefined}
                  />
                </FormField>
              );
            })}
          </FormFieldGroup>

          <div className="actions">
            <Button
              type="submit"
              variant="primary"
              disabled={hasErrors || save.isPending}
            >
              {save.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </form>
      </section>

      <Toaster />
    </div>
  );
}
