# Deploy on a server

This guide walks through a production-ready deployment of Burrow on a Linux
server using Docker Compose and built-in ACME (Let's Encrypt). After following
it you will have a running relay at `https://burrow.insingo.com` with automatic
TLS and no extra reverse proxy required.

---

## 1. What you will build

One container. One domain. Automatic HTTPS.

- The relay server (`burrowd`) listens on port `443` for the dashboard and REST
  API, port `7000` for client connections, and port `80` solely for ACME
  HTTP-01 challenges and HTTPS redirects.
- Every HTTP tunnel is reachable at two equivalent URLs:
  - **Subdomain**: `https://<id>.burrow.insingo.com/`
  - **Path** (same origin, no extra DNS): `https://burrow.insingo.com/t/<id>/`
- Raw TCP tunnels get a dedicated port in the `9000–9100` range (open only the
  ports you actually use).

**Topology**

```
              Internet
                 │
         ┌───────▼────────┐
         │   Your server  │
         │                │
         │  :80   ACME / redirect
         │  :443  Dashboard + API
         │  :7000 Client control channel
         │  :9001 TCP tunnel (optional)
         │                │
         └───────┬────────┘
                 │ yamux TLS
          ┌──────▼──────┐
          │ burrow CLI  │  (developer laptop)
          └─────────────┘
```

::: info Path routing and mTLS
The `/t/<id>/` path route and subdomain route reach the same upstream. The
only feature not available on the path route is mTLS — mutual TLS requires a
dedicated TLS handshake on the `:8443` ingress listener (subdomain route only).
:::

---

## 2. Prerequisites

- A Linux server with a public IP address and Docker installed.
- Firewall rules that allow inbound traffic on:

  | Port | Required for |
  |------|-------------|
  | `80` | ACME HTTP-01 challenge + HTTPS redirect |
  | `443` | Dashboard, API, HTTP tunnels |
  | `7000` | Client control channel |
  | `9001` | Raw TCP tunnels (open only if you use them) |

- A domain name you control (e.g. `burrow.insingo.com`).

---

## 3. DNS

Create a single A record pointing to your server's IP. No wildcard is needed
for path-routed HTTP tunnels; a wildcard is only required if you want subdomain
routing without the `/t/<id>/` path.

```
burrow.insingo.com.  300  IN  A  SERVER_IP
```

Verify propagation before starting the server:

```sh
dig +short burrow.insingo.com
# should print SERVER_IP
```

::: warning DNS must resolve before first boot
`burrowd` calls `certmagic.ManageSync` at startup, which blocks until the
ACME challenge succeeds. If your A record is not yet reachable or port `80` is
firewalled, startup will fail.
:::

---

## 4. Get the image

The container image is published to GitHub Container Registry:

```
ghcr.io/ankoehn/burrow:develop
```

The image is multi-arch (`linux/amd64` + `linux/arm64`) and is rebuilt on
every push to the `develop` branch.

::: warning Package visibility
The GHCR package is **private by default**. Before your server can pull it you
must either:

1. Make it public at
   [github.com/users/ankoehn/packages/container/burrow/settings](https://github.com/users/ankoehn/packages/container/burrow/settings),
   **or**
2. Run `docker login ghcr.io` on the server with a GitHub personal access token
   that has `read:packages` scope.
:::

Pull the image to confirm access:

```sh
docker pull ghcr.io/ankoehn/burrow:develop
```

---

## 5. docker-compose.yml

Create a working directory and write the compose file:

```sh
mkdir -p /opt/burrow && cd /opt/burrow
```

**docker-compose.yml**

```yaml
services:
  burrow:
    image: ghcr.io/ankoehn/burrow:develop
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "7000:7000"
      # Uncomment if you use raw TCP tunnels:
      # - "9001:9001"
    environment:
      BURROW_ACME_DOMAIN: burrow.insingo.com
      BURROW_ACME_EMAIL: ops@insingo.com
      BURROW_ACME_STORAGE: /data/acme
      BURROW_ADMIN_EMAIL: admin@insingo.com
      BURROW_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
      BURROW_DATABASE_PATH: /data/burrow.db
    secrets:
      - admin_password
    volumes:
      - burrow_data:/data

secrets:
  admin_password:
    file: ./secrets/admin_password.txt

volumes:
  burrow_data:
```

Create the admin password secret:

```sh
mkdir -p secrets
echo -n 'ChangeMe123!' > secrets/admin_password.txt
chmod 600 secrets/admin_password.txt
```

::: tip _FILE convention
Any `BURROW_<KEY>` environment variable also accepts a `BURROW_<KEY>_FILE`
form that reads the value from a file. This is the recommended pattern for
secrets. See [Configuration](/guide/configuration) for the full list.
:::

### Equivalent `docker run`

If you prefer a single command without Compose:

```sh
docker run -d \
  --name burrow \
  --restart unless-stopped \
  -p 80:80 -p 443:443 -p 7000:7000 \
  -e BURROW_ACME_DOMAIN=burrow.insingo.com \
  -e BURROW_ACME_EMAIL=ops@insingo.com \
  -e BURROW_ACME_STORAGE=/data/acme \
  -e BURROW_ADMIN_EMAIL=admin@insingo.com \
  -e BURROW_ADMIN_PASSWORD=ChangeMe123! \
  -e BURROW_DATABASE_PATH=/data/burrow.db \
  -v burrow_data:/data \
  ghcr.io/ankoehn/burrow:develop
```

::: warning Production secrets
Pass `BURROW_ADMIN_PASSWORD_FILE` (not the plain `BURROW_ADMIN_PASSWORD`) in
production. The plain variable ends up in `docker inspect` output and shell
history.
:::

---

## 6. First boot

Start the server:

```sh
docker compose up -d
```

Watch the logs while Let's Encrypt issues the certificate. This typically takes
5–15 seconds:

```sh
docker compose logs -f
```

Look for lines like:

```
INFO  acme: obtaining certificate domain=burrow.insingo.com
INFO  acme: certificate obtained successfully
INFO  server: listening addr=:443
INFO  server: listening addr=:7000
```

Once the certificate is issued, open the dashboard in your browser:

```
https://burrow.insingo.com
```

Log in with the `BURROW_ADMIN_EMAIL` and password you set. The first thing to
do is change the default password under **Account → Change password**.

::: tip Admin seed is first-boot only
`BURROW_ADMIN_EMAIL` and `BURROW_ADMIN_PASSWORD_FILE` seed the admin account
only when the database is empty. They are ignored on subsequent starts if the
account already exists. You can safely remove them from the compose file after
the first boot.
:::

---

## 7. Testing without hitting Let's Encrypt rate limits

Before deploying to production, use the Let's Encrypt **staging** CA to verify
your setup without consuming production certificate quota:

```yaml
environment:
  BURROW_ACME_DOMAIN: burrow.insingo.com
  BURROW_ACME_EMAIL: ops@insingo.com
  BURROW_ACME_CA: https://acme-staging-v02.api.letsencrypt.org/directory
```

The staging CA issues certificates that are not trusted by browsers but are
otherwise functionally identical. Your server will start and you can confirm
the ACME flow works before switching back to production.

::: warning Remove staging CA before going live
Delete or comment out `BURROW_ACME_CA` when you are ready for production. The
default is the Let's Encrypt production directory.
:::

---

## Next steps

- [Connect a client](/guide/connect-client) — install the `burrow` CLI and
  expose your first service.
- [Expose services](/guide/expose-services) — HTTP vs TCP tunnels, subdomain
  vs path routing, stable TCP ports.
- [Access control & security](/guide/access-control) — lock down tunnels with
  API keys, `burrow_login`, or mTLS.
- [Configuration](/guide/configuration) — full `BURROW_*` environment variable
  reference.
- [Operations](/guide/operations) — backups, log levels, upgrading.
