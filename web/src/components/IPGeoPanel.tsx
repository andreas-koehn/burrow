import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, Input, Select } from "@/components/ds";
import { isAddrOrCidr } from "@/lib/tcpAccess";
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

/**
 * `tcp` turns it into the permanent list of a tcp service in IP allowlist mode: addresses and
 * ranges only (a bare address is allowed). The country rules of a tcp service are TcpCountriesPanel's.
 */
export function IPGeoPanel({ serviceId, tcp = false }: { serviceId: string; tcp?: boolean }) {
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
    enabled: !tcp,
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
    if (tcp ? !isAddrOrCidr(cidr) : !CIDR_RE.test(cidr.trim())) {
      setError(tcp
        ? "That is not an address or range. Use 203.0.113.9, 10.0.0.0/8 or 2001:db8::/32."
        : "Invalid CIDR. Use form 10.0.0.0/8 or fe80::/10.");
      return;
    }
    if (!data) return;
    const next: IpGeoConfig = {
      ...data,
      enabled: true,
      [list]: [...data[list], cidr.trim()],
    };
    setError(null);
    if (tcp) {
      // The relay judges the prefix length (too wide a range): the dialog stays until it has said yes.
      save.mutate(next, { onSuccess: () => { setOpen(false); setCidr(""); } });
      return;
    }
    save.mutate(next);
    setOpen(false);
    setCidr("");
  }

  if (tcp) {
    return (
      <TcpList
        data={data}
        open={open} setOpen={setOpen} list={list} setList={setList} cidr={cidr} setCidr={setCidr}
        error={error} setError={setError} submit={submitCidr}
      />
    );
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

interface TcpListProps {
  data: IpGeoConfig | undefined;
  open: boolean; setOpen: (v: boolean) => void;
  list: "allow_cidrs" | "block_cidrs"; setList: (v: "allow_cidrs" | "block_cidrs") => void;
  cidr: string; setCidr: (v: string) => void;
  error: string | null; setError: (v: string | null) => void;
  submit: () => void;
}

function TcpList({ data, open, setOpen, list, setList, cidr, setCidr, error, setError, submit }: TcpListProps) {
  const titleId = useId();
  const allow = data?.allow_cidrs ?? [];
  const block = data?.block_cidrs ?? [];
  return (
    <section className="ipgeo-panel card col gap-2" aria-labelledby={titleId}>
      <h2 id={titleId}>Allowed addresses</h2>
      <p className="muted small">
        The permanent list of the IP allowlist. An address on the block list is always refused.
      </p>
      {allow.length === 0 && block.length === 0 ? (
        <p className="muted">
          The list is empty: only a grant lets an address in. With no grant, nobody can connect.
        </p>
      ) : (
        <div className="chip-row">
          {allow.map((c) => <span key={`a-${c}`} className="chip allow">allow {c}</span>)}
          {block.map((c) => <span key={`b-${c}`} className="chip block">block {c}</span>)}
        </div>
      )}
      <div className="actions">
        <Button type="button" variant="secondary" size="sm" onClick={() => { setOpen(true); setError(null); }}>
          Add address
        </Button>
      </div>
      {error && !open && <p role="alert" className="notice-inline error">{error}</p>}

      <Dialog
        open={open}
        onOpenChange={(o) => { setOpen(o); if (!o) setError(null); }}
        title="Add address"
        footer={
          <>
            <Button type="button" variant="secondary" onClick={() => setOpen(false)}>Cancel</Button>
            <Button type="button" variant="primary" onClick={submit}>Add</Button>
          </>
        }
      >
        <div className="field">
          <label htmlFor="tcp-list">List</label>
          <Select
            id="tcp-list"
            value={list}
            onChange={(v) => setList(v as "allow_cidrs" | "block_cidrs")}
            options={[{ value: "allow_cidrs", label: "Allow" }, { value: "block_cidrs", label: "Block" }]}
          />
        </div>
        <div className="field">
          <label htmlFor="tcp-address">Address or range</label>
          <Input
            id="tcp-address"
            className="mono"
            value={cidr}
            onChange={(e) => setCidr(e.target.value)}
            placeholder="203.0.113.9 or 10.0.0.0/8"
          />
        </div>
        {error && <p role="alert" className="notice-inline error">{error}</p>}
      </Dialog>
    </section>
  );
}
