# Quickstart

Get a public HTTPS tunnel running in under five minutes.

---

## Production deploy (recommended)

The fastest path to a permanent relay is one Docker Compose file and one DNS record.

### 1. Point DNS at your server

Create an A record pointing to your VM's public IP:

```
burrow.insingo.com      A   <your-server-ip>
```

::: info
No wildcard record or wildcard certificate is needed. HTTP services are served
at `https://burrow.insingo.com/svc/<slug>/`.
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
    image: ghcr.io/andreas-koehn/burrow:develop
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "7000:7000"
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

On the machine that runs your local app, install the client, sign in once, and
run:

**Linux and macOS**

```sh
curl -fsSL https://burrow.insingo.com/install.sh | sh
burrow login burrow.insingo.com
burrow http 3000
```

**Windows (PowerShell)**

```powershell
irm https://burrow.insingo.com/install.ps1 | iex
burrow login burrow.insingo.com
burrow http 3000
```

`burrow login` prints a page address and a short code. Open the page (it opens
by itself on a desktop), check that it shows the same code, and approve. No
token is copied by hand.

```
Open this page to sign this machine in:

  https://burrow.insingo.com/link?code=BRRW-7Q4K

Check that the page shows the code BRRW-7Q4K.
Waiting for approval…  signed in as admin@insingo.com (token "kohns-laptop")
```

`burrow http 3000` then shows the status view with the address of your app:

```
burrow  ●  connected to burrow.insingo.com     v0.7.0   12 ms

  kohns-laptop-3000   https://burrow.insingo.com/svc/p7baeh/  →  127.0.0.1:3000
                      access: open (anyone with the URL)       3 open, 41 total
```

Your local app is now reachable at `https://burrow.insingo.com/svc/<slug>/`; the
dashboard shows the address too.

::: warning The service is open by default
A service created this way is open: anyone who has the URL can use it. Add
`--access login` or `--access api-key` to the first `burrow http` command, or
change the access mode in the dashboard. See
[Expose services](/guide/expose-services#access).

An open `/svc/` app shares the dashboard's origin, and its scripts can call the dashboard API as the signed-in user, so expose only apps you trust (see [Apps behind a path](/guide/expose-services#apps-behind-a-path)).
:::

Other ways to connect (a manual download, a token instead of the browser,
`burrow.yaml`, a system service) are in [Connect a client](/guide/connect-client).

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
[develop release](https://github.com/andreas-koehn/burrow/releases/tag/develop) or
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
- [Connect a client](/guide/connect-client) — other ways to sign in, `burrow.yaml`, running as a service, updating
