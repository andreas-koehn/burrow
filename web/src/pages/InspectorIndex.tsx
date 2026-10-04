import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, EmptyState, ErrorNotice, PageHeader, SkeletonRows } from "@/components/ds";
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

  // A failed list is not "no services": say so and offer a retry.
  if (services.isError && !services.data) {
    return (
      <div className="inspector-page">
        <PageHeader title="Request inspector" />
        <ErrorNotice
          action={
            <Button variant="secondary" size="sm" onClick={() => void services.refetch()}>
              Retry
            </Button>
          }
        >
          Couldn't load services:{" "}
          {services.error instanceof ApiError ? services.error.message : "Unknown error"}
        </ErrorNotice>
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
