# Quickstart

Get a public HTTPS tunnel running in under five minutes.

---

## Production deploy (recommended)

The fastest path to a permanent relay is one Docker Compose file and two DNS records.

### 1. Point DNS at your server

Create an A record (and, optionally, a wildcard) pointing to your VM's public IP:

```
burrow.insingo.com      A   <your-server-ip>
*.burrow.insingo.com    A   <your-server-ip>
```

::: info
The wildcard record is needed for subdomain-routed HTTP tunnels
(`https://<id>.burrow.insingo.com/`). If you only need path-routed access
(`https://burrow.insingo.com/t/<id>/`), the wildcard is optional.
:::

### 2. Open firewall ports

| Port | Purpose |
|------|---------|
| `80` | ACME HTTP-01 challenge + HTTPS redirect |
| `443` | Dashboard, API, and HTTPS tunnels |
| `7000` | Client control channel (TLS) |
| `9001` | Example raw TCP tunnel (open only if needed) |

### 3. Create `compose.yaml`

```yaml
services:
  burrowd:
    image: ghcr.io/ankoehn/burrow:develop
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "7000:7000"
      - "8443:8443"
    volumes:
      - burrow_data:/data
    environment:
      BURROW_DATABASE_PATH: /data/burrow.db
      BURROW_ACME_DOMAIN: burrow.insingo.com
      BURROW_ACME_EMAIL: admin@insingo.com
      BURROW_ADMIN_EMAIL: admin@insingo.com
      BURROW_ADMIN_PASSWORD: changeme   # change this before going live
      BURROW_HTTP_SECURE_COOKIES: "true"

volumes:
  burrow_data:
```

### 4. Start the relay

```sh
docker compose up -d
```

Burrow contacts Let's Encrypt, obtains a certificate for `burrow.insingo.com`,
and starts all listeners. Open `https://burrow.insingo.com` — you should see the
dashboard login.

::: warning Change the default password
`BURROW_ADMIN_PASSWORD` is only seeded on first boot. Log in, go to Account
settings, and change it immediately.
:::

### 5. Connect a client

Mint a token in the dashboard (Users → your user → Tokens → New token), then
run the client on your local machine:

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token bur_YOUR_TOKEN_HERE \
  --local 127.0.0.1:3000 \
  --type http \
  --name my-app
```

Your local app is now reachable at:

- `https://burrow.insingo.com/t/<id>/` — path-routed (same origin as the dashboard)
- `https://<id>.burrow.insingo.com/` — subdomain-routed (requires the `*` DNS record)

Both URLs reach the same upstream.

See [Deploy on a server](/guide/deploy) for the full production setup, including
binary installs, file-based TLS, firewall hardening, and backup configuration.

---

## Try it locally

No domain, no DNS — use self-signed certs to explore Burrow on your own machine.

```sh
burrowd serve --dev-certs
```

This generates self-signed certificates in `./certs/` and starts all listeners.
The dashboard is at `https://localhost:8080` (accept the browser TLS warning).

::: tip Getting the binary
Download a pre-built binary from the
[develop release](https://github.com/ankoehn/burrow/releases/tag/develop) or
build from source:

```sh
go build ./cmd/server   # produces burrowd
go build ./cmd/client   # produces burrow
```
:::

::: details First-boot admin credentials (local)
Set these before running `burrowd serve`:

```env
BURROW_ADMIN_EMAIL=admin@example.com
BURROW_ADMIN_PASSWORD=changeme
```

Or pass them inline:

```sh
BURROW_ADMIN_EMAIL=admin@example.com \
BURROW_ADMIN_PASSWORD=changeme \
burrowd serve --dev-certs
```
:::

Connect a client against the local relay:

```sh
burrow connect \
  --server localhost:7000 \
  --token bur_YOUR_TOKEN_HERE \
  --local 127.0.0.1:3000 \
  --type http \
  --name my-app \
  --insecure
```

`--insecure` skips TLS verification for the self-signed control-channel cert.

---

## Next steps

- [Deploy on a server](/guide/deploy) — full production setup with all options
- [Connect a client](/guide/connect-client) — `burrow.yaml` multi-service config, flags reference
