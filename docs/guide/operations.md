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

Every label value comes from a set whose size you control: the id of a service, a user, a key or a client token, the slug of a provider, the name of a credential slot, a custom domain's hostname, or a fixed word. The one exception is `hash` of `burrow_audit_chain_last_hash`: it changes with every audit row, but the exporter keeps only one series, the current one, so the family never grows. No metric carries a visitor's address, a path, a model name a caller sent, or an error text. What a visitor or an upstream chooses is reduced first:

- `method` is `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `CONNECT`, `TRACE`, or `other`.
- `status` of an HTTP family is the class of the status: `1xx`, `2xx`, `3xx`, `4xx`, `5xx`, or `none` when nothing was answered.

Requests to the model gateway (`/openai`, `/anthropic`, `/ai/…`) are not proxied requests: they appear in the `burrow_ai_*` families and not in `burrow_http_*`, `burrow_connections_total`, `burrow_connection_bytes_*` or `burrow_client_bytes_*`. A gateway request that is refused before it reaches a provider (by a guardrail, a rate limit, a bad key, an unknown model, a budget, a redaction that drops it, or a body over the limit) leaves no sample in any family; a guardrail and a rate limit still publish their webhook event.

| Family | Labels | Counts |
|---|---|---|
| `burrow_http_requests_total` | `service`, `method`, `status` | requests the proxy answered for an http service (a visitor who hangs up is counted too) |
| `burrow_http_request_duration_seconds` | `service`, `method` | their duration up to the last byte, as a summary: quantiles 0.5, 0.95 and 0.99 plus `_count` and `_sum` |
| `burrow_http_request_bytes_in_total` | `service` | request body bytes |
| `burrow_http_request_bytes_out_total` | `service` | response body bytes |
| `burrow_connections_total` | `service`, `kind`, `status` | finished connections. `kind` is `http_proxy` (one per request), `tcp_proxy` (one per visitor connection) or `control` (one per client session). `status` is `closed_clean`, `closed_error`, `closed_idle` or `rejected`; a tcp connection gives only `closed_clean` and `closed_error`, `rejected` and `closed_idle` come from http requests only. A model request through the gateway (a caller with a gateway key) is not a proxied request and never reaches this family. `service` is empty for `tcp_proxy` and `control` |
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
| `burrow_ai_upstream_errors_total` | `service`, `status` | failed upstream attempts. For a request through the gateway, `service` is the service of the upstream that failed and `status` the class of its answer, `none` when it gave no response. For a model service behind a tunnel (a key service), `status` is the class of what the visitor received on the traffic route. An attempt the gateway refuses itself (the tunnel client is offline: 502; the credential slot is unset: 503) counts too, under the class of the gateway's own status. A provider 429 that moves on is a failover, not an error |
| `burrow_ai_semantic_lookups_total` | `service` | lookups in the semantic cache (only in a build with `-tags semantic_cache`) |
| `burrow_ai_semantic_hits_total` | `service`, `policy` | its hits; `policy` is `treat_as_miss` or `return_cached_marked` |
| `burrow_ai_semantic_promotions_total` | `service` | answers added to its index |
| `burrow_ai_semantic_errors_total` | `service`, `kind` | its errors; `kind` is `timeout`, `model` or `index` |
| `burrow_ai_semantic_index_entries` | `service` | entries in its index, refreshed after a promotion |
| `burrow_ai_semantic_index_bytes` | `service` | its size on disk, refreshed after a promotion |
| `burrow_ai_credential_injections_total` | `service`, `slot` | upstream credentials put on a request. `slot` is the name of the credential slot the operator bound (the suffix of the environment variable that holds the key, never the key) |
| `burrow_ai_credential_misses_total` | `service` | requests where the slot was bound but its environment variable was missing |
| `burrow_mcp_tool_calls_total` | `source`, `outcome` | finished MCP tool calls. `source` is the slug of a tool source, or `burrow` for the management server; `outcome` is `ok`, `denied` or `error`. The tool's name is never a label |
| `burrow_db_backend` | `driver` | always 1; `sqlite` or `postgres` |
| `burrow_db_pool_active` | none | database connections in use, read at each scrape |
| `burrow_db_pool_idle` | none | idle connections |
| `burrow_db_query_duration_seconds` | `op` | duration of statements (a summary). `op` is `select`, `insert`, `update`, `delete` or `other`. It measures until the call returns, including the wait for a pool connection and excluding the iteration over rows and the scan. Timed: the statements of the typed database layer outside a transaction, the usage-event INSERT of the model-request meter and the per-connection INSERT of the connection log. Not timed: statements inside a transaction (including the rollup's writes and the exact cache's insert) and statements on the raw handle: the connection log's reads and its rollup reads, the exact prompt cache's lookups, the semantic index |
| `burrow_audit_chain_length` | none | rows present in the audit log (the daily clean-up of the leaf entries reduces it), read from the database at most every 10 seconds |
| `burrow_audit_chain_last_hash` | `hash` | the hash of the last row, in the label; the value is always 1 |
| `burrow_custom_domain_count` | none | custom domains, read at most every 10 seconds |
| `burrow_cert_expiry_days` | `cert` | days until a custom domain's certificate expires, set when the certificate is uploaded or renewed. `cert` is the hostname |
| `burrow_retention_compact_rows_deleted_total` | `table` | rows the daily clean-up deleted: `audit_events`, `usage_events`, `connection_logs`, `connection_log_rollups`, `webhook_deliveries`, `client_login_requests` |
| `burrow_retention_compact_last_run_seconds` | none | Unix time at which the clean-up last ran (00:30 UTC daily) |
| `burrow_goroutines` | none | goroutines of the relay |

A family that has had no event yet has no sample, except the gauges that are read at each scrape.

What the figures do not include, by design:

- A control session is `closed_clean` whenever the relay cannot tell why it ended, so also after a heartbeat timeout or a shutdown. `rejected` and `closed_idle` come from http requests, `closed_error` from requests and tcp connections.
- A caller of the model gateway who hangs up is visible nowhere in `burrow_connections_total` (gateway requests are not in it): the attempt is recorded as `client_closed` in the request's attempts. It is no upstream error and no failover.
- A visitor who hangs up on a proxied service is not an upstream error and not a failover. The request is still counted, with status `closed_error` in `burrow_connections_total`, so `closed_error` there is not only failing upstreams.
- A replay from the request inspector is not observed at all.
- A request refused by a rate limit or a budget is not a model request: it adds no tokens, no cost and no cache figure, and no model-request event is produced. `quota.exceeded` or `budget.exceeded` is what fires.

The traffic figures of the **Tunnels** and **Clients** pages are not these counters: they count every byte that passes through a tunnel, headers included, while it passes.

The provider list (`GET /api/v1/ai/providers`) and a single provider (`GET /api/v1/ai/providers/{slug}`) report `latency_p95_ms`: the nearest-rank 95th percentile of the latency of the requests of the last 24 hours that the provider answered with a 2xx. The relay computes it at most once a minute, for all providers together, so the figure is up to a minute old; a request that arrives while it is being computed again gets the previous figure. The request and cache-hit counts next to it are read on every request. If the computation fails, the last figure stays (0 when there never was one) and one warning is logged until it succeeds again.

---

## Webhook events

A webhook receives the events it subscribes to (**Settings → Webhooks**). Each delivery is a POST with the event name in `Burrow-Event` and, unless the webhook has its own payload template, a JSON body `{"event": …, "data": {…}}`. The fields of `data` are listed here exactly as they are sent (some start with a capital letter, as they always did). A delivery that does not get a 2xx answer is retried.

Several events are throttled so that one fault cannot flood a receiver; the throttle is part of the entry. `tunnel.connected`, `tunnel.disconnected` and the `service.created` of a client's first registration share one budget of 60 events per user per minute (a first registration spends two); the rest are dropped. A dropped tunnel event logs a warning (once a minute per user); a dropped `service.created` is dropped silently. `service.created` made by an admin through the API is not throttled. `tunnel.failed` and the events that say "at most one per …" below are throttled as stated; every other event is published each time it happens.

| Event | When | Fields |
|---|---|---|
| `tunnel.connected` | a client registers a tunnel (60 per user per minute, shared with `tunnel.disconnected` and `service.created`) | `ServiceID`, `TunnelID`, `Kind` (`http`, `tcp` or `other`) |
| `tunnel.disconnected` | a tunnel is unregistered or its session ends (same budget). Not when the relay itself stops: see below the table | `ServiceID`, `TunnelID`, `Kind` |
| `tunnel.failed` | the relay refuses a registration; at most one per user and refusal code per minute | `ServiceID` (empty), `TunnelID` (empty), `Error` (the refusal code) |
| `service.created` | a client's first registration creates a service (same budget), or an admin creates one through the API (not throttled) | `service_id`, `name`, `type`, `access_mode` |
| `service.deleted` | never published: no code path deletes a service yet | `service_id`, `name` |
| `access.denied` | a request is refused with 401 or 403 by a service's access mode or IP policy (not the redirect to the login page, not a 500); at most one per service and reason per minute | `ServiceID`, `Reason` (`access` or `ip_geo`), `IP` (the visitor's address, empty when unknown; it is sent whatever the setting `connection_logs.rollup_include_top_ips` says) |
| `quota.exceeded` | a rate limit or day quota denies a request; at most one per limit per minute | `scope`, `subject`, `dimension`, `limit`, `window`, `service_id` |
| `budget.exceeded` | a budget is exceeded, once per budget and day | `budget_id`, `scope`, `subject_id`, `daily_usd`, `current_usd`, `daily_tokens`, `current_tokens`, `action_on_exceed`, plus `gateway_key_id` for a gateway key budget or `model` for a model budget |
| `redaction.applied` | a redaction rule changed a model request; at most one per service and rule per minute | `ServiceID`, `RuleID` |
| `guardrail.refused` | a guardrail refused a model request, an answer pattern matched an answer (delivered or withheld, see [Guardrails on answers](/guide/expose-services#guardrails)), or the judge model refused a request or withheld an answer (see [The judge](/guide/expose-services#guardrails-judge)); at most one per service, tier, pattern and direction per minute | `ServiceID`, `Pattern` (empty for the judge), `direction` (`request` or `answer`), `tier` (`pattern` or `judge`), `action` (`refuse_403`, `refuse_safe`, `flag` or `block`) |
| `ai.upstream_error` | an upstream of a model service failed; at most one per service per hour | `service_id`, `backend_service_id`, `status` (0 without a response), `error` (`http_<status>` or `no_response`), `retry_count` |
| `ai.cache_promotion` | an answer was added to the semantic index (only in a build with `-tags semantic_cache`); at most one per service per hour | `service_id`, `exact_key_hash`, `prompt_fingerprint` (always empty) |
| `cert.expiring` | a custom-domain certificate is uploaded or renewed with less than 14 days left | `hostname`, `service_id`, `not_after` |
| `custom_domain.cert.expiring` | the daily check finds an active certificate entering its last 14 days | `id`, `hostname`, `service_id`, `not_after` |
| `connection.session_summary` | daily at 00:10 UTC, one per service and kind that had connections the day before. The same run also happens once at every relay start (on its own goroutine, it does not delay the start), and catches up finished days that have connection logs and no rollup yet, oldest first, within both retention windows (`connection_logs.retention_days` and `connection_logs.rollups_retention_days`; 0 keeps forever), and announces each such day once; a day with rollup rows is never rolled or announced again. The rollup rows of a day are written in one transaction: a day is rolled completely or not at all. The announcements come after it, paced, and are best-effort: if the relay stops while it is still announcing, the rest of that day is not announced later. Two relays on one database each publish it. The `day` of a rollup (`GET /api/v1/connection-logs/rollups`) is a date, `YYYY-MM-DD`, on SQLite and on Postgres. `top_source_ips` are visitor addresses (up to 10 per group, on by default); `connection_logs.rollup_include_top_ips` set to `false` leaves them out | `service_id`, `kind`, `window_start`, `window_end`, `sessions`, `bytes_in`, `bytes_out`, `avg_duration_ms`, `p95_duration_ms`, `top_source_ips` |
| `audit.exported` | the audit log was exported (GET `/api/v1/audit/export` finished without an error) | `ActorEmail`, `RecordCount` (the signature line is not counted) |
| `audit.policy_change` | a retention setting was saved with a value different from the stored one. One event per save, naming only the settings that changed | `actor_email`, `action` (`retention.update`), `before`, `after` (each a map from setting key to value) |
| `backup.completed` | a backup started from the dashboard or the API finished. `burrowd backup` on the command line announces nothing | `Path` (the archive's name), `SizeBytes` |
| `webhook.test` | the **Test** button | `hello` |

Events are delivered from a queue of 256; when it is full, events are dropped and a warning is logged. A request that a rate limit or a budget refuses produces no model-request event.

A relay that is stopping does not wait on webhook receivers. From the moment it is told to stop, it publishes none of the events that come from sessions, tunnels, proxied requests and model requests (`tunnel.connected`, `tunnel.disconnected`, `tunnel.failed`, a client's `service.created`, `access.denied`, `redaction.applied`, `guardrail.refused`, `ai.upstream_error`, `ai.cache_promotion`): the tunnels it closes itself are not announced as disconnected, so a receiver does not learn of a stop or a restart from `tunnel.disconnected`. The metrics still count what ends during the stop. Events that are still queued get 5 seconds in all: each is tried once, without retries; a delivery that is still running when the 5 seconds are over is cancelled and the rest of the queue is dropped, and one warning in the log says how many events were not delivered. A delivery cancelled this way is not counted against the webhook as a failure.

Writes of the model gateway's global settings (guardrails, cache, redaction) leave an audit entry that names the settings key and never its value. A guardrail that matches leaves one too, naming the pattern and never the matched text: `guardrail.refused` for a refused request or a withheld answer (with `direction: "answer"`), `guardrail.flagged` for an answer that was delivered with a hit. Both are kept to one entry per service, pattern, action and gateway key per hour. A block by the judge model is a `guardrail.refused` entry with `tier: "judge"` and the judge's reason instead of a pattern; a judge that could not judge leaves `guardrail.judge_failed`, naming the class of the failure (`timeout`, `error`, `malformed`, `unavailable`, `too_large`) and what `on_failure` did, never the judged text. These are kept to one entry per service, direction and gateway key per hour (a failure also per class). The audit entry of a change is written even when the caller has already hung up.

---

*Next: [Troubleshooting](/guide/troubleshooting) — common problems and fixes.*
