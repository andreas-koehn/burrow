import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { Badge, Button, ErrorNotice, FormFieldGroup, Input, Dialog } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { CertPemEditor } from "@/components/CertPemEditor";
import type { CertPemEditorValue } from "@/components/CertPemEditor";
import type { CustomDomain, CustomDomainRejection } from "@/lib/contract";

// ---- Verbatim rejection messages ----
const REJECTION_MESSAGES: Record<CustomDomainRejection["reason"], string> = {
  san_mismatch: "The certificate's SAN does not include this hostname.",
  chain_invalid: "The certificate chain failed to validate against the system roots.",
  key_mismatch: "The private key does not match the certificate's public key.",
  expired: "The certificate's validity window has already ended.",
};

// v0.5.2 Task 10: 4-state badge map for the custom-domain status enum.
// Uses existing badge tokens — pending is neutral (idle), cert_expiring is
// idle (yellow-ish), cert_expired is suspended (red), active is connected
// (green). Exhaustive switch — the `never` annotation guarantees TS will
// complain if a new state is added to CustomDomainStatus without a badge.
export function statusBadgeKind(status: CustomDomain["status"]): string {
  switch (status) {
    case "active":        return "status-connected";
    case "pending":       return "status-idle";
    case "cert_expiring": return "status-idle";
    case "cert_expired":  return "status-suspended";
    default: {
      const _exhaustive: never = status;
      return _exhaustive;
    }
  }
}

// Human label for each status — keeps snake_case out of the UI.
export const STATUS_LABEL: Record<CustomDomain["status"], string> = {
  active:        "Active",
  pending:       "Pending",
  cert_expiring: "Expiring",
  cert_expired:  "Expired",
};

function truncateFp(fp: string): string {
  if (fp.length <= 12) return fp;
  return fp.slice(0, 12) + "…";
}

const emptyPem: CertPemEditorValue = { cert_pem: "", key_pem: "" };

export function CustomDomainsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const domainsKey = ["service", serviceId, "domains"] as const;

  const { data } = useQuery({
    queryKey: domainsKey,
    queryFn: () => apiFetch<CustomDomain[]>(`/services/${serviceId}/domains`),
    staleTime: 30_000,
  });

  // Add-domain dialog
  const [adding, setAdding] = useState(false);
  const [hostname, setHostname] = useState("");
  const [pem, setPem] = useState<CertPemEditorValue>(emptyPem);
  const [rejectMsg, setRejectMsg] = useState<string | null>(null);

  // Delete confirmation dialog
  const [deleteTarget, setDeleteTarget] = useState<CustomDomain | null>(null);

  function openAdd() {
    setHostname("");
    setPem(emptyPem);
    setRejectMsg(null);
    setAdding(true);
  }
  function closeAdd() {
    setAdding(false);
    setRejectMsg(null);
  }

  // Capture rejection reason in mutationFn and surface it as a verbatim inline alert.
  const addDomainWithRejection = useMutation({
    mutationFn: async () => {
      const res = await fetch(`/api/v1/services/${serviceId}/domains`, {
        method: "POST",
        credentials: "include",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": (() => {
            const m = document.cookie.split(";").find((c) => c.trim().startsWith("burrow_csrf="));
            return m ? m.trim().slice("burrow_csrf=".length) : "";
          })(),
        },
        body: JSON.stringify({ hostname, cert_pem: pem.cert_pem, key_pem: pem.key_pem }),
      });
      if (res.status === 400) {
        const j = await res.json() as Partial<CustomDomainRejection>;
        if (j.reason && j.reason in REJECTION_MESSAGES) {
          throw new DomainRejectionError(REJECTION_MESSAGES[j.reason]);
        }
        throw new Error((j as { error?: string }).error ?? "Bad request");
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json() as Promise<CustomDomain>;
    },
    onSuccess: () => {
      closeAdd();
      void qc.invalidateQueries({ queryKey: domainsKey });
      toast.success("Domain added.");
    },
    onError: (e) => {
      if (e instanceof DomainRejectionError) {
        setRejectMsg(e.message);
      } else {
        toast.error("Could not add domain.");
      }
    },
  });

  const deleteDomain = useMutation({
    mutationFn: (did: string) =>
      apiFetch(`/services/${serviceId}/domains/${did}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleteTarget(null);
      void qc.invalidateQueries({ queryKey: domainsKey });
      toast.success("Domain removed.");
    },
    onError: () => toast.error("Could not remove domain."),
  });

  const domains = data ?? [];

  return (
    <div className="custom-domains-panel">
      <div className="panel-head">
        <h3>Custom domains</h3>
        <Button variant="primary" size="sm" onClick={openAdd}>
          Add domain
        </Button>
      </div>
      <p className="help">Certificates here apply only to this service.</p>

      <div className="table-wrap">
        <table className="data" aria-label="Custom domains">
          <thead>
            <tr>
              <th>Hostname</th>
              <th>Status</th>
              <th>Expires</th>
              <th>Fingerprint</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {domains.map((d) => (
              <tr key={d.id}>
                <td className="mono small">{d.hostname}</td>
                <td>
                  <Badge kind={statusBadgeKind(d.status)}>
                    {STATUS_LABEL[d.status]}
                  </Badge>
                </td>
                <td>{formatTimestamp(d.not_after)}</td>
                <td className="mono small">{truncateFp(d.cert_sha256)}</td>
                <td className="col-actions">
                  <Button
                    variant="destructive"
                    size="sm"
                    aria-label={`Delete domain ${d.hostname}`}
                    onClick={() => setDeleteTarget(d)}
                  >
                    Delete
                  </Button>
                </td>
              </tr>
            ))}
            {domains.length === 0 && (
              <tr>
                <td colSpan={5} className="muted">No custom domains yet.</td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {/* Add domain dialog */}
      <Dialog
        open={adding}
        onOpenChange={(o) => { if (!o) closeAdd(); }}
        title="Add custom domain"
        description="Supply a hostname and the TLS certificate + private key in PEM format."
        footer={
          <>
            <Button variant="secondary" onClick={closeAdd}>Cancel</Button>
            <Button
              variant="primary"
              disabled={!hostname || addDomainWithRejection.isPending}
              onClick={() => { setRejectMsg(null); addDomainWithRejection.mutate(); }}
            >
              {addDomainWithRejection.isPending ? "Adding…" : "Add"}
            </Button>
          </>
        }
      >
        <FormFieldGroup>
          <div className="field">
            <label htmlFor="domain-hostname">Hostname</label>
            <Input
              id="domain-hostname"
              aria-label="Hostname"
              mono
              placeholder="e.g. api.example.com"
              value={hostname}
              onChange={(e) => setHostname(e.target.value)}
            />
          </div>
          <CertPemEditor value={pem} onChange={setPem} />
          {rejectMsg && (
            <ErrorNotice>{rejectMsg}</ErrorNotice>
          )}
        </FormFieldGroup>
      </Dialog>

      {/* Delete confirmation dialog */}
      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        title="Remove custom domain"
        description={deleteTarget ? `Remove ${deleteTarget.hostname} from this service?` : ""}
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteTarget(null)}>Cancel</Button>
            <Button
              variant="destructive"
              disabled={deleteDomain.isPending}
              onClick={() => deleteTarget && deleteDomain.mutate(deleteTarget.id)}
            >
              {deleteDomain.isPending ? "Removing…" : "Remove"}
            </Button>
          </>
        }
      >
        <p className="muted">This cannot be undone. Traffic to this hostname will no longer use this certificate.</p>
      </Dialog>

      <Toaster />
    </div>
  );
}

// ---- internal error type for rejection-reason surfacing ----
class DomainRejectionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DomainRejectionError";
  }
}
