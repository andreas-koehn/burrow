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

| Command | Purpose |
|---------|---------|
| [`login`](#login) | Sign this machine in, once |
| [`logout`](#logout) | Forget the stored sign-in |
| [`http`](#http-and-tcp) | Expose a local HTTP service |
| [`tcp`](#http-and-tcp) | Expose a local TCP port |
| [`up`](#up) | Run every service in `burrow.yaml` |
| [`status`](#status) | Show what this machine is signed in to |
| [`doctor`](#doctor) | Check relay, sign-in, versions and local targets |
| [`service`](#service) | Run `burrow up` as a system service |
| [`update`](#update) | Replace this binary with the relay's version |
| [`connect`](#connect) | The explicit form with every setting on the command line; unchanged |
| `version` | Print version information |

**Global flags** (every command except where noted):

| Flag | Description |
|------|-------------|
| `--config <file>` | User config file (default: `config.yaml` in the `burrow` directory of the user config directory). For `connect`, `--config` is the path to `burrow.yaml` instead. |
| `--log text\|json` | Print log lines in this format instead of the status view |
| `--cacert <pem>` | PEM CA to trust (e.g. `certs/dev-ca.pem`) |
| `--server-name <name>` | TLS SNI/verify name (default: host of the control endpoint) |
| `--insecure` | Skip TLS verification (**dev only**) |

**Exit codes**

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Any other failure (also: a slug or access mode the relay refused) |
| `2` | Wrong usage or a bad target |
| `3` | Not signed in |
| `4` | The relay rejected the token |
| `5` | Relay unreachable, or its certificate is not trusted |
| `6` | Client too old for this relay |

A handled Ctrl-C during `login`, `doctor` or `update` exits 1 with one line
saying what did or did not happen.

The status view is shown only by `http`, `tcp` and `up`, when stdout is a
terminal, `--log` is not given, `BURROW_LOG_FORMAT` and `BURROW_LOG_LEVEL` are
unset and `TERM` is not `dumb`. `NO_COLOR` is honoured. Otherwise they print log
lines.

---

### login

Sign this machine in, once.

```sh
burrow login <relay>
burrow login <relay> --token -
```

`<relay>` is the dashboard address: `burrow.insingo.com` or a full `https://`
URL. The client asks the relay for its control endpoint and version, prints a
page address and a short code, opens the page in the default browser (unless
`--no-browser`, or stdout is not a terminal), and waits until the sign-in is
approved in the dashboard. The token is created then and stored; you never see
it. Approving needs an admin account or `tokens:manage:own` / `tokens:manage:any`.

| Flag | Default | Description |
|------|---------|-------------|
| `--no-browser` | | Do not open the browser; show the address and the code only |
| `--name <name>` | the hostname | Name for the token |
| `--token -` | | Store a token created in the dashboard (**Clients → Tokens**). Give `-`: the token is asked for (hidden) or read from standard input |
| `--control <host:port>` | the relay's own answer, else `<relay host>:7000` | Control endpoint |
| `--force` | | Replace a stored sign-in without asking |

Without a browser sign-in on the relay, the command stops with exit code 2 and
the `--token -` command. Exit codes: 0 signed in; 1 sign-in denied in the
dashboard, code expired, too many pending sign-ins on the relay, sign-in not
finished, sign-in could not be stored, or interrupted; 2 wrong usage (including
that case); 5 relay unreachable or certificate not trusted; 6 the relay needs a
newer client.

### logout

```sh
burrow logout
```

Deletes the stored sign-in and prints where to revoke the token (**Clients →
Tokens**). It does not revoke the token itself.

### http and tcp

```sh
burrow http <target> [--name <name>] [--slug <slug>] [--access open|login|api-key]
burrow tcp  <target> [--name <name>] [--remote <port>]
```

`<target>` is `3000` (`127.0.0.1:3000`), `localhost:3000`, `192.168.1.20:8080`
or `http://localhost:3000`. A path or an `https://` address is rejected (exit
code 2).

| Flag | Applies to | Description |
|------|-----------|-------------|
| `--name <name>` | both | Service name (default: `<hostname>-<port>`) |
| `--slug <slug>` | `http` | Path under `/svc/`; used only when the service is created |
| `--access open\|login\|api-key` | `http` | Access mode; used only when the service is created. `login` is the relay's `burrow_login`, `api-key` is `api_key` and creates no key |
| `--remote <port>` | `tcp` | Fixed public port (default: any free port) |

The command runs in the foreground until interrupted and reconnects by itself.
Exit codes: 2 bad target or flag; 3 not signed in (or the stored sign-in is for
another relay); 4 token rejected; 6 client too old; 1 anything else, including
a slug the relay refused and an access mode the relay did not apply. When the
relay cannot be reached, the command says so once and keeps retrying.

### up

```sh
burrow up [--file <path>]
```

Runs every service in a `burrow.yaml`, found as `--file <path>`, `./burrow.yaml`,
then `burrow.yaml` in the user config directory. `server` and `token` in the
file are optional; what is missing comes from the sign-in. `BURROW_SERVER` and
`BURROW_TOKEN`, when set, come before the file. `burrow connect --config` uses
the file alone. See [Configuration](/guide/configuration#burrow-yaml).

### status

```sh
burrow status
```

Prints, without connecting a tunnel: the relay and its version, the control
endpoint, the token's name and last four characters, where the sign-in came
from, the client version, and whether the system service is installed and
running. The relay is asked for its version at its web address; the token is not
sent. Exit code 0 when signed in, 3 when not.

### doctor

```sh
burrow doctor
```

Runs the checks listed in [Troubleshooting](/guide/troubleshooting#start-with-burrow-doctor)
and prints one line each. Exit code 0 when nothing failed; otherwise the code of
the first failure (3, 4, 5, 6, or 1).

### service

```sh
burrow service install [burrow.yaml]
burrow service uninstall
burrow service status
burrow service start
burrow service stop
burrow service logs
```

Runs `burrow up` as a system service: a systemd unit on Linux (runs as the user
who ran `install` through `sudo`), a launchd agent on macOS (the current user),
a Windows service running as LocalSystem. `install` needs a `burrow.yaml` and a
stored sign-in, needs root or an administrator on Linux and Windows, and refuses
to install a second copy. `--cacert`, `--server-name` and `--insecure` of
`install` are kept for the service. `service status` exits 0 when the service is
installed and 1 when it is not. See [Keep it running](/guide/connect-client#keep-it-running).

### update

```sh
burrow update [relay] [--check] [--force]
```

Replaces this binary with the client that matches the relay's version. The
relay is the one this machine is signed in to, or the address given. The client
is downloaded through the relay's download address over HTTPS, its SHA-256 is
compared with the release's checksums, it is started once with `version`, and
only then does it take the place of the running binary. The checksum catches a
damaged or incomplete download; it comes from the same place as the client. The
token is not sent. `--check` only reports whether an update exists. `--force`
installs the relay's build when it is untagged and versions cannot be compared.
`--cacert`, `--server-name` and `--insecure` apply to the relay only.

Exit codes: 0 updated or already up to date; 1 not updated (the old binary
stays); 2 used wrongly; 3 no relay known; 5 the relay or the download host
cannot be reached or trusted.

### connect

Connect to a Burrow relay and register one or more tunnels. Reconnects
automatically on disconnect. Exits cleanly on `SIGINT` / `SIGTERM`.

`connect` is unchanged. It does not use the stored sign-in, `BURROW_SERVER` or
`BURROW_TOKEN`, never shows the status view, and a missing server or token is
an error.

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
burrow login burrow.insingo.com
burrow http 8080 --name devserver
```

The same with the explicit form:

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
burrow tcp 22 --remote 9001 --name ssh
```

The same with the explicit form:

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

**Run as a system service**

```sh
sudo "$(command -v burrow)" service install
```

See [Keep it running](/guide/connect-client#keep-it-running). To run the
explicit form under your own systemd unit instead, store the token in a file and
reference it from `burrow.yaml`:

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
