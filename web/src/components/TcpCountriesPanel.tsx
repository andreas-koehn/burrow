import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, ErrorNotice, FormField, Input, Select } from "@/components/ds";
import { countryCode } from "@/lib/tcpAccess";
import type { GeoStatus, IpGeoConfig } from "@/lib/contract";

type List = "allow_countries" | "block_countries";

/**
 * The country rules of a tcp service in mode ip or open, checked when a visitor connects. They need
 * a GeoIP database on the relay: without one nothing enforces them, and the panel says so.
 */
export function TcpCountriesPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const key = ["service", serviceId, "ipgeo"];
  const cfg = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<IpGeoConfig>(`/services/${serviceId}/ip-geo`),
    retry: false,
  });
  const status = useQuery({
    queryKey: ["geo", "status"],
    queryFn: () => apiFetch<GeoStatus>("/geo/status"),
    retry: false,
  });
  const [list, setList] = useState<"allow" | "block">("block");
  const [code, setCode] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (next: IpGeoConfig) =>
      apiFetch<void>(`/services/${serviceId}/ip-geo`, {
        method: "PUT",
        body: JSON.stringify({
          enabled: next.enabled,
          allow_cidrs: next.allow_cidrs, block_cidrs: next.block_cidrs,
          allow_countries: next.allow_countries, block_countries: next.block_countries,
        }),
      }),
    onSuccess: async () => {
      setErr(null);
      setCode("");
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the countries."),
  });

  const data = cfg.data;
  const allow = data?.allow_countries ?? [];
  const block = data?.block_countries ?? [];
  // Known once the relay has said so; until then nothing is claimed.
  const noDatabase = status.data ? !status.data.enabled : data?.country_rules_enforced === false;
  const notEnforced = noDatabase || data?.country_rules_enforced === false;

  function add() {
    const c = countryCode(code);
    if (!c) { setErr("Use a two-letter country code such as DE or US."); return; }
    if (!data) return;
    const field: List = list === "allow" ? "allow_countries" : "block_countries";
    if (data[field].includes(c)) { setErr(`${c} is on the list already.`); return; }
    setErr(null);
    save.mutate({ ...data, enabled: true, [field]: [...data[field], c] });
  }
  function remove(field: List, c: string) {
    if (!data) return;
    setErr(null);
    save.mutate({ ...data, [field]: data[field].filter((x) => x !== c) });
  }

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Countries</h2>
      <p className="muted small">
        Checked when a visitor connects, in the modes IP allowlist and Open. A visitor whose country is unknown is refused
        by an allow list and let in by a block list.
      </p>
      {notEnforced && (
        <ErrorNotice variant="warn" role="status">
          Country rules are not enforced: this relay has no GeoIP database loaded. Set BURROW_GEOIP_DB to a
          GeoLite2-Country file and restart the relay.{" "}
          {(allow.length > 0 || block.length > 0) && "The countries below do nothing until then; you can remove them."}
        </ErrorNotice>
      )}
      {cfg.error ? (
        <ErrorNotice role="alert">Couldn&apos;t load the countries.</ErrorNotice>
      ) : allow.length === 0 && block.length === 0 ? (
        !cfg.isLoading && <p className="muted small">No country rule: visitors are let in from everywhere.</p>
      ) : (
        <div className="chip-row">
          {allow.map((c) => (
            <span key={`a-${c}`} className="chip allow">
              allow {c}{" "}
              <button type="button" className="chip-remove" aria-label={`Remove country ${c}`} onClick={() => remove("allow_countries", c)}>×</button>
            </span>
          ))}
          {block.map((c) => (
            <span key={`b-${c}`} className="chip block">
              block {c}{" "}
              <button type="button" className="chip-remove" aria-label={`Remove country ${c}`} onClick={() => remove("block_countries", c)}>×</button>
            </span>
          ))}
        </div>
      )}
      <div className="row row-center gap-3">
        <Select
          aria-label="List"
          options={[{ value: "block", label: "Block" }, { value: "allow", label: "Allow" }]}
          value={list}
          onChange={(v) => setList(v as "allow" | "block")}
        />
        <FormField label="Country code" htmlFor={`${titleId}-code`} w="sm" error={err ?? undefined}>
          <Input
            id={`${titleId}-code`}
            className="mono"
            value={code}
            maxLength={2}
            placeholder="DE"
            autoComplete="off"
            invalid={err !== null}
            onChange={(e) => { setCode(e.target.value); setErr(null); }}
            onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); add(); } }}
          />
        </FormField>
        <Button type="button" variant="secondary" size="sm" disabled={save.isPending || !data || noDatabase} onClick={add}>
          Add country
        </Button>
      </div>
    </section>
  );
}
