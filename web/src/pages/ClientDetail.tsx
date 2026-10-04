import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { Badge, PageHeader, SkeletonRows } from "@/components/ds";
import type { AccessMode, ClientDetail as ClientDetailT } from "@/lib/contract";

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

export default function ClientDetail() {
  const { id = "" } = useParams();
  const { data, isLoading, error } = useQuery({
    queryKey: ["client", id],
    queryFn: () => apiFetch<ClientDetailT>(`/clients/${id}`),
    retry: false,
  });

  if (error) {
    return (
      <div className="users-page">
        <PageHeader title="Client" />
        <div className="notice-block error">
          <div className="icon-bubble"><AlertTriangle size={18} /></div>
          <p role="alert">{error instanceof ApiError ? error.message : "client not found"}</p>
        </div>
      </div>
    );
  }
  if (isLoading || !data) return <div className="table-wrap"><SkeletonRows n={3} /></div>;

  return (
    <div className="users-page">
      <PageHeader
        title={data.token_name}
        subtitle={<span className="mono">{data.session_id} · {data.os}/{data.arch} · burrow {data.client_version}</span>}
      />
      <section className="account-section" aria-labelledby="sec-services">
        <div className="section-head"><div className="left"><h2 id="sec-services">Services</h2></div></div>
        <div className="table-wrap">
          <table className="data" aria-label="Services">
            <thead><tr><th>Name</th><th>Type</th><th>Remote</th><th>Local</th><th>Traffic</th><th>Access</th></tr></thead>
            <tbody>
              {data.services.map((s) => (
                <tr key={s.id}>
                  <td>{s.name}</td>
                  <td><Badge kind={`type-${s.type}`} nodot>{s.type}</Badge></td>
                  <td className="col-created">
                    {s.type === "http" ? <span className="muted">—</span> : `:${s.remote_port}`}
                  </td>
                  <td className="col-created mono">{s.local_addr}</td>
                  <td className="col-created">↓{formatBytes(s.total_bytes_in)} ↑{formatBytes(s.total_bytes_out)}</td>
                  <td>
                    <span className="row row-center gap-2">
                      <Badge kind={`access-${s.access_mode}`} nodot>{ACCESS_LABEL[s.access_mode]}</Badge>
                      {s.service_id && (
                        <Link className="link-inline" to={`/services/${s.service_id}`}>Configure</Link>
                      )}
                    </span>
                  </td>
                </tr>
              ))}
              {data.services.length === 0 && (
                <tr><td colSpan={6} className="muted">Connected, but not serving any service yet.</td></tr>
              )}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}
