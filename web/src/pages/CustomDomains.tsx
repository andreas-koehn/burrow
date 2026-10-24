import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { Badge, Button, ErrorNotice, FormFieldGroup, Input, Dialog, Segmented, SkeletonRows, TableEmptyRow } from "@/components/ds";
import { CopyButton } from "@/components/CopyableCode";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { grpcAddress } from "@/lib/grpc";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { CertPemEditor } from "@/components/CertPemEditor";
import type { CertPemEditorValue } from "@/components/CertPemEditor";
import type { CertSource, CertStatus, CustomDomain, CustomDomainRejection } from "@/lib/contract";

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

// The status of an automatic certificate: ready once the relay holds one, pending until then,
// not needed where a proxy in front terminates TLS.
const CERT_STATUS_LABEL: Record<CertStatus, string> = {
  ready: "Ready",
  pending: "Waiting for a certificate",
  not_needed: "Handled by the proxy",
};
const CERT_STATUS_KIND: Record<CertStatus, string> = {
  ready: "status-connected",
  pending: "status-idle",
  not_needed: "status-idle",
};

function truncateFp(fp: string): string {
  if (fp.length <= 12) return fp;
  return fp.slice(0, 12) + "…";
}

const emptyPem: CertPemEditorValue = { cert_pem: "", key_pem: "" };

export function CustomDomainsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const domainsKey = ["service", serviceId, "domains"] as const;

  // The relay lets the owner (:own) and the holder of :any write domains; an automatic certificate
  // is for :any only, because it proves nothing about who controls the name.
  const canWrite = useCan([P.servicesConfigureOwn, P.servicesConfigureAny]);
  const canAuto = useCan(P.servicesConfigureAny);

  const { data, isError } = useQuery({
    queryKey: domainsKey,
    queryFn: () => apiFetch<CustomDomain[]>(`/services/${serviceId}/domains`),
    staleTime: 30_000,
  });

  // Add-domain dialog
  const [adding, setAdding] = useState(false);
  const [hostname, setHostname] = useState("");
  const [source, setSource] = useState<CertSource>("uploaded");
  const [pem, setPem] = useState<CertPemEditorValue>(emptyPem);
  const [rejectMsg, setRejectMsg] = useState<string | null>(null);

  // Delete confirmation dialog
  const [deleteTarget, setDeleteTarget] = useState<CustomDomain | null>(null);

  function openAdd() {
    setHostname("");
    setSource("uploaded");
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
      // apiFetch, so that a missing password confirmation opens the shared dialog.
      try {
        return await apiFetch<CustomDomain>(`/services/${serviceId}/domains`, {
          method: "POST",
          body: JSON.stringify(
            source === "auto"
              ? { hostname, cert_source: "auto" }
              : { hostname, cert_pem: pem.cert_pem, key_pem: pem.key_pem },
          ),
        });
      } catch (e) {
        if (e instanceof ApiError && e.status === 400) {
          const j = (e.body ?? {}) as Partial<CustomDomainRejection>;
          if (j.reason && j.reason in REJECTION_MESSAGES) throw new DomainRejectionError(REJECTION_MESSAGES[j.reason]);
        }
        // 400, 403 (automatic certificates are for administrators), 409 (the name has another owner)
        // and 429 (the relay's caps): the relay's own text says what to change.
        if (e instanceof ApiError && [400, 403, 409, 429].includes(e.status) && e.message) throw new DomainRejectionError(e.message);
        throw e;
      }
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
        {canWrite && (
          <Button variant="primary" size="sm" onClick={openAdd}>
            New domain
          </Button>
        )}
      </div>
      <p className="help">
        Certificates here apply only to this service. A gRPC client dials a custom domain on port 443
        (<span className="mono">host:443</span>).
      </p>

      <div className="table-wrap">
        <table className="data" aria-label="Custom domains">
          <thead>
            <tr>
              <th>Hostname</th>
              <th>Certificate</th>
              <th>Status</th>
              <th>Expires</th>
              <th>Fingerprint</th>
              <th>gRPC address</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {domains.map((d) => {
              // A relay from before automatic certificates leaves the field out: the certificate was uploaded.
              const auto = d.cert_source === "auto";
              const addr = grpcAddress(d.hostname);
              return (
                <tr key={d.id}>
                  <td className="mono">{d.hostname}</td>
                  <td>{auto ? "Automatic" : "Uploaded"}</td>
                  <td>
                    {auto ? (
                      <Badge kind={CERT_STATUS_KIND[d.cert_status ?? "pending"]}>
                        {CERT_STATUS_LABEL[d.cert_status ?? "pending"]}
                      </Badge>
                    ) : (
                      <Badge kind={statusBadgeKind(d.status)}>
                        {STATUS_LABEL[d.status]}
                      </Badge>
                    )}
                  </td>
                  <td>{auto ? "—" : formatTimestamp(d.not_after)}</td>
                  <td className="mono">{auto ? "—" : truncateFp(d.cert_sha256)}</td>
                  <td>
                    {addr ? (
                      <span className="row row-center gap-1">
                        <span className="mono small">{addr}</span>
                        <CopyButton text={addr} label={`Copy gRPC address ${addr}`} />
                      </span>
                    ) : "—"}
                  </td>
                  <td className="col-actions">
                    {canWrite && (
                      <Button
                        variant="destructive"
                        size="sm"
                        aria-label={`Delete domain ${d.hostname}`}
                        onClick={() => setDeleteTarget(d)}
                      >
                        Delete
                      </Button>
                    )}
                  </td>
                </tr>
              );
            })}
            {!data && !isError && (
              <tr><td colSpan={7}><SkeletonRows n={3} /></td></tr>
            )}
            {isError && !data && (
              <tr><td colSpan={7}><ErrorNotice>Could not load custom domains.</ErrorNotice></td></tr>
            )}
            {data && data.length === 0 && (
              <TableEmptyRow colSpan={7} title="No custom domains yet.">
                Add a domain to serve this service on your own hostname.
              </TableEmptyRow>
            )}
          </tbody>
        </table>
      </div>

      {/* Add domain dialog */}
      <Dialog
        open={adding}
        onOpenChange={(o) => { if (!o) closeAdd(); }}
        title="Add custom domain"
        description={source === "auto"
          ? "Supply a hostname. The relay orders its certificate itself."
          : "Supply a hostname and the TLS certificate + private key in PEM format."}
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
          {canAuto ? (
            <Segmented
              aria-label="Certificate source"
              options={[{ value: "uploaded", label: "Upload a certificate" }, { value: "auto", label: "Automatic (Let's Encrypt)" }]}
              value={source}
              onChange={(v) => { setSource(v); setRejectMsg(null); }}
            />
          ) : (
            <p className="muted small">Automatic certificates are set up by an administrator; here you upload one.</p>
          )}
          {source === "auto" ? (
            <p className="muted small">
              The relay orders the certificate itself once the name&apos;s DNS points at this relay and port 80 or 443 reaches it.
              Until then the status reads Waiting for a certificate. Behind a proxy that ends TLS no certificate is needed.
            </p>
          ) : (
            <CertPemEditor value={pem} onChange={setPem} />
          )}
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
