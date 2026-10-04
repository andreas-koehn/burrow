import { useEffect, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Checkbox, Input, Select, FormField, FormFieldGroup, PageHeader, InfoHint } from "@/components/ds";
import type { SettingsMap } from "@/lib/contract";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

export default function Settings() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["settings"], queryFn: () => apiFetch<SettingsMap>("/settings"), retry: false });
  const [form, setForm] = useState<SettingsMap>({});
  const [showTest, setShowTest] = useState(false);
  const [testTo, setTestTo] = useState("");
  const [testError, setTestError] = useState("");

  useEffect(() => { if (data) setForm({ ...data }); }, [data]);
  const set = (k: string, v: string) => setForm((f) => ({ ...f, [k]: v }));
  const configured = !!form["smtp.host"];

  const save = useMutation({
    mutationFn: () => apiFetch("/settings", { method: "PUT", body: JSON.stringify(form) }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["settings"] }); toast.success("Email settings saved."); },
    onError: (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Save failed"),
  });

  // v0.5.1 Q12 (UI landed in v0.5.2): connection-log privacy toggle for the
  // per-day top-source-IPs aggregation. Default-true policy applied client-
  // side when the key is absent (matches the backend reader).
  const topIPsEnabled =
    (form["connection_logs.rollup_include_top_ips"] ?? "true") !== "false";
  const togglePrivacy = useMutation({
    mutationFn: (next: boolean) =>
      apiFetch("/settings", {
        method: "PUT",
        body: JSON.stringify({
          "connection_logs.rollup_include_top_ips": next ? "true" : "false",
        }),
      }),
    onSuccess: (_data, next) => {
      // Update local form so the toggle UI reflects the new value
      // without waiting for the GET refetch round-trip.
      setForm((f) => ({
        ...f,
        "connection_logs.rollup_include_top_ips": next ? "true" : "false",
      }));
      void qc.invalidateQueries({ queryKey: ["settings"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Save failed"),
  });
  const test = useMutation({
    mutationFn: () => apiFetch("/settings/test-email", { method: "POST", body: JSON.stringify({ to: testTo }) }),
    onSuccess: () => { setTestError(""); toast.success(`Sent a test email to ${testTo}.`); },
    onError: (e: unknown) => setTestError(e instanceof ApiError ? e.message : "Test failed"),
  });

  return (
    <div className="account-page">
      <PageHeader title="Settings" subtitle="Admin-only configuration for this Burrow relay." />

      {/* ---- General section: Privacy + Email/SMTP (above the nav cards) ---- */}
      <section className="account-section" aria-labelledby="sec-general">
        <div className="section-head"><div className="left"><h2 id="sec-general">General</h2></div></div>

        {/* ---- v0.5.2 Privacy section (Q12 toggle for connection-log top-source-IPs) ---- */}
        <div className="section-head sub"><div className="left"><h3 id="sec-privacy">Privacy</h3></div></div>
        <div className="form-field">
          <label htmlFor="rollup-include-top-ips" className="checkbox-row">
            <Checkbox
              id="rollup-include-top-ips"
              checked={topIPsEnabled}
              onChange={(v) => { if (!togglePrivacy.isPending) togglePrivacy.mutate(v); }}
            />
            <span>Include top source IPs in daily connection-log rollups</span>
          </label>
          <p className="help">
            When enabled, the daily rollup includes the top 10 source IPs per
            service. Turn off for stricter privacy. Default-on.
          </p>
        </div>

        <div className="section-head sub">
          <div className="left">
            <h3 id="sec-smtp">Email / SMTP</h3>
            <InfoHint label="Email / SMTP" content="SMTP enables password-reset and test emails." />
          </div>
        </div>
        {!configured && <p role="status" className="notice-inline">Email isn't set up — password resets and test emails are unavailable until you configure SMTP.</p>}
        <form className="pw-form" onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
          <FormFieldGroup>
            <FormField label="SMTP server" htmlFor="smtp-host" w="full">
              <Input id="smtp-host" value={form["smtp.host"] ?? ""} onChange={(e) => set("smtp.host", e.target.value)} placeholder="smtp.example.com" />
            </FormField>
            <FormField label="Port" htmlFor="smtp-port" w="sm">
              <Input id="smtp-port" inputMode="numeric" value={form["smtp.port"] ?? ""} onChange={(e) => set("smtp.port", e.target.value)} placeholder="587" />
            </FormField>
            <FormField label="Encryption" htmlFor="smtp-enc" w="full">
              <Select id="smtp-enc"
                options={[{ value: "starttls", label: "STARTTLS" }, { value: "implicit", label: "Implicit TLS" }, { value: "none", label: "None" }]}
                value={form["smtp.tls"] ?? "starttls"} onChange={(v) => set("smtp.tls", v)} />
            </FormField>
            <FormField label="Username" htmlFor="smtp-user" w="full">
              <Input id="smtp-user" value={form["smtp.username"] ?? ""} onChange={(e) => set("smtp.username", e.target.value)} placeholder="burrow@example.com" />
            </FormField>
            <FormField label="From address" htmlFor="smtp-from" w="full">
              <Input id="smtp-from" value={form["smtp.from"] ?? ""} onChange={(e) => set("smtp.from", e.target.value)} placeholder="burrow@example.com" />
            </FormField>
          </FormFieldGroup>
          <p className="muted">Password is supplied via BURROW_SMTP_PASSWORD or BURROW_SMTP_PASSWORD_FILE. We never echo a stored secret.</p>
          <div className="actions">
            <Button type="submit" variant="primary" disabled={save.isPending}>Save settings</Button>
          </div>
        </form>

        <div className="section-head sub"><div className="left"><h3>Test connection</h3></div></div>
        {!showTest ? (
          <Button variant="secondary" size="sm" onClick={() => setShowTest(true)}>Send test email</Button>
        ) : (
          <div className="row row-end gap-2">
            <FormField label="Test recipient" htmlFor="smtp-test-to" w="full">
              <Input id="smtp-test-to" value={testTo} onChange={(e) => setTestTo(e.target.value)} placeholder="you@example.com" />
            </FormField>
            <Button variant="secondary" size="sm" disabled={test.isPending} onClick={() => { setTestError(""); test.mutate(); }}>
              {test.isPending ? "Testing…" : "Test now"}
            </Button>
          </div>
        )}
        {testError && <p role="alert" className="field-error">{testError}</p>}
      </section>

      {/* ---- v0.5.0 nav cards ---- */}
      <section className="account-section" aria-labelledby="sec-configuration">
        <div className="section-head"><div className="left"><h2 id="sec-configuration">Configuration</h2></div></div>
        <div className="settings-nav-grid">
          <Link to="/settings/retention" className="settings-nav-card">
            <div className="settings-nav-card-title">Retention &amp; compliance</div>
            <div className="settings-nav-card-desc muted">Audit log, usage events, inspector ring buffer, and other retention knobs.</div>
          </Link>
          <Link to="/settings/database" className="settings-nav-card">
            <div className="settings-nav-card-title">Database backend</div>
            <div className="settings-nav-card-desc muted">Driver in use (SQLite default; Postgres alpha).</div>
          </Link>
          <Link to="/settings/backups" className="settings-nav-card">
            <div className="settings-nav-card-title">Backup &amp; restore</div>
            <div className="settings-nav-card-desc muted">Snapshots of the relay&apos;s SQLite database.</div>
          </Link>
          <Link to="/openapi" className="settings-nav-card">
            <div className="settings-nav-card-title">OpenAPI viewer</div>
            <div className="settings-nav-card-desc muted">Browse the JSON/HTTP API docs.</div>
          </Link>
          <Link to="/connection-logs" className="settings-nav-card">
            <div className="settings-nav-card-title">Connection logs</div>
            <div className="settings-nav-card-desc muted">Per-tunnel HTTP/TCP/control connection history.</div>
          </Link>
          <Link to="/settings/custom-domains" className="settings-nav-card">
            <div className="settings-nav-card-title">Custom domains (all services)</div>
            <div className="settings-nav-card-desc muted">Read-only roll-up of every service&apos;s domains; open a service to edit.</div>
          </Link>
        </div>
      </section>
      <Toaster />
    </div>
  );
}
