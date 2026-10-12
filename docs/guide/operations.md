# Operations

Day-2 tasks for a running Burrow relay: backups, upgrades, certificates, and logs.

---

## Backups

### In-app backup

The dashboard **Settings → Backup** panel and the `burrowd backup` command both
produce a timestamped archive of the SQLite database. Archives land in the
directory set by `BURROW_BACKUP_DIR` (default: `<database_path>.backups`).

```sh
burrowd backup
```

Restore from an archive with:

```sh
burrowd restore <archive-file>
```

::: warning Stop the server before restoring
`burrowd restore` writes directly to the database file. Bring the container
down before running restore to avoid corruption.
:::

### Volume snapshot (Docker)

Back up the `burrow_data` Docker volume with a one-liner. The container does
not need to be stopped for a read-consistent snapshot — SQLite's WAL mode keeps
the database readable while the copy runs.

```sh
docker run --rm \
  -v burrow_data:/data:ro \
  -v "$(pwd)/backups":/out \
  alpine \
  tar czf /out/burrow-backup-$(date +%Y%m%d-%H%M%S).tar.gz -C /data .
```

This writes a compressed archive to `./backups/` on the host. Schedule it with
`cron` or your infrastructure's snapshot tool.

::: tip Restore a volume snapshot
```sh
# Stop the relay first
docker compose down

# Restore the archive into the volume
docker run --rm \
  -v burrow_data:/data \
  -v "$(pwd)/backups":/out \
  alpine \
  tar xzf /out/burrow-backup-<timestamp>.tar.gz -C /data

# Start the relay again
docker compose up -d
```
:::

---

## Upgrades

### Docker (recommended)

Pull the latest `:develop` image and restart the stack with zero downtime:

```sh
docker compose pull
docker compose up -d
```

The relay container is replaced in-place. Active client connections disconnect
briefly and reconnect automatically.

::: info Image visibility
The GHCR package starts **private** after the first push. Make it public once at
https://github.com/users/andreas-koehn/packages/container/burrow/settings — or run
`docker login ghcr.io` before pulling on hosts that cannot reach a public
package.
:::

::: tip Pin to a digest for reproducible deployments
```yaml
# docker-compose.yml
services:
  burrowd:
    image: ghcr.io/andreas-koehn/burrow:develop@sha256:<digest>
```
:::

### Binary install

Download the latest rolling binary from the `develop` release page:

```
https://github.com/andreas-koehn/burrow/releases/tag/develop
```

Replace the running `burrowd` binary and restart the service.

### Build from source

```sh
git pull
go build ./cmd/server
go build ./cmd/client
```

Then restart `burrowd` with the new binary.

---

## Certificates

When `BURROW_ACME_DOMAIN` is set, Burrow manages the full certificate
lifecycle via [CertMagic](https://github.com/caddyserver/certmagic) (backed by
Let's Encrypt):

- **Issuance**: certificates are obtained before any listener starts.
- **Renewal**: CertMagic auto-renews well before expiry — no cron job needed.
- **Storage**: accounts and certificates are written to `BURROW_ACME_STORAGE`
  (default: `<dir(database_path)>/acme`). Include this directory in backups.

There is nothing to operate for certificates under normal conditions.

::: warning Renewal requires port 80
CertMagic uses the HTTP-01 challenge. Port `80` must remain reachable from the
public internet at all times — including during renewals. See
[Deploy](/guide/deploy) for firewall rules.
:::

::: details Testing with Let's Encrypt staging
Point `BURROW_ACME_CA` at the staging CA to avoid rate-limit errors while
validating a new setup:

```env
BURROW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory
```

Staging certificates are signed by an untrusted root, so browsers will warn.
Switch back to the default (production) CA once everything works.
:::

---

## Logs

Stream live logs from a running Docker stack:

```sh
docker compose logs -f
```

Scope to the relay container only:

```sh
docker compose logs -f burrowd
```

### Log format and level

Control verbosity and format via environment variables:

```env
BURROW_LOG_LEVEL=info      # debug | info | warn | error
BURROW_LOG_FORMAT=text     # text | json
```

Switch to `json` format for structured log ingestion (e.g. into Loki, Datadog,
or CloudWatch):

```env
BURROW_LOG_FORMAT=json
BURROW_LOG_LEVEL=warn
```

::: tip Debugging a connection issue
Set `BURROW_LOG_LEVEL=debug` temporarily. This logs individual proxy requests,
TLS handshakes, and ACME challenge activity. Revert to `info` in production —
debug output is verbose.
:::

---

## Health check

The relay exposes a health endpoint on the dashboard listener:

```sh
curl -sf https://burrow.insingo.com/healthz
```

A `200 OK` response indicates the server is up. Wire this into your load
balancer or uptime monitor.

---

## Retention

Log and connection-record retention is configured in **Settings → General** in
the dashboard. There is no env var — the setting is stored in the database and
applies immediately without a restart.

---

## Metrics

`GET /metrics` on the dashboard address answers in the Prometheus text format. It needs an admin session or an automation token with the `metrics:read` permission.

Every label value comes from a set whose size you control: the id of a service, a user, a key or a client token, the slug of a provider, a custom domain's hostname, or a fixed word. No metric carries a visitor's address, a path, a model name a caller sent, or an error text. What a visitor or an upstream chooses is reduced first:

- `method` is `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `CONNECT`, `TRACE`, or `other`.
- `status` of an HTTP family is the class of the status: `1xx`, `2xx`, `3xx`, `4xx`, `5xx`, or `none` when nothing was answered.

| Family | Labels | Counts |
|---|---|---|
| `burrow_http_requests_total` | `service`, `method`, `status` | requests the proxy answered for an http service (a visitor who hangs up is counted too) |
| `burrow_http_request_duration_seconds` | `service`, `method` | their duration up to the last byte, as a summary: quantiles 0.5, 0.95 and 0.99 plus `_count` and `_sum` |
| `burrow_http_request_bytes_in_total` | `service` | request body bytes |
| `burrow_http_request_bytes_out_total` | `service` | response body bytes |
| `burrow_connections_total` | `service`, `kind`, `status` | finished connections. `kind` is `http_proxy` (one per request), `tcp_proxy` (one per visitor connection) or `control` (one per client session). `status` is `closed_clean`, `closed_error`, `closed_idle` or `rejected`. `service` is empty for `tcp_proxy` and `control` |
| `burrow_connection_bytes_in_total` | `service`, `kind` | bytes visitors sent, by the same `service` and `kind` |
| `burrow_connection_bytes_out_total` | `service`, `kind` | bytes sent back to visitors |
| `burrow_client_session_count` | `user` | control sessions that are open now |
| `burrow_client_session_duration_seconds` | `user` | how long finished sessions lasted (a summary) |
| `burrow_client_bytes_in_total` | `user` | request bytes through the tunnels of a user's clients |
| `burrow_client_bytes_out_total` | `user` | response bytes through those tunnels |
| `burrow_control_reconnects_total` | `client` | sessions opened with a client token that had one before (also while the first is still open). `client` is the token's id; the relay remembers at most 16384 tokens and counts none beyond them |
| `burrow_ai_tokens_in_total` | `service`, `api_key` | input tokens of metered model requests. `api_key` is the id of the service key, `gw:<id>` for a gateway key, empty without a key |
| `burrow_ai_tokens_out_total` | `service`, `api_key` | output tokens. A stream cut before its usage chunk, or a provider that reports none, is estimated as response bytes divided by 4 |
| `burrow_ai_cost_usd_total` | `service`, `api_key` | their cost in USD: what a provider the relay calls itself stated, else the price table |
| `burrow_ai_cache_hits_total` | `service` | requests answered from the response cache |
| `burrow_ai_cache_misses_total` | `service` | cacheable requests it could not answer |
| `burrow_ai_failover_events_total` | `service`, `from`, `to`, `cross_provider` | steps from one upstream to the next within a request. `from` and `to` are provider slugs, `cross_provider` is `true` or `false`. An attempt the gateway refuses before calling the upstream (tunnel client offline, credential slot unset) is a step; so is a provider answer of 429 that moves on |
| `burrow_ai_upstream_errors_total` | `service`, `status` | failed upstream attempts. For a request through the gateway, `service` is the service of the upstream that failed and `status` the class of its answer, `none` when it gave no response. For a model service behind a tunnel (a key service), `status` is the class of what the visitor received on the traffic route. A provider 429 that moves on is a failover, not an error |
| `burrow_ai_semantic_lookups_total` | `service` | lookups in the semantic cache (only in a build with `-tags semantic_cache`) |
| `burrow_ai_semantic_hits_total` | `service`, `policy` | its hits; `policy` is `treat_as_miss` or `return_cached_marked` |
| `burrow_ai_semantic_promotions_total` | `service` | answers added to its index |
| `burrow_ai_semantic_errors_total` | `service`, `kind` | its errors; `kind` is `timeout`, `model` or `index` |
| `burrow_ai_semantic_index_entries` | `service` | entries in its index, refreshed after a promotion |
| `burrow_ai_semantic_index_bytes` | `service` | its size on disk, refreshed after a promotion |
| `burrow_ai_credential_injections_total` | `service`, `slot` | upstream credentials put on a request |
| `burrow_ai_credential_misses_total` | `service` | requests where the slot was bound but its environment variable was missing |
| `burrow_db_backend` | `driver` | always 1; `sqlite` or `postgres` |
| `burrow_db_pool_active` | none | database connections in use, read at each scrape |
| `burrow_db_pool_idle` | none | idle connections |
| `burrow_db_query_duration_seconds` | `op` | duration of statements (a summary). `op` is `select`, `insert`, `update`, `delete` or `other`; statements inside a transaction are not timed |
| `burrow_audit_chain_length` | none | rows of the audit log, read from the database at most every 10 seconds |
| `burrow_audit_chain_last_hash` | `hash` | the hash of the last row, in the label; the value is always 1 |
| `burrow_custom_domain_count` | none | custom domains, read at most every 10 seconds |
| `burrow_cert_expiry_days` | `cert` | days until a custom domain's certificate expires, set when the certificate is uploaded or renewed. `cert` is the hostname |
| `burrow_retention_compact_rows_deleted_total` | `table` | rows the daily clean-up deleted: `audit_events`, `usage_events`, `connection_logs`, `connection_log_rollups`, `webhook_deliveries`, `client_login_requests` |
| `burrow_retention_compact_last_run_seconds` | none | Unix time at which the clean-up last ran (00:30 UTC daily) |
| `burrow_goroutines` | none | goroutines of the relay |

A family that has had no event yet has no sample, except the gauges that are read at each scrape.

What the figures do not include, by design:

- A control session is `closed_clean` whenever the relay cannot tell why it ended, so also after a heartbeat timeout or a shutdown. Only `rejected` and `closed_error` come from requests and tcp connections.
- A visitor or caller who hangs up is not an upstream error and not a failover. The request is still counted, with status `closed_error` in `burrow_connections_total`, so `closed_error` there is not only failing upstreams.
- A replay from the request inspector is not observed at all.
- A request refused by a rate limit or a budget is not a model request: it adds no tokens, no cost and no cache figure.

The traffic figures of the **Tunnels** and **Clients** pages are not these counters: they count every byte that passes through a tunnel, headers included, while it passes.

The provider list (`GET /api/v1/ai/providers`) reports `latency_p95_ms` per provider: the nearest-rank 95th percentile of the last 24 hours of requests an upstream answered.

---

## Webhook events

A webhook receives the events it subscribes to (**Settings → Webhooks**). Each delivery is a POST with the event name in `Burrow-Event` and, unless the webhook has its own payload template, a JSON body `{"event": …, "data": {…}}`. The fields of `data` are listed here exactly as they are sent (some start with a capital letter, as they always did). A delivery that does not get a 2xx answer is retried.

Several events are throttled so that one fault cannot flood a receiver; the throttle is part of the entry.

| Event | When | Fields |
|---|---|---|
| `tunnel.connected` | a client registers a tunnel | `ServiceID`, `TunnelID`, `Kind` (`http`, `tcp` or `other`) |
| `tunnel.disconnected` | a tunnel is unregistered or its session ends | `ServiceID`, `TunnelID`, `Kind` |
| `tunnel.failed` | the relay refuses a registration; at most one per user and refusal code per minute | `ServiceID` (empty), `TunnelID` (empty), `Error` (the refusal code) |
| `service.created` | a client's first registration creates a service, or an admin creates one through the API | `service_id`, `name`, `type`, `access_mode` |
| `service.deleted` | never published: no code path deletes a service yet | `service_id`, `name` |
| `access.denied` | a request is refused with 401 or 403 by a service's access mode or IP policy (not the redirect to the login page, not a 500); at most one per service and reason per minute | `ServiceID`, `Reason` (`access` or `ip_geo`), `IP` (empty when the address is unknown) |
| `quota.exceeded` | a rate limit or day quota denies a request; at most one per limit per minute | `scope`, `subject`, `dimension`, `limit`, `window`, `service_id` |
| `budget.exceeded` | a budget is exceeded, once per budget and day | `budget_id`, `scope`, `subject_id`, `daily_usd`, `current_usd`, `daily_tokens`, `current_tokens`, `action_on_exceed`, plus `gateway_key_id` for a gateway key budget or `model` for a model budget |
| `redaction.applied` | a redaction rule changed a model request; at most one per service and rule per minute | `ServiceID`, `RuleID` |
| `guardrail.refused` | a guardrail refused a model request; at most one per service and pattern per minute | `ServiceID`, `Pattern` |
| `ai.upstream_error` | an upstream of a model service failed; at most one per service per hour | `service_id`, `backend_service_id`, `status` (0 without a response), `error` (`http_<status>` or `no_response`), `retry_count` |
| `ai.cache_promotion` | an answer was added to the semantic index (only in a build with `-tags semantic_cache`); at most one per service per hour | `service_id`, `exact_key_hash`, `prompt_fingerprint` (always empty) |
| `cert.expiring` | a custom-domain certificate is uploaded or renewed with less than 14 days left | `hostname`, `service_id`, `not_after` |
| `custom_domain.cert.expiring` | the daily check finds an active certificate entering its last 14 days | `id`, `hostname`, `service_id`, `not_after` |
| `connection.session_summary` | daily at 00:10 UTC, one per service and kind that had connections the day before. A day whose run failed, or whose time the relay was down for, is not caught up. Two relays on one database each publish it | `service_id`, `kind`, `window_start`, `window_end`, `sessions`, `bytes_in`, `bytes_out`, `avg_duration_ms`, `p95_duration_ms`, `top_source_ips` |
| `audit.exported` | the audit log was exported (GET `/api/v1/audit/export` finished without an error) | `ActorEmail`, `RecordCount` (the signature line is not counted) |
| `audit.policy_change` | a retention setting was saved with a value different from the stored one. One event per save, naming only the settings that changed | `actor_email`, `action` (`retention.update`), `before`, `after` (each a map from setting key to value) |
| `backup.completed` | a backup started from the dashboard or the API finished. `burrowd backup` on the command line announces nothing | `Path` (the archive's name), `SizeBytes` |
| `webhook.test` | the **Test** button | `hello` |

Events are delivered from a queue of 256; when it is full, events are dropped and a warning is logged. A relay that is stopping delivers what is queued. A request that a rate limit or a budget refuses produces no model-request event.

Writes of the model gateway's global settings (guardrails, cache, redaction) leave an audit entry that names the settings key and never its value. The audit entry of a change is written even when the caller has already hung up.

---

*Next: [Troubleshooting](/guide/troubleshooting) — common problems and fixes.*
