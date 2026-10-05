# Access control & security

Burrow lets you decide, per service, who can reach your tunnelled upstream. Four
access modes cover everything from "anyone can call this webhook" to "only my
mTLS client gets in." This page explains each mode, how to configure it, and a
short security checklist for hardening your relay.

---

## Access modes

| Mode | Who can reach the upstream |
|------|---------------------------|
| `open` | Everyone — no authentication |
| `api_key` | Callers presenting a valid API key |
| `burrow_login` | Burrow users who are logged in to the dashboard |
| `mtls` | Clients presenting a trusted TLS client certificate |

### `open`

No gate. The relay forwards every request to your upstream without checking
anything. Use it for public-facing services (static sites, webhooks that carry
their own HMAC signature, etc.).

### `api_key`

The relay validates an API key on every inbound request before forwarding.
Keys are minted per service and stored hashed in the database.

**When to use:** machine-to-machine calls, CI/CD pipelines, or any non-browser
client that can send a custom header.

### `burrow_login`

The relay checks that the caller has an active Burrow session cookie. Requests
without a valid session get a `401`. This is session-based authentication
against the same user directory that drives the dashboard.

**When to use:** internal tools that your team members access via a browser
after logging in to `https://burrow.insingo.com`.

### `mtls`

Mutual TLS. The relay demands a client certificate signed by a CA you upload.
The TLS handshake itself rejects any connection without a valid cert — the
access check is defense-in-depth.

**When to use:** service-to-service calls where you control both ends and want
cryptographic identity rather than shared secrets.

::: warning mTLS requires the host-routed ingress
mTLS requires the opt-in host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`) and is
not available with path URLs. The `https://burrow.insingo.com/svc/<slug>/`
endpoint shares the dashboard TLS connection and **cannot** carry a per-service
client certificate. The dashboard no longer offers mTLS; a service still in
`mtls` mode must be moved to another mode by hand.
:::

---

## Configuring the access mode

### Dashboard

Open the service detail page, click **Configure**, and select the mode from the
**Access** dropdown. For `mtls`, you will also be prompted to paste or upload
the PEM-encoded CA that signed your client certificates.

### REST API

```sh
PUT /api/v1/services/{id}/access-policy
```

```sh
# Set to api_key
curl -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-policy \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"access_mode": "api_key"}'

# Set to open
curl -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-policy \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"access_mode": "open"}'
```

---

## API key details

### Minting a key

Via the dashboard (service detail page → **API Keys** → **Add key**), or via
the API:

```sh
POST /api/v1/services/{id}/api-keys
```

```sh
curl -X POST https://burrow.insingo.com/api/v1/services/abc123/api-keys \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "ci-pipeline"}'
```

The response body includes the raw key value. Copy it — it is shown only once.

### Sending the key — default (`Authorization: Bearer`)

By default, callers send the key in the standard `Authorization` header:

```sh
curl https://abc123.burrow.insingo.com/api/data \
  -H "Authorization: Bearer buk_YOUR_API_KEY"
```

The relay strips the `Bearer ` prefix before validating.

### Sending the key — custom header (`api_key_header`)

You can configure a custom header name (e.g. `X-Api-Key`) in the service
settings. When a custom header is set, callers send the **raw key value** with
no prefix:

```sh
curl https://abc123.burrow.insingo.com/api/data \
  -H "X-Api-Key: buk_YOUR_API_KEY"
```

::: info No `Bearer` prefix on custom headers
The relay calls `strings.TrimSpace(value)` directly on the custom header value.
Do **not** include `Bearer ` — the call will fail with `401 invalid api key`.
:::

### Error responses

| Situation | Status | Body |
|-----------|--------|------|
| Header missing entirely | `401` | `{"error":"missing api key"}` |
| Header present but key invalid | `401` | `{"error":"invalid api key"}` |

---

## Security checklist

### Firewall

Open only the ports Burrow needs. With ACME (recommended):

| Port | Purpose |
|------|---------|
| `80` | ACME HTTP-01 challenge + HTTPS redirect |
| `443` | Dashboard, REST API, and path-routed tunnels |
| `8443` | Host-routed ingress (opt-in, off by default; mTLS) |
| `7000` | Client control channel (TLS) |

If you use raw TCP tunnels, also open the specific port you pin in `remote:`
(e.g. `9001`). No need to open the full `9000`–`9100` range unless you expose
many dynamic TCP services.

::: warning Keep `8443` closed unless you enable the host-routed ingress
`BURROW_HTTP_PROXY_LISTEN` is empty by default, so nothing listens on `8443`.
If you turn it on, restrict the port at the network level to known source IPs
unless you rely on it for mTLS.
:::

### Strong admin password

Never leave the default or a weak password on the admin account. Pass the
secret via an environment file or Docker secret — not a shell variable:

```env
BURROW_ADMIN_EMAIL=admin@burrow.insingo.com
BURROW_ADMIN_PASSWORD_FILE=/run/secrets/burrow-admin-password
```

The `_FILE` form reads the value from the file at startup. See the
[Configuration reference](/guide/configuration) for all `_FILE`-capable vars.

### Keep the image updated

The `develop` channel rebuilds on every push. Pull regularly to pick up
security fixes:

```sh
docker pull ghcr.io/ankoehn/burrow:develop
docker compose up -d
```

### Back up the database

Burrow stores all configuration, users, and audit logs in a single SQLite file
(`burrow.db` by default). Take regular backups:

```sh
# CLI backup (writes a timestamped archive to BURROW_BACKUP_DIR)
burrowd backup

# Or via the API
curl -X POST https://burrow.insingo.com/api/v1/backup \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN"
```

See [Operations](/guide/operations) for restore instructions and retention
settings.

### Secure cookies

If TLS is terminated by an upstream proxy rather than by `burrowd` itself, set:

```env
BURROW_HTTP_SECURE_COOKIES=true
BURROW_TRUSTED_PROXIES=10.0.0.1/32
```

Without `BURROW_HTTP_SECURE_COOKIES=true` the session cookie will not carry the
`Secure` flag, which means browsers will refuse to send it on HTTPS pages.

::: tip ACME handles TLS for you
With `BURROW_ACME_DOMAIN` set, `burrowd` terminates TLS itself. You do **not**
need `BURROW_HTTP_SECURE_COOKIES` or a reverse proxy for HTTPS — the cookie
flag is set correctly by default.
:::

---

## Related pages

- [Deploy on a server](/guide/deploy) — firewall setup and ACME configuration
- [Expose services](/guide/expose-services) — tunnel types and URL shapes
- [Configuration](/guide/configuration) — full `BURROW_*` env var reference
- [Operations](/guide/operations) — backup, restore, and log settings
