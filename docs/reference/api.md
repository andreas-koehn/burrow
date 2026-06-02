# HTTP API

Burrow exposes a JSON REST API at `/api/v1`. Every dashboard action—creating services, minting tokens, configuring access policy, reviewing the audit log—is available through the same API the UI uses.

The API is served on the same port as the dashboard: `:8080` by default, or `:443` when [ACME is enabled](/guide/deploy#built-in-acme).

---

## Authentication

Every request needs one of two credentials.

### Session cookie

Log in once and carry the `burrow_session` cookie:

```sh
curl -c cookies.txt -X POST https://burrow.insingo.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"changeme"}'
```

State-changing requests (POST, PUT, DELETE, PATCH) with a session cookie must also include the CSRF double-submit header:

```sh
curl -b cookies.txt \
  -H "X-CSRF-Token: $(grep burrow_csrf cookies.txt | awk '{print $NF}')" \
  -X DELETE https://burrow.insingo.com/api/v1/tokens/tok_abc123
```

### Bearer token (automation)

Mint an automation token from the dashboard (Settings → Tokens) or via `POST /api/v1/tokens`. Use it as a standard `Authorization` header — no CSRF header needed:

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  https://burrow.insingo.com/api/v1/tunnels
```

Bearer tokens carry the permissions declared at mint time, scoped to the issuing user's role.

::: tip
Bearer tokens are the right choice for CI pipelines, scripts, and SDK clients. Session cookies are for the dashboard UI.
:::

---

## OpenAPI spec

The server always serves its own spec — no need to keep a local copy in sync:

| URL | Content |
|-----|---------|
| `/api/v1/openapi.yaml` | Machine-readable spec (YAML) |
| `/api/v1/openapi.json` | Machine-readable spec (JSON) |
| `/api/v1/openapi` | Embedded browser viewer (Swagger UI) |

The repo also ships `docs/openapi.yaml` as a canonical reference. It is kept in sync with the router by `TestOpenAPI_RouteCoverage` — a build-time test that fails if any route is missing from the spec.

::: info
Health probes (`/healthz`, `/readyz`) and the OpenAPI doc-serving endpoints themselves are excluded from the spec by convention.
:::

---

## Resource groups

The table below lists the major resource groups, their path prefixes, and what they cover. See the OpenAPI viewer at `/api/v1/openapi` or `docs/openapi.yaml` for the full endpoint list, request bodies, and response schemas.

| Group | Prefix | What it covers |
|-------|--------|----------------|
| **auth** | `/api/v1/auth/` | Login, logout, password change |
| **users** | `/api/v1/users/` | Admin user management (create, list, suspend, edit role) |
| **roles** | `/api/v1/roles/` | Role + permission catalog; editable custom roles |
| **sessions** | `/api/v1/sessions/` | Per-user session list and revoke |
| **tokens** | `/api/v1/tokens/` | Personal client tokens (`bur_…`) for `burrow connect` |
| **tunnels** | `/api/v1/tunnels/` | Live tunnel view — what is connected right now |
| **services** | `/api/v1/services/` | Durable service config: access mode, API keys, policy, AI config, inspector, cache |
| **api-keys** | `/api/v1/services/{id}/api-keys/` | Mint and revoke per-service API keys |
| **access-policy** | `/api/v1/services/{id}/access-policy` | Set access mode (`open`, `api_key`, `burrow_login`, `mtls`) |
| **webhooks** | `/api/v1/webhooks/` | HMAC outbound webhooks: create, list, pause, resume, delivery log |
| **audit** | `/api/v1/audit/` | Immutable audit event log; chain verify and export |
| **backups** | `/api/v1/backups/` | Create and restore database backups |
| **clients** | `/api/v1/clients/` | Live control-session overview (which clients are connected) |
| **settings** | `/api/v1/settings/` | Admin settings: SMTP, retention, config |
| **events** | `/api/v1/events` | Server-sent event stream for dashboard live refresh |
| **metrics** | `/metrics` | Prometheus 0.0.4 scrape endpoint (`metrics:read` permission) |

---

## Common request and response shapes

All responses use `Content-Type: application/json`. Errors always carry an `error` field:

```json
{"error": "invalid api key"}
```

Successful state-changing requests that have nothing to return respond with `204 No Content` or `{"ok": true}`.

### Service object

```json
{
  "id": "abc123",
  "name": "my-app",
  "type": "http",
  "access_mode": "api_key",
  "remote_port": 0,
  "local_addr": "127.0.0.1:3000",
  "connected": true
}
```

### Tunnel object

```json
{
  "id": "abc123",
  "name": "my-app",
  "type": "http",
  "remote_port": 0,
  "local_addr": "127.0.0.1:3000",
  "bytes_in": 10240,
  "bytes_out": 4096,
  "connected": true
}
```

### Token object

```json
{
  "id": "tok_abc123",
  "name": "laptop",
  "created_at": "2026-06-01T09:00:00Z"
}
```

---

## Key operations

### List live tunnels

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  https://burrow.insingo.com/api/v1/tunnels
```

### Create a service

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -X POST https://burrow.insingo.com/api/v1/services \
  -d '{"service_id":"my-app","title":"My App"}'
```

### Set access mode

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-policy \
  -d '{"access_mode":"api_key"}'
```

To use a custom header name instead of `Authorization`:

```sh
-d '{"access_mode":"api_key","api_key_header":"X-Api-Key"}'
```

See [Access control](/guide/access-control) for the full header-semantics rules.

### Mint a service API key

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -X POST https://burrow.insingo.com/api/v1/services/abc123/api-keys
```

### Mint a client token

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -X POST https://burrow.insingo.com/api/v1/tokens \
  -d '{"name":"laptop"}'
```

The response includes the raw token value (`bur_…`) — store it immediately; it is not recoverable.

### Trigger a backup

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -X POST https://burrow.insingo.com/api/v1/backups
```

### Export the audit log

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  "https://burrow.insingo.com/api/v1/audit?limit=1000" \
  -o audit-export.json
```

---

## HTTP status codes

| Code | Meaning |
|------|---------|
| `200` | OK — response body present |
| `204` | OK — no body |
| `400` | Bad request — malformed JSON or missing required field |
| `401` | Missing or invalid session / bearer token |
| `403` | Authenticated but lacking permission |
| `404` | Resource not found |
| `429` | Rate-limited (login endpoint is per-IP rate-limited) |

---

## CSRF

Session-cookie requests that mutate state must mirror the `burrow_csrf` cookie value in the `X-CSRF-Token` header (double-submit pattern). Bearer token requests are exempt — the token itself is the CSRF defense.

::: warning
Omitting `X-CSRF-Token` on a cookie-authenticated POST/PUT/DELETE returns `403 Forbidden`, not `401`. This is by design.
:::

---

## Related pages

- [Access control & security](/guide/access-control) — access modes, API key header semantics, mTLS constraints
- [CLI reference](/reference/cli) — `burrowd token` to mint tokens from the server CLI
- [Operations](/guide/operations) — backup/restore via CLI and API
- [Configuration](/guide/configuration) — all `BURROW_*` env vars
