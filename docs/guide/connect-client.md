# Connect a client

Get the `burrow` client binary, create a config file, and connect to your relay.

---

## 1. Install the `burrow` binary

### Download a pre-built binary

Rolling binaries are published to the `develop` release channel:

**Linux (amd64)**

```sh
curl -L https://github.com/ankoehn/burrow/releases/download/develop/burrow_linux_amd64.tar.gz \
  | tar xz
chmod +x burrow
sudo mv burrow /usr/local/bin/
```

**Windows (amd64)**

Download and extract:

```
https://github.com/ankoehn/burrow/releases/tag/develop
```

Pick `burrow_windows_amd64.zip`, extract it, and place `burrow.exe` on your `PATH`.

::: info No stable release yet
There is no `:latest` or version-tagged release yet. The `develop` channel is the
correct installation method. A `checksums.txt` file is included in every release
for verification.
:::

### Build from source

```sh
git clone https://github.com/ankoehn/burrow.git
cd burrow
go build ./cmd/client
# produces ./burrow (or burrow.exe on Windows)
```

---

## 2. Mint a token

The client authenticates with a bearer token. Mint one using either the
dashboard or the server CLI.

### Option A — Dashboard

1. Log in to `https://burrow.insingo.com`.
2. Open **Tokens** in the left navigation.
3. Click **New token**, give it a name (e.g. `laptop`), and copy the value.

::: tip Token starts with `bur_`
The token is shown once. Copy it now — it cannot be retrieved afterwards.
:::

### Option B — Server CLI

On the host running the relay container:

```sh
docker compose exec burrowd burrowd token \
  --email you@insingo.com \
  --name laptop
```

This writes directly to the database and prints the token.

---

## 3. Write `burrow.yaml`

Create a `burrow.yaml` file on your local machine:

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE

services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http
```

### Config key reference

| Key | Required | Notes |
|-----|----------|-------|
| `server` | Yes | Relay control address (`host:7000`) |
| `token` | Yes (or `token_file`) | Bearer token from the dashboard or CLI |
| `token_file` | Yes (or `token`) | Path to a file containing the token (Docker Secrets / Kubernetes Secrets) |
| `services[].name` | Yes | Label shown in the dashboard |
| `services[].local` | Yes | Local address to forward traffic to (e.g. `127.0.0.1:3000`) |
| `services[].type` | No | `http` or `tcp`; defaults to `tcp` |
| `services[].remote` | No | Fixed public TCP port; TCP tunnels only; `0` = auto-assigned |

::: details Multi-service example

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE

services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http

  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001   # request a stable port across reconnects
```

:::

---

## 4. Connect

```sh
burrow connect --config burrow.yaml
```

Once connected, each `http` service is reachable at:

```
https://burrow.insingo.com/svc/<slug>/
```

The client prints the full URL on `tunnel registered`.

::: tip TLS is validated automatically
The client connects to port `7000` using TLS. When the relay uses
Let's Encrypt (ACME), the certificate is signed by a public CA and validated by
the client automatically — no `--insecure` flag or extra CA bundle needed.
:::

### Single-tunnel shortcut (no config file)

For a quick one-off tunnel without writing a file:

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token bur_YOUR_TOKEN_HERE \
  --local 127.0.0.1:3000 \
  --type http \
  --name my-app
```

### All flags

| Flag | Default | Notes |
|------|---------|-------|
| `--config` | | Path to `burrow.yaml` |
| `--server` | | Relay address (`host:port`) |
| `--token` | | Bearer token |
| `--local` | `127.0.0.1:3000` | Local address to tunnel |
| `--type` | `tcp` | `http` or `tcp` |
| `--name` | | Tunnel label |
| `--remote` | `0` | Requested TCP port (TCP type only) |
| `--insecure` | | Skip TLS verification (dev only) |
| `--cacert` | | Path to a custom CA PEM bundle |
| `--server-name` | | Override TLS server name |

::: warning `--insecure` in production
Only use `--insecure` against a local dev relay with self-signed certificates.
Never use it against a production relay.
:::

---

## Next steps

- [Expose services](/guide/expose-services) — HTTP vs TCP tunnels, stable TCP ports, apps behind a path.
- [Access control & security](/guide/access-control) — Lock a service behind an API key, Burrow login, or mTLS.
- [CLI reference](/reference/cli) — Full `burrow` command tree.
