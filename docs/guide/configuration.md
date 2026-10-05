# Configuration

All server behaviour is controlled through environment variables prefixed `BURROW_`.
No config file is required — export the variables before running `burrowd serve`,
or drop them into a `.env` file and pass `--env-file` to Docker.

The `burrow` client is configured via [`burrow.yaml`](#burrow-yaml) or inline flags.

---

## Environment variables

### ACME (automatic TLS)

Set `BURROW_ACME_DOMAIN` and `BURROW_ACME_EMAIL` to enable built-in certificate
management. Burrowd calls Let's Encrypt before any listener starts — no nginx,
no Certbot, no manual renewal. See [Deploy on a server](/guide/deploy) for a
complete production example.

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_ACME_DOMAIN` | `""` | Comma-separated hostnames to obtain certs for. Setting any value enables ACME. First hostname becomes the primary domain. |
| `BURROW_ACME_EMAIL` | `""` | Contact email sent to the CA. Required when `ACME_DOMAIN` is set; used for expiry notices. |
| `BURROW_ACME_CA` | `https://acme-v02.api.letsencrypt.org/directory` | ACME directory URL. Swap for the [staging URL](#acme-staging) during testing. |
| `BURROW_ACME_STORAGE` | `<dir(database_path)>/acme` | Directory where certificates and account keys are persisted across restarts. |

::: warning ACME is mutually exclusive with file certificates
Setting `BURROW_ACME_DOMAIN` and any of `BURROW_TLS_CERT`, `BURROW_TLS_KEY`,
`BURROW_HTTP_TLS_CERT`, `BURROW_HTTP_TLS_KEY`, `BURROW_HTTP_PROXY_TLS_CERT`, or
`BURROW_HTTP_PROXY_TLS_KEY` at the same time is unsupported. Use one approach or
the other.
:::

::: info What ACME changes at runtime
- The dashboard listener auto-promotes from `:8080` to `:443` (only when `BURROW_HTTP_LISTEN` is still the default `:8080`).
- A `:80` listener starts to serve HTTP-01 challenges and redirect HTTP → HTTPS.
- The host-routed proxy ingress, if enabled, adopts the ACME-managed certificate.
- The control channel (`:7000`) also uses the ACME certificate; the port itself does not change.
- `BURROW_AUTH_DOMAIN` is **not required** when ACME is on — burrowd infers the base domain from the first ACME domain.
:::

#### ACME staging {#acme-staging}

Use the Let's Encrypt staging CA during testing to avoid rate limits:

```env
BURROW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory
```

Staging certificates are not trusted by browsers, but the full ACME flow is
exercised. Switch back to the production CA (the default) when going live.

---

### Listeners

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_LISTEN` | `:7000` | Control channel (TLS + yamux). Clients connect here. |
| `BURROW_HTTP_LISTEN` | `:8080` | Dashboard and REST API. Promoted to `:443` by ACME when still at its default value. |
| `BURROW_HTTP_PROXY_LISTEN` | `""` | Opt-in host-routed ingress (for example `:8443`). Empty = off. See below. |
| `BURROW_PUBLIC_BIND` | `0.0.0.0` | Network interface for TCP tunnel data ports. |
| `BURROW_PORT_MIN` | `9000` | Inclusive lower bound for auto-assigned TCP tunnel ports. |
| `BURROW_PORT_MAX` | `9100` | Inclusive upper bound for auto-assigned TCP tunnel ports. |

`BURROW_HTTP_PROXY_LISTEN` is empty by default. Set it (for example to `:8443`)
to start the opt-in host-routed ingress, which serves services at
`https://<slug>.<domain>:8443/` and needs wildcard DNS and a wildcard
certificate. mTLS access mode and custom domains only work with this ingress.
With `burrow_login`, sign-in returns to the service only on `/svc/<slug>/`; over
this ingress the visitor lands on the dashboard root.

::: tip Stable TCP ports
Set `remote: 9001` (or any value in the `PORT_MIN`–`PORT_MAX` range) in
`burrow.yaml` to keep a fixed port across reconnects. See
[Expose services](/guide/expose-services).
:::

---

### TLS — file certificates

An alternative to ACME for operators who manage their own certificates (e.g.
internal CA, wildcard cert from a managed service). Mutually exclusive with
ACME variables.

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_TLS_CERT` | `certs/dev-server.pem` | PEM certificate for the control channel (`:7000`). |
| `BURROW_TLS_KEY` | `certs/dev-server-key.pem` | PEM private key for the control channel. |
| `BURROW_HTTP_TLS_CERT` | `""` | PEM certificate for the dashboard/API listener. Empty = plain HTTP. |
| `BURROW_HTTP_TLS_KEY` | `""` | PEM private key for the dashboard/API listener. |
| `BURROW_HTTP_PROXY_TLS_CERT` | `""` | PEM certificate for the host-routed proxy ingress. |
| `BURROW_HTTP_PROXY_TLS_KEY` | `""` | PEM private key for the proxy ingress. |

::: tip Dev certs shortcut
`burrowd serve --dev-certs` generates self-signed certificates in `./certs/`
and starts immediately. Useful for local development — no cert management needed.
:::

---

### Behind a reverse proxy

When burrowd runs behind a load balancer or reverse proxy that terminates TLS,
configure these two variables so that sessions use the correct cookie flags and
client IPs resolve correctly.

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_HTTP_SECURE_COOKIES` | `false` | Set `true` when TLS is terminated upstream. Without this, session cookies lack the `Secure` flag and browsers may reject them over HTTPS. |
| `BURROW_TRUSTED_PROXIES` | `""` | Comma-separated CIDRs or IPs whose `X-Forwarded-For` / `X-Real-IP` headers are trusted. Empty = no forwarded headers trusted; all client IPs read directly from the connection. |

::: warning Cookie flag
If the dashboard is served over HTTPS through a reverse proxy and
`BURROW_HTTP_SECURE_COOKIES` is `false`, login will appear to work but the
session cookie will be silently dropped by modern browsers.
:::

Example for a proxy running on localhost:

```env
BURROW_HTTP_SECURE_COOKIES=true
BURROW_TRUSTED_PROXIES=127.0.0.1,::1
```

---

### Admin seed

Set before the first `burrowd serve`. Burrowd seeds the admin account on first
boot and skips this step on subsequent starts (idempotent).

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_ADMIN_EMAIL` | `""` | Email address of the first admin account. |
| `BURROW_ADMIN_PASSWORD` | `""` | Password for the first admin account. Also accepts `BURROW_ADMIN_PASSWORD_FILE` (see [_FILE convention](#file-convention)). |

```env
BURROW_ADMIN_EMAIL=admin@burrow.insingo.com
BURROW_ADMIN_PASSWORD=changeme
```

::: tip Change after first boot
Once the server has started and the account exists, update the password through
the dashboard (Account → Change password) or the API. The seed variables are
ignored on subsequent boots.
:::

---

### Database

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_DATABASE_PATH` | `./burrow.db` | Path to the SQLite database file. |
| `BURROW_BACKUP_DIR` | `<database_path>.backups` | Directory where backup archives are written. |

::: details PostgreSQL (experimental)
A PostgreSQL backend is available but requires a custom build:

```sh
go build -tags=postgres ./cmd/server
```

Then set:

```env
BURROW_EXPERIMENTAL_POSTGRES_BACKEND=true
BURROW_DATABASE_URL=postgres://user:pass@host:5432/burrow
```

`BURROW_DATABASE_URL` also accepts the `_FILE` form:
`BURROW_DATABASE_URL_FILE=/run/secrets/db-url`.
:::

---

### Logging

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error`. |
| `BURROW_LOG_FORMAT` | `text` | Output format: `text` (human-readable) or `json` (structured, for log aggregators). |

JSON format example — useful with Loki, Datadog, or any OpenTelemetry collector:

```env
BURROW_LOG_FORMAT=json
BURROW_LOG_LEVEL=warn
```

---

### Security

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_LOGIN_RATE_LIMIT_PER_IP` | `0` (= built-in default of 10 req/min) | Per-IP login rate limit. Set a positive integer to override. |
| `BURROW_CERT_VALIDATION_ROOTS_FILE` | `""` | Path to a PEM CA bundle used when validating custom-domain TLS certificates. |

---

### Routing

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_AUTH_DOMAIN` | `""` | Base domain of the relay. HTTP services are served at `https://<domain>/svc/<slug>/`. Inferred from `BURROW_ACME_DOMAIN` when ACME is on — no need to set separately. |

---

### SMTP

SMTP host, port, username, and sender address are configured through the
dashboard (Settings → SMTP). The password is secrets-only and must be supplied
via environment:

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_SMTP_PASSWORD` | `""` | SMTP authentication password. Also accepts `BURROW_SMTP_PASSWORD_FILE`. |

---

### Advanced

| Variable | Default | Purpose |
|----------|---------|---------|
| `BURROW_MCP_LISTEN` | `""` | MCP JSON-RPC listener address (e.g. `:7800`). Empty = MCP disabled. |
| `BURROW_MCP_TOKEN` | `""` | Bearer token for the MCP endpoint. Also accepts `BURROW_MCP_TOKEN_FILE`. |
| `BURROW_GEO_DB_PATH` | `""` | Path to a MaxMind GeoLite2 `.mmdb` file. Required for the `geo` access mode (needs `-tags=geo` build). |
| `BURROW_PRICING_PATH` | `""` | Path to a YAML file that overrides the embedded AI gateway pricing table. |
| `BURROW_AI_ALLOW_PRIVATE_UPSTREAMS` | `false` | Lets direct AI providers use a base URL that resolves to a private, loopback or link-local address (a self-hosted model server on the LAN). Off = the relay refuses to connect to such addresses. Accepted values: `true`, `TRUE`, `1`, `false`, `FALSE`, `0`; empty means `false`. Any other value, such as `yes`, is an error and the server does not start. |

---

### `_FILE` secret convention {#file-convention}

Any variable that holds a secret value accepts a parallel `_FILE` variant that
names a file containing the value. This is the Docker Secrets / Kubernetes
Secrets pattern — secrets are mounted as files and never appear in environment
variable listings.

The `_FILE` form takes precedence when both are set.

Variables that commonly use `_FILE`:

| `_FILE` variable | Reads secret from |
|-----------------|-------------------|
| `BURROW_ADMIN_PASSWORD_FILE` | Admin seed password |
| `BURROW_TLS_CERT_FILE` | Control channel certificate |
| `BURROW_TLS_KEY_FILE` | Control channel private key |
| `BURROW_SMTP_PASSWORD_FILE` | SMTP auth password |
| `BURROW_MCP_TOKEN_FILE` | MCP bearer token |
| `BURROW_DATABASE_URL_FILE` | PostgreSQL DSN |

Example Docker Compose usage:

```yaml
services:
  burrowd:
    image: ghcr.io/ankoehn/burrow:develop
    secrets:
      - burrow_admin_password
    environment:
      BURROW_ADMIN_EMAIL: admin@burrow.insingo.com
      BURROW_ADMIN_PASSWORD_FILE: /run/secrets/burrow_admin_password

secrets:
  burrow_admin_password:
    external: true
```

---

## `burrow.yaml` — client configuration {#burrow-yaml}

The client reads a YAML file when started with `burrow connect --config burrow.yaml`.

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE      # or use token_file: /run/secrets/burrow-token
services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http                  # "http" or "tcp"; defaults to "tcp" if omitted
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001                # fixed public TCP port; 0 = auto-assigned
```

### Field reference

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `server` | Yes | — | Relay address including port, e.g. `burrow.insingo.com:7000`. |
| `token` | One of `token`/`token_file` | — | Client token minted from the dashboard or via `burrowd token`. |
| `token_file` | One of `token`/`token_file` | — | Path to a file containing the token value. |
| `services` | Yes | — | List of services to expose. |
| `services[].name` | Yes | — | Display name; shown in the dashboard. |
| `services[].local` | Yes | — | Local address to forward traffic to, e.g. `127.0.0.1:3000`. |
| `services[].type` | No | `tcp` | `http` or `tcp`. HTTP tunnels get a slug and a `/svc/<slug>/` URL; TCP tunnels get a port. |
| `services[].remote` | No | `0` | TCP only. Requested public port. `0` = auto-assigned from `PORT_MIN`–`PORT_MAX`. |

### Single-tunnel flags (no config file)

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token bur_YOUR_TOKEN_HERE \
  --local 127.0.0.1:3000 \
  --type http \
  --name my-app
```

Available flags: `--server`, `--token`, `--local` (default `127.0.0.1:3000`),
`--remote` (default `0`), `--name`, `--type` (default `tcp`), `--insecure`,
`--cacert`, `--server-name`.

See the full [CLI reference](/reference/cli) for all flags and subcommands.

---

## Complete `.env` example

A minimal production deployment using ACME:

```env
# Identity
BURROW_ADMIN_EMAIL=admin@burrow.insingo.com
BURROW_ADMIN_PASSWORD_FILE=/run/secrets/burrow_admin_password

# ACME — automatic TLS
BURROW_ACME_DOMAIN=burrow.insingo.com
BURROW_ACME_EMAIL=ops@insingo.com

# Database
BURROW_DATABASE_PATH=/data/burrow.db

# Logging
BURROW_LOG_FORMAT=json
BURROW_LOG_LEVEL=info
```

That is all that is needed. Burrowd obtains certificates, promotes the dashboard
to `:443`, starts the HTTP-01 challenge solver on `:80`, and infers the routing
domain from `BURROW_ACME_DOMAIN`.

For a step-by-step deploy walkthrough including Docker Compose and firewall
rules, see [Deploy on a server](/guide/deploy).
