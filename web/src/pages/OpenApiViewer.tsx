import { PageHeader } from "@/components/ds";

const BACK = { to: "/settings/general", label: "Settings" } as const;

export default function OpenApiViewer() {
  return (
    <div className="openapi-viewer-page">
      <PageHeader
        back={BACK}
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
