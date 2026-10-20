# HTTP API

Burrow exposes a JSON REST API at `/api/v1`. Every dashboard action—creating services, minting tokens, configuring access policy, reviewing the audit log—is available through the same API the UI uses.

While the relay has no [services domain](/guide/configuration#services-domain), a dashboard session must confirm its password before it calls a route that mints a secret or changes who may do what: `POST /auth/reauth` with `{"password": "…"}` opens a ten-minute window for that session. Outside it those routes answer `403 {"error":"reauth_required"}` (they are marked in the OpenAPI description); automation tokens are exempt. A session's `id` in `GET /sessions` is a hash, not the cookie value; the audit log names sessions by the same hash. Audit rows written by older relays hold the raw session id as the subject of `session.create` and `session.delete`: `GET /audit/events` and `/audit/export` show their hash instead (the export marks such a line `subject_redacted`, and offline verification skips the hash recomputation for it; the stored rows and `POST /audit/verify` are unchanged). Changing your password signs your other sessions out. Wrong passwords to `/auth/change-password` and `/auth/reauth` are rate-limited per address and, together, to 10 a minute per session (a correct password is refused during that minute on that session only); a user past 100 wrong passwords an hour is flagged in the audit log but never locked out. Only `session.*` rows older than the first start of this version (`audit.session_ref_cutoff` setting) are shown redacted.

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

Mint an automation token from the dashboard (Settings → Automation tokens) or via `POST /api/v1/automation/tokens`. Use it as a standard `Authorization` header — no CSRF header needed:

```sh
curl -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  https://burrow.insingo.com/api/v1/tunnels
```

Bearer tokens carry the permissions declared at mint time, scoped to the issuing user's role.

`GET /api/v1/automation/presets` lists the presets the dashboard offers
(`read-only`, `operator`, `full`), each cut to the permissions you hold.
Send the list you want as `permissions` when you mint: the token stores the
list, so a later change to a preset never widens it. `GET /api/v1/me`
returns your effective permissions as `permissions`.

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
| `/api/v1/openapi/viewer/` | Embedded browser viewer (needs `metrics:read`; the admin role holds it) |

The repo also ships `docs/openapi.yaml` as a canonical reference. It is kept in sync with the router by `TestOpenAPI_RouteCoverage` — a build-time test that fails if any route is missing from the spec.

::: info
Health probes (`/healthz`, `/readyz`) and the OpenAPI doc-serving endpoints themselves are excluded from the spec by convention.
:::

---

## Operation ids and extension fields

Every operation in the spec has an `operationId`. Generated clients name their methods after it, and the management MCP server names its tools after it (`serviceApiKeysList` becomes the tool `service_api_keys_list`).

| Rule | Example |
|------|---------|
| lowerCamelCase, the resource first, then the action | `servicesList`, `serviceGet`, `providerCreate`, `aiModelUpdate` |
| A list is plural, one item singular | `webhooksList`, `webhookUpdate` |
| A sub-resource carries its parent | `serviceApiKeysList`, `providerModelsSync` |
| An action is the last path segment | `webhookPause`, `backupVerify` |
| Acronyms are written as words | `serviceIpGeoGet`, `mcpToolsList` |

An id is a public name: it is added and never changed. `TestOpenAPI_OperationIDs` holds the list and fails on a rename.

Three extension fields appear on operations:

| Field | Meaning |
|-------|---------|
| `x-burrow-mcp` | `false` keeps the operation out of the tool list of the management MCP server. An operation without the field may become a tool. |
| `x-burrow-permission` | Who may call the operation: `kind` is `public` (no credentials), `self` (any signed-in caller), `needs` (one of `any_of`; with `every: true` all of them) or `dashboard` (a dashboard session with one of `any_of`; an automation token gets 403). It mirrors the relay's route table, and a test keeps the two equal. |
| `x-burrow-destructive` | `true` marks an operation that is not a DELETE but removes or revokes something; MCP clients must confirm it. |

`x-burrow-mcp: false` is set on login, logout, password confirmation (`/auth/reauth`) and password change; on the client sign-in endpoints (discovery, start, poll, and the sign-in request lookup, approval and denial in the dashboard); on downloads and exports (backup archive, audit, cost and connection-log export); on the event streams; on backup restore and its status poll; on approving tools of a tool source and changing their overrides (an agent must not approve tool text that may have been written to steer it, nor rename or reclassify tools); on creating and changing a tool source and on listing its tools (an agent must not point a source and its credential at another host, nor read the tool text that could steer it into doing so; the source list and a single source stay); on the OAuth consent request, its approval and its denial (an agent must not consent in a person's name); on every write that routes a credential to an address or turns off inspection: creating and changing a provider and its upstream, binding and removing a service's upstream credential, the SMTP settings, adding and changing a custom domain (it carries a TLS key), creating, changing, deleting, testing, pausing and resuming a webhook, the guardrail settings, the redaction settings and rules, a service's AI config (redaction, guardrails, inspector), the retention settings and a service's gateway-only switch (an agent must not re-route credentials or switch off inspection; the reads of all of these stay tools); on registering an OAuth client (an agent must not choose where authorization codes are sent); on the writes that widen access, grant privileges, mint a data-plane secret into the model's context or plant a lasting instruction: a service's and a tunnel's access mode, changing a gateway key, minting a client token or a service API key, creating, changing and deleting users and roles, and creating and changing a synthetic model, whose `system_text` reaches other users' requests (a service's access policy and IP/geo rules and minting an automation token stay tools); and on the OpenAPI viewer.

A test (`TestRegistry_WritesAreClassified` in `internal/mcpserv`) holds the golden set of every write that is a tool: a write added later fails it until somebody adds it to the set or marks it `x-burrow-mcp: false`. Its message says why the write looks sensitive when its path has a segment such as `/inspector`, `/guardrails`, `/redaction`, `/retention`, `/ai-config`, `/upstream` or `/webhooks`, or its body has an address (`url`, `host`, `endpoint`, `uri` and their `_`-suffixed forms), a credential field (`token`, `api_key`, `secret`, a credential slot, a password) or redaction, guardrail or inspector settings; such a write in the set also needs a reason in an explicit allow list (today: the redaction, guardrail and webhook previews and the inspector replays).

The schemas are checked against the server: tests compare every JSON request and response schema with the Go type the handler decodes or writes, so a field added to the API without the spec fails the build.

---

## Resource groups

The table below lists the major resource groups, their path prefixes, and what they cover. See the OpenAPI viewer at `/api/v1/openapi/viewer/` or `docs/openapi.yaml` for the full endpoint list, request bodies, and response schemas.

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
| **access-mode** | `/api/v1/services/{id}/access-mode` | Set access mode (`open`, `api_key`, `burrow_login`, `mtls`) |
| **access-policy** | `/api/v1/services/{id}/access-policy` | Role allowlist of `burrow_login` mode |
| **webhooks** | `/api/v1/webhooks/` | HMAC outbound webhooks: create, list, pause, resume, delivery log |
| **audit** | `/api/v1/audit/` | Immutable audit event log; chain verify and export |
| **backups** | `/api/v1/backups/` | Create, list, verify, download, delete and restore database backups |
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

Successful state-changing requests that have nothing to return respond with `204 No Content`.

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
  -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-mode \
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
  -H "Content-Type: application/json" \
  -X POST https://burrow.insingo.com/api/v1/services/abc123/api-keys \
  -d '{"name":"ci"}'
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
  https://burrow.insingo.com/api/v1/audit/export \
  -o audit.ndjson
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
