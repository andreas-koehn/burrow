# Burrow testing guide

Burrow is verified across a **four-tier test pyramid**. Each tier proves
something the others can't:

- **Unit** isolates a single package's logic with no I/O.
- **Integration (Tier 1)** proves the data plane is reachable end to end
  (client → relay → upstream) with nothing but `curl`.
- **E2E (Tier 2)** drives the real dashboard UI in a real browser against the
  live container stack.
- **Manual (Tier 3)** is a human walkthrough for the things automation can't
  judge — look, feel, and surfaces no script asserts.

Read this top to bottom to know *what* is covered, *where* it lives, and *how*
to run it. The capstone is the [feature coverage matrix](#feature-coverage-matrix).

## The pyramid

### Unit — Go package tests

Colocated with the source they exercise: `internal/…`, `cmd/…`, `pkg/…`, and
`web/embed_test.go`. Fast, hermetic, no Docker, no network.

```sh
go test ./...
```

Build-tagged suites (e.g. Postgres, semantic cache, geo) run under their tag,
for example `go test -tags=postgres ./...`.

### Integration (Tier 1) — data-plane reachability

Curl-only assertions that the full tunnel chain carries real traffic. Brings the
Docker stack up, polls the public visitor port, asserts round-trips, tears down.

```sh
task integration:smoke        # minimal: base stack, single HTTP tunnel round-trip
task integration:smoke-full   # full stack: multi-service TCP + AI SSE round-trips
```

`smoke.sh` exercises the minimal `compose.base.yml` stack (one tunnel,
`/healthz` + `/echo` round-trips, dashboard reachable). `smoke-full.sh`
exercises `compose.full.yml` (three TCP tunnels on 9002/9003/9004 and an AI
`/v1/chat/completions` SSE stream).

### E2E (Tier 2) — dashboard UI in a real browser

Playwright drives Chromium against the running stack and asserts the dashboard's
behaviour.

```sh
task e2e:run                  # one-shot: bring stack up, run all specs, tear down
```

For fast iteration, leave the stack up and re-run specs by hand:

```sh
task e2e:up                   # bring the full stack up and leave it running
cd test/e2e && npx playwright test
task e2e:down                 # tear the stack down when finished
```

### Manual (Tier 3) — human walkthrough

```sh
task e2e:lab                  # bring the lab up and print the login creds
```

Then follow [`test/manual/RUNBOOK.md`](manual/RUNBOOK.md) step by step. For a
scripted, opinionated user-acceptance pass instead of the manual checklist:

```sh
task e2e:user                 # runs test/manual/smoke-user.sh end to end
```

## Layout

```
test/
  harness/       shared Docker infra
                   compose.base.yml, compose.full.yml, compose.postgres.yml
                   Dockerfile.*, entrypoints/, certs/, mockoai/, upstream/
                   burrow-multi.yaml
  integration/   Tier-1 curl gates: smoke.sh (minimal), smoke-full.sh (full)
  e2e/           Tier-2 Playwright: playwright.config.ts, package.json, run.sh
                   fixtures/, spec/<feature>/
  manual/        Tier-3: RUNBOOK.md, smoke-user.sh
```

## Harness profiles

The harness composes three stacks of increasing fidelity:

| Profile      | Compose file(s)                            | What it brings up |
|--------------|--------------------------------------------|-------------------|
| **base**     | `compose.base.yml`                         | Minimal: a default-build relay + one client + the test upstream. Backs `integration:smoke`. |
| **full**     | `compose.full.yml`                         | Integration-build relay + `client-ai` / `client-tcp` / `client-multi` clients + `mockoai`. Backs `integration:smoke-full`, the E2E suite, and the manual lab. |
| **postgres** | `compose.full.yml` + `compose.postgres.yml` | The full stack with the relay's store swapped to Postgres (override layered on top of full). |

## Task name changes

The runner targets were renamed during the `test/` restructure. Old names no
longer exist:

| Old target                            | New target               |
|---------------------------------------|--------------------------|
| `task e2e:smoke`                      | `task integration:smoke` |
| `task e2e:ui` / `task e2e:full`       | `task e2e:run`           |
| `task e2e:smoke-user`                 | `task e2e:user`          |

## Feature coverage matrix

Each cell cites the concrete artifact that covers the feature, or `—` for a gap.

- **Unit** cites an `internal/…`, `cmd/…`, or `pkg/…` package that has a real
  `*_test.go`.
- **Integration** cites `smoke.sh` / `smoke-full.sh` (curl data-plane) or
  `smoke-user.sh` steps (API-driven).
- **E2E** cites the spec path under `test/e2e/spec/<group>/`.
- **Manual** cites the `RUNBOOK` section that covers it.

| Feature | Unit | Integration | E2E | Manual |
|---------|------|-------------|-----|--------|
| Tunnel data-plane | `internal/proxy/proxy_test.go`, `internal/server/data_test.go` | `smoke.sh` (HTTP round-trip), `smoke-full.sh` (TCP 9002) | `tunnels/02-tunnels.spec.ts` | RUNBOOK §2 |
| Services / burrow.yaml multi-service | `internal/store/services_test.go`, `internal/client/fileconfig_test.go` | `smoke-full.sh` (svc-a 9003, svc-b 9004) | `tunnels/03-services-burrow-yaml.spec.ts` | RUNBOOK §3 |
| Clients listing | `cmd/server/clients_adapter_test.go`, `cmd/server/tunnel_lister_adapter_test.go` | — | `tunnels/22-clients.spec.ts` | RUNBOOK §2 |
| Token mint (UI) | `internal/api/token_test.go` | `smoke-user.sh` step 2 (mint-token) | `tokens/04-tokens-mint.spec.ts` | RUNBOOK §4 |
| Token connect (CLI) | `internal/client/client_test.go`, `cmd/client/main_test.go` | `smoke-user.sh` step 3 (connect) | `tokens/33-token-connect.spec.ts` | RUNBOOK §4 |
| Users / roles | `internal/db/roles_test.go`, `internal/store/roles_test.go`, `internal/api/role_test.go` | — | `users-roles/05-users-roles.spec.ts` | RUNBOOK §5 |
| Access: open | `internal/proxy/access_test.go`, `internal/proxy/gate_test.go` | `smoke-full.sh` (open AI tunnel) | `access-modes/06-access-mode-open.spec.ts` | RUNBOOK §6a |
| Access: api_key | `internal/proxy/gate_test.go`, `internal/api/automation_test.go` | `smoke-user.sh` step 5 (access-gate-api-key) | `access-modes/07-access-mode-api-key.spec.ts` | RUNBOOK §6b |
| Access: burrow_login | `internal/auth/auth_test.go`, `internal/api/auth_test.go` | — | `access-modes/08-access-mode-burrow-login.spec.ts` | RUNBOOK §6c |
| Access: mTLS | `internal/proxy/mtls_test.go`, `cmd/server/e2e_mtls_test.go` | — | — | RUNBOOK §6d |
| AI gateway basic | `internal/aigw/anthropic_test.go`, `cmd/server/e2e_openai_test.go` | `smoke-full.sh` (`/v1/chat/completions` SSE) | `ai-gateway/10-ai-gateway-basic.spec.ts` | RUNBOOK §7 |
| AI semantic cache | `internal/cache/semantic/semantic_test.go`, `internal/cache/exact/cache_test.go`, `internal/api/cache_test.go` | — | `ai-gateway/11-ai-gateway-semantic-cache.spec.ts` | RUNBOOK §8a |
| AI metering / cost | `internal/aimeter/meter_test.go`, `internal/cost/engine_test.go`, `internal/api/cost_test.go` | — | `ai-gateway/12-ai-gateway-metering-cost.spec.ts` | RUNBOOK §7 |
| MCP | `internal/mcpserv/server_test.go`, `internal/api/mcp_test.go` | — | `ai-gateway/26-mcp.spec.ts` | — |
| Custom domains | `internal/proxy/customdomain/customdomain_test.go`, `internal/proxy/customdomain/status_test.go` | — | `domains/13-custom-domains.spec.ts` | RUNBOOK §9 |
| Connection logs | `internal/connlog/sink_test.go` | — | `observability/14-connection-logs.spec.ts` | RUNBOOK §10 |
| Audit chain | `internal/audit/logger_test.go`, `internal/db/audit_test.go`, `internal/api/audit_test.go` | `smoke-user.sh` (audit step) | `observability/15-audit-chain.spec.ts` | RUNBOOK §11 |
| Inspector | `internal/inspector/ring_test.go`, `internal/api/inspector_test.go` | — | `observability/24-inspector.spec.ts` | — |
| IP-geo | `internal/proxy/ipgeo_test.go`, `internal/proxy/geo_test.go`, `internal/db/ipgeo_test.go`, `internal/api/ipgeo_test.go` | `smoke-user.sh` (ip-geo 403 step) | `observability/29-ipgeo.spec.ts` | — |
| Webhooks | `internal/db/webhooks_test.go`, `cmd/server/e2e_webhooks_test.go` | — | `webhooks/16-webhooks.spec.ts`, `webhooks/30-webhooks-delivery.spec.ts` | RUNBOOK §11 |
| OpenAPI viewer | — | — | `admin/17-openapi-viewer.spec.ts` | RUNBOOK §11 |
| Retention | `internal/retention/compactor_test.go`, `internal/api/retention_test.go` | — | `admin/18-retention.spec.ts` | RUNBOOK §11 |
| Quota / rate-limit | `internal/quota/engine_test.go`, `internal/db/ratelimits_test.go`, `cmd/server/e2e_quota_test.go` | `smoke-user.sh` step 7 (quota-429) | `admin/25-quota-rate-limit.spec.ts`, `admin/32-login-rate-limit.spec.ts` | RUNBOOK §7 |
| Backups | `internal/db/backup_test.go`, `internal/api/backup_test.go`, `cmd/server/backup_test.go`, `cmd/server/restore_test.go` | — | `admin/28-backups.spec.ts` | — |
| Postgres backend | `cmd/server/postgres_test.go`, `cmd/server/e2e_v050_postgres_test.go` | — | `resilience/19-postgres-swap.spec.ts` | RUNBOOK §12 |
| Reconnect after restart | `internal/backoff/backoff_test.go`, `internal/server/keepalive_test.go` | `smoke-user.sh` (restart & reconnect step) | `resilience/20-relay-restart.spec.ts` | RUNBOOK §13 |
| Failover | — | — | `resilience/27-failover.spec.ts` | — |

Empty (`—`) cells are coverage gaps: file them to the backlog, do not fix them
here.
