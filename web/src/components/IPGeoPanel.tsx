import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, Input, Select } from "@/components/ds";
import type { IpGeoConfig } from "@/lib/contract";

interface GeoStatus {
  enabled: boolean;
  db_path: string;
  db_age_seconds: number;
}

const CIDR_RE = /^[0-9.]+\/\d{1,2}$|^[0-9a-f:]+\/\d{1,3}$/i;

const LIST_OPTIONS = [
  { value: "allow_cidrs", label: "Allow CIDR" },
  { value: "block_cidrs", label: "Block CIDR" },
];

export function IPGeoPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const cfg = useQuery({
    queryKey: ["service", serviceId, "ipgeo"],
    queryFn: () => apiFetch<IpGeoConfig>(`/services/${serviceId}/ip-geo`),
    retry: false,
  });
  const status = useQuery({
    queryKey: ["geo", "status"],
    queryFn: () => apiFetch<GeoStatus>("/geo/status"),
    retry: false,
  });

  const [open, setOpen] = useState(false);
  const [list, setList] = useState<"allow_cidrs" | "block_cidrs">("allow_cidrs");
  const [cidr, setCidr] = useState("");
  const [error, setError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (next: IpGeoConfig) =>
      apiFetch<void>(`/services/${serviceId}/ip-geo`, { method: "PUT", body: JSON.stringify(next) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["service", serviceId, "ipgeo"] }),
    onError: (e: unknown) =>
      setError(e instanceof ApiError ? e.message : "Couldn't save IP/geo settings."),
  });

  const data = cfg.data;
  const empty = !data
    || (data.allow_cidrs.length === 0
      && data.block_cidrs.length === 0
      && data.allow_countries.length === 0
      && data.block_countries.length === 0);

  function submitCidr() {
    if (!CIDR_RE.test(cidr.trim())) {
      setError("Invalid CIDR. Use form 10.0.0.0/8 or fe80::/10.");
      return;
    }
    if (!data) return;
    const next: IpGeoConfig = {
      ...data,
      enabled: true,
      [list]: [...data[list], cidr.trim()],
    };
    save.mutate(next);
    setOpen(false);
    setCidr("");
    setError(null);
  }

  return (
    <div className="ipgeo-panel">
      <h3>IP / geo restrictions</h3>
      {/* The relay was built without it: a fact about the relay, not something that went wrong. */}
      {status.data && !status.data.enabled && (
        <p className="muted small">
          Geo restrictions aren&apos;t available on this relay.
        </p>
      )}
      {empty ? (
        <p className="muted">Allow everywhere.</p>
      ) : (
        <div className="chip-row">
          {data?.allow_cidrs.map((c) => <span key={`a-${c}`} className="chip allow">allow {c}</span>)}
          {data?.block_cidrs.map((c) => <span key={`b-${c}`} className="chip block">block {c}</span>)}
          {data?.allow_countries.map((c) => <span key={`ac-${c}`} className="chip allow">allow {c}</span>)}
          {data?.block_countries.map((c) => <span key={`bc-${c}`} className="chip block">block {c}</span>)}
        </div>
      )}
      <div className="actions">
        <Button variant="secondary" size="sm" onClick={() => { setOpen(true); setError(null); }}>
          Add CIDR
        </Button>
      </div>
      {error && <p role="alert" className="notice-inline error">{error}</p>}

      <Dialog
        open={open}
        onOpenChange={(o) => { setOpen(o); if (!o) setError(null); }}
        title="Add CIDR"
        footer={
          <>
            <Button variant="secondary" onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="primary" onClick={submitCidr}>Add</Button>
          </>
        }
      >
        <div className="field">
          <label htmlFor="cidr-list">List</label>
          <Select
            id="cidr-list"
            value={list}
            onChange={(v) => setList(v as "allow_cidrs" | "block_cidrs")}
            options={LIST_OPTIONS}
          />
        </div>
        <div className="field">
          <label htmlFor="cidr-input">CIDR</label>
          <Input
            id="cidr-input"
            className="mono"
            value={cidr}
            onChange={(e) => setCidr(e.target.value)}
            placeholder="10.0.0.0/8"
          />
        </div>
      </Dialog>
    </div>
  );
}
