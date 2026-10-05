import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { PageHeader } from "@/components/ds";
import type { DatabaseStatus } from "@/lib/contract";


export default function DatabaseBackend() {
  const { data } = useQuery({
    queryKey: ["database"],
    queryFn: () => apiFetch<DatabaseStatus>("/database"),
    retry: false,
  });

  const status = data;

  return (
    <div className="account-page page-narrow">
      <PageHeader title="Database backend" subtitle="The storage engine Burrow is currently using." />

      <section className="account-section">
        {status && (
          <>
            {/* Alpha banner — amber, verbatim text */}
            {status.postgres_alpha && (
              <div className="notice-inline warn" role="status">
                Postgres backend is alpha — see release notes.
              </div>
            )}

            <dl className="def-list">
              <div className="def-row">
                <dt className="def-key">Driver</dt>
                <dd className="def-val"><code className="mono">{status.driver}</code></dd>
              </div>

              {status.driver === "postgres" && status.url_redacted && (
                <div className="def-row">
                  <dt className="def-key">Connection URL (redacted)</dt>
                  <dd className="def-val"><code className="mono">{status.url_redacted}</code></dd>
                </div>
              )}
            </dl>
          </>
        )}

        {/* "How is this configured?" disclosure — static, always visible */}
        <details className="details-disclosure">
          <summary>How is this configured?</summary>
          <p>
            Set <code>BURROW_DATABASE_URL</code> + <code>experimental.postgres_backend: true</code>. See docs.
          </p>
        </details>
      </section>
    </div>
  );
}
