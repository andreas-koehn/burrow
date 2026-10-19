import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Checkbox, ErrorNotice, PageHeader } from "@/components/ds";
import type { SettingsMap } from "@/lib/contract";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

export default function GeneralSettings() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["settings"], queryFn: () => apiFetch<SettingsMap>("/settings"), retry: false });

  // v0.5.1 Q12 (UI landed in v0.5.2): connection-log privacy toggle for the
  // per-day top-source-IPs aggregation. Default-true policy applied client-
  // side when the key is absent (matches the backend reader).
  const servicesDomain = data?.["services_domain"];
  const topIPsEnabled =
    (data?.["connection_logs.rollup_include_top_ips"] ?? "true") !== "false";
  const togglePrivacy = useMutation({
    mutationFn: (next: boolean) =>
      apiFetch("/settings", {
        method: "PUT",
        body: JSON.stringify({
          "connection_logs.rollup_include_top_ips": next ? "true" : "false",
        }),
      }),
    onSuccess: (_data, next) => {
      // Update the cached settings so the toggle reflects the new value
      // without waiting for the GET refetch round-trip.
      // Nothing cached (the GET failed): leave it so, rather than a map of this one
      // key that other readers of ["settings"] would take for the relay's settings.
      qc.setQueryData<SettingsMap>(["settings"], (f) => f && {
        ...f,
        "connection_logs.rollup_include_top_ips": next ? "true" : "false",
      });
      void qc.invalidateQueries({ queryKey: ["settings"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Save failed"),
  });

  return (
    <div className="account-page settings-page settings-form">
      <PageHeader title="General" subtitle="Settings that apply to the whole relay." />

      {/* ---- v0.5.2 Privacy section (Q12 toggle for connection-log top-source-IPs) ---- */}
      <section className="account-section" aria-labelledby="sec-privacy">
        <div className="section-head"><div className="left"><h2 id="sec-privacy">Privacy</h2></div></div>
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
      </section>

      {/* D27. The relay only reports its services domain on newer versions: no field, no line. */}
      {servicesDomain !== undefined && (
        <section className="account-section" aria-labelledby="sec-services-address">
          <div className="section-head"><div className="left"><h2 id="sec-services-address">Services address</h2></div></div>
          {servicesDomain !== "" ? (
            <p>
              Published services are served from <code className="mono">{servicesDomain}</code>, apart from the dashboard.
              A page of a service cannot act as the signed-in dashboard user.
            </p>
          ) : (
            <ErrorNotice variant="warn" role="note">
              Published services share the dashboard&apos;s address, so a page one of them serves can act as the signed-in
              user. Set <code className="mono">BURROW_SERVICES_DOMAIN</code> to a separate name to keep them apart. Until then,
              approving a machine or an app asks for your password.
            </ErrorNotice>
          )}
        </section>
      )}
      <Toaster />
    </div>
  );
}
