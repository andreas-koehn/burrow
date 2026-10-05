# CLI Reference

Burrow ships two binaries: **`burrowd`** (the relay server) and **`burrow`** (the local client).

---

## burrowd — relay server

```
burrowd <command> [flags]
```

### serve

Start the relay server. Opens (and migrates) the database, seeds the first
admin, starts all listeners, and serves the embedded dashboard SPA.

```sh
burrowd serve
burrowd serve --dev-certs
```

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:7000` | Control-channel listen address (overrides `BURROW_LISTEN`) |
| `--tls-cert` | `certs/dev-server.pem` | Control-channel TLS certificate PEM |
| `--tls-key` | `certs/dev-server-key.pem` | Control-channel TLS key PEM |
| `--dev-certs` | `false` | Generate self-signed certs in `./certs/` and start |

All flags can also be set via environment variables — see the full table in
[Configuration](/guide/configuration). Flags take precedence over env vars.

::: tip ACME (recommended for production)
Set `BURROW_ACME_DOMAIN` and `BURROW_ACME_EMAIL` instead of file certs.
burrowd fetches and auto-renews Let's Encrypt certificates for every listener.
See [Deploy on a server](/guide/deploy).
:::

::: warning --dev-certs is for local testing only
Generated certs are self-signed and browser-untrusted. Use ACME or real file
certs in production.
:::

**Key environment variables for `serve`**

```env
# Admin seed (first boot only)
BURROW_ADMIN_EMAIL=admin@example.com
BURROW_ADMIN_PASSWORD=changeme

# ACME (mutually exclusive with file certs)
BURROW_ACME_DOMAIN=burrow.insingo.com
BURROW_ACME_EMAIL=ops@insingo.com

# Listeners (defaults shown)
BURROW_LISTEN=:7000
BURROW_HTTP_LISTEN=:8080
BURROW_HTTP_PROXY_LISTEN=

# Database
BURROW_DATABASE_PATH=./burrow.db

# Logging
BURROW_LOG_LEVEL=info      # debug | info | warn | error
BURROW_LOG_FORMAT=text     # text | json
```

**Listener behaviour summary**

| Port | Protocol | Condition |
|------|----------|-----------|
| `:7000` | TLS (yamux) | Always — control channel |
| `:8080` | HTTP | Default dashboard + API; promotes to `:443` when ACME is on |
| `:443` | HTTPS | Dashboard + API when ACME is enabled |
| `:8443` | HTTP or TLS | Opt-in host-routed ingress (off unless `BURROW_HTTP_PROXY_LISTEN=:8443` is set) |
| `:80` | HTTP | ACME HTTP-01 challenge solver + HTTPS redirect (ACME mode only) |

---

### token

Mint a client token for an existing user. Writes directly to the database — no
running server is required. Useful for scripted provisioning or CI.

```sh
burrowd token --email admin@example.com --name ci-runner
```

The token is printed to stdout; progress/label lines go to stderr.

| Flag | Default | Required | Description |
|------|---------|----------|-------------|
| `--email` | — | Yes | Email of the existing user |
| `--name` | `cli` | No | Human-readable label for the token |

::: info
The user must already exist (created by seeding or the dashboard). To create
the first admin, set `BURROW_ADMIN_EMAIL` / `BURROW_ADMIN_PASSWORD` and run
`burrowd serve` once.
:::

**Example — store the token in a variable**

```sh
TOKEN=$(burrowd token --email admin@example.com --name ci 2>/dev/null)
```

---

### backup

Create a database backup archive. Writes a timestamped archive to the configured
backup directory (default: `<database_path>.backups`).

```sh
burrowd backup
```

The backup directory can be overridden with `BURROW_BACKUP_DIR`. No flags are
required; the command reads the same `BURROW_*` config as `serve`.

---

### restore

Restore a database backup archive. The archive to restore must be specified as
a positional argument or is prompted interactively.

```sh
burrowd restore <archive-file>
```

::: warning
Restoring replaces the current database. Stop `burrowd serve` before running
`restore` to avoid conflicts.
:::

---

### audit verify

Verify the cryptographic signatures on the audit log. Exits non-zero if any
entry fails verification.

```sh
burrowd audit verify
```

No flags are required; the command connects to the same database as `serve`
using `BURROW_DATABASE_PATH`.

---

### version

Print version, commit hash, build date, and platform.

```sh
burrowd version
```

```
burrowd 0.0.0-dev (commit abc1234, built 2026-06-02, linux/amd64)
```

---

## burrow — local client

```
burrow <command> [flags]
```

### connect

Connect to a Burrow relay and register one or more tunnels. Reconnects
automatically on disconnect. Exits cleanly on `SIGINT` / `SIGTERM`.

**File-config mode (recommended for multiple services)**

```sh
burrow connect --config burrow.yaml
```

**Single-tunnel mode (no config file)**

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token  bur_YOUR_TOKEN_HERE \
  --local  127.0.0.1:3000 \
  --type   http \
  --name   my-app
```

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | — | Path to `burrow.yaml` (multi-service mode; mutually exclusive with single-tunnel flags) |
| `--server` | — | Relay host:port, e.g. `burrow.insingo.com:7000` (required without `--config`) |
| `--token` | — | Auth token starting with `bur_` (required without `--config`) |
| `--local` | `127.0.0.1:3000` | Local address to expose |
| `--remote` | `0` | Requested remote TCP port (`0` = auto-assigned; ignored for HTTP tunnels) |
| `--name` | — | Tunnel name / label (shown in the dashboard) |
| `--type` | `tcp` | Tunnel type: `tcp` or `http` |
| `--insecure` | `false` | Skip TLS verification — **dev only** |
| `--cacert` | — | PEM CA file to trust (e.g. `certs/dev-ca.pem`) |
| `--server-name` | host from `--server` | Override TLS SNI / verify name |

::: warning --insecure is for development only
Skipping TLS verification exposes your token to interception. Never use
`--insecure` against a production relay.
:::

::: tip burrow.yaml for multi-service setups
Put all your services in a `burrow.yaml` config file and use `--config`. The
dashboard's **Connect a client** wizard generates the exact YAML for your
account. See [Connect a client](/guide/connect-client).
:::

**burrow.yaml schema**

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE        # or: token_file: /run/secrets/burrow-token
services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http                    # "tcp" | "http"; default: "tcp"
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001                  # fixed port; 0 = auto-assigned
```

::: details YAML field reference

| Field | Required | Description |
|-------|----------|-------------|
| `server` | Yes | Relay address (`host:port`) |
| `token` | One of `token` or `token_file` | Auth token literal |
| `token_file` | One of `token` or `token_file` | Path to a file containing the token |
| `services[].name` | Yes | Tunnel name (displayed in dashboard) |
| `services[].local` | Yes | Local address to expose, e.g. `127.0.0.1:3000` |
| `services[].type` | No (default `tcp`) | `tcp` or `http` |
| `services[].remote` | No (default `0`) | Fixed TCP port on the relay; `0` = auto. Ignored for HTTP tunnels. |

:::

---

### version

Print version, commit hash, build date, and platform.

```sh
burrow version
```

```
burrow 0.0.0-dev (commit abc1234, built 2026-06-02, linux/amd64)
```

---

## Common patterns

**Expose a local dev server over HTTPS**

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token  bur_YOUR_TOKEN_HERE \
  --local  127.0.0.1:8080 \
  --type   http \
  --name   devserver
```

The tunnel is then reachable at `https://burrow.insingo.com/svc/<slug>/`. The
client prints the URL on `tunnel registered`.

**Forward a fixed TCP port (e.g. SSH)**

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token  bur_YOUR_TOKEN_HERE \
  --local  127.0.0.1:22 \
  --type   tcp \
  --remote 9001 \
  --name   ssh
```

Connect to it with: `ssh -p 9001 user@burrow.insingo.com`

**Run as a systemd service**

Store the token in a file and reference it from `burrow.yaml`:

```yaml
server: burrow.insingo.com:7000
token_file: /etc/burrow/token
services:
  - name: web
    local: 127.0.0.1:3000
    type: http
```

```sh
burrow connect --config /etc/burrow/burrow.yaml
```

::: tip _FILE secrets
`token_file` follows the same `_FILE` convention as server-side env vars —
the file's contents (trimmed) are used as the token value. This integrates
cleanly with Docker Secrets and systemd `LoadCredential`.
:::

---

## Related pages

- [Deploy on a server](/guide/deploy) — install burrowd, ACME setup, firewall ports
- [Connect a client](/guide/connect-client) — install the burrow binary, create burrow.yaml
- [Expose services](/guide/expose-services) — HTTP vs TCP tunnels, apps behind a path
- [Configuration](/guide/configuration) — full `BURROW_*` env var reference
- [HTTP API](/reference/api) — REST endpoints for tokens, services, and access policy
