import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { Button, EmptyState, PageHeader, SkeletonRows } from "@/components/ds";
import type { Service } from "@/lib/contract";

// Stable entry point for the request inspector: picks the first http service
// so the sidebar link does not have to embed a service id.
export default function InspectorIndex() {
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });

  if (services.isLoading) {
    return (
      <div className="inspector-page">
        <PageHeader title="Request inspector" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  const first = (Array.isArray(services.data) ? services.data : []).find((s) => s.type === "http");
  if (first) return <Navigate to={`/inspector/${first.id}`} replace />;

  return (
    <div className="inspector-page">
      <PageHeader title="Request inspector" subtitle="Tail and replay traffic on an HTTP service." />
      <EmptyState
        title="No HTTP services to inspect"
        action={<Link to="/clients/connect"><Button variant="primary" size="sm">Connect a client</Button></Link>}
      >
        Connect a client with an HTTP service to see its requests here.
      </EmptyState>
    </div>
  );
}
