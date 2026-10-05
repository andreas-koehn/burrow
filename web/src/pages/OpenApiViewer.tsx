import { PageHeader } from "@/components/ds";

export default function OpenApiViewer() {
  return (
    <div className="openapi-viewer-page">
      <PageHeader
        title="API reference"
        subtitle={<>Browse this relay&apos;s JSON/HTTP API. The full spec is also available at <code className="mono">/api/v1/openapi.yaml</code>.</>}
      />
      <iframe
        src="/api/v1/openapi/viewer/"
        title="OpenAPI viewer"
      />
    </div>
  );
}
