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
- Every HTTP tunnel is reachable at `https://burrow.insingo.com/svc/<slug>/`
  (same origin as the dashboard, no extra DNS).
- Raw TCP tunnels in mode `open` or `ip` get a dedicated port in the `9000–9100` range; `private` ones get none (open only the
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

::: info Path URLs and mTLS
The only features not available on `/svc/<slug>/` URLs are mTLS and custom
domains (and so gRPC, which needs a host name; see [gRPC](/guide/grpc)). Mutual TLS requires a dedicated TLS handshake on the opt-in
host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`).
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
  | `8444` | TLS passthrough by host name (only with `BURROW_TLS_PASSTHROUGH_LISTEN`, see [TCP by host name](#tcp-by-host-name)). Not needed with `BURROW_TLS_PASSTHROUGH_ON_HTTPS`, which shares `443` ([One port 443](#one-port-443)) |

- A domain name you control (e.g. `burrow.insingo.com`).

---

## 3. DNS

Create a single A record pointing to your server's IP. No wildcard record or
wildcard certificate is needed; HTTP tunnels are served under
`/svc/<slug>/`.

```
burrow.insingo.com.  300  IN  A  SERVER_IP
```

Recommended: a second name for the tunnelled apps, so they do not share the
dashboard's origin. Add its A record and set `BURROW_SERVICES_DOMAIN`; ACME
obtains its certificate too, and services move to
`https://apps.insingo.com/svc/<slug>/`. See
[Services domain](/guide/configuration#services-domain). Set it before you
rely on OAuth for MCP clients: until then a tunnelled app shares the origin of
the consent page, and only your password protects an approval.

```
apps.insingo.com.    300  IN  A  SERVER_IP
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
ghcr.io/andreas-koehn/burrow:develop
```

The image is multi-arch (`linux/amd64` + `linux/arm64`) and is rebuilt on
every push to the `develop` branch.

::: warning Package visibility
The GHCR package is **private by default**. Before your server can pull it you
must either:

1. Make it public at
   [github.com/users/andreas-koehn/packages/container/burrow/settings](https://github.com/users/andreas-koehn/packages/container/burrow/settings),
   **or**
2. Run `docker login ghcr.io` on the server with a GitHub personal access token
   that has `read:packages` scope.
:::

Pull the image to confirm access:

```sh
docker pull ghcr.io/andreas-koehn/burrow:develop
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
    image: ghcr.io/andreas-koehn/burrow:develop
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "7000:7000"
      # Uncomment if you use open or ip TCP tunnels (private ones need no port):
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
  ghcr.io/andreas-koehn/burrow:develop
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

## TCP by host name (optional) {#tcp-by-host-name}

To reach tcp services by a host name on a shared TLS port, set
`BURROW_TLS_PASSTHROUGH_LISTEN=:8444` and publish the port:

```yaml
    ports:
      - "8444:8444"
    environment:
      BURROW_TLS_PASSTHROUGH_LISTEN: ":8444"
      BURROW_TCP_DOMAIN: tcp.example.com   # optional; needs *.tcp.example.com in DNS
```

The relay reads the SNI of the first TLS record and does not terminate TLS.
Only `open` and `ip` services are reachable this way. Open the port in the
firewall. Do not set `BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES` unless a proxy
in front sends a PROXY v2 header.

**Behind Coolify (Traefik).** Traefik owns port 443. Give the Burrow service a
TCP router with `HostSNI(...)`, `tls.passthrough=true` and a `proxyProtocol`
version 2 to port 8444 instead of publishing the port, and set
`BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES` to Traefik's address. The labels
are in
[Behind a proxy](/guide/remote-desktop-and-tcp#behind-a-proxy-coolify-and-traefik).
Names that no TCP router matches still reach Coolify's HTTP routers.

## HTTP/2 and gRPC {#http2-and-grpc}

Visitors can speak HTTP/2 to the relay, which is what gRPC needs. Nothing to
switch on:

- **Over TLS** the dashboard listener and the host-routed ingress offer `h2`
  and `http/1.1` by ALPN. A browser or a gRPC client picks `h2`. This holds for
  ACME certificates and file certificates alike.
- **Without TLS** (a proxy such as Traefik terminates TLS in front) both
  listeners accept prior-knowledge HTTP/2, **h2c**, next to HTTP/1.1.
  `BURROW_HTTP_H2C` controls it and defaults to `true`. Set it to `false` to
  accept HTTP/1.1 only on the plain listeners; a client that then speaks h2c
  gets a connection error.

The `Upgrade: h2c` flavour of h2c stays refused with 505 whatever you set. A
connection with no active stream is closed after 120 seconds, and a dead peer
is found by an HTTP/2 ping.

::: warning h2c and the proxy in front
A plain listener that takes h2c trusts the network path to it, as it always
did for HTTP/1.1: anything that can reach the port can talk to it. Do not
publish the plain port (`8080`) beyond the proxy, and keep
`BURROW_TRUSTED_PROXIES` to the proxy's address. h2c adds no new access, but
if you do not run gRPC behind a proxy, `BURROW_HTTP_H2C=false` keeps the
listener to what it spoke before.
:::

The same relay speaks to the **local service** by the service's upstream
protocol (`http1`, `https`, `h2c` or `h2`). That is a per-service setting; see
[gRPC](/guide/grpc). A gRPC client cannot use the `/svc/<slug>/` prefix, so a
gRPC service needs a host name of its own, a custom domain.

### Coolify and Traefik: gRPC {#traefik-grpc}

Traefik terminates TLS. The dashboard keeps the router Coolify generated, to
port `8080`. Each gRPC host name gets a router of its own to the same port,
with the service scheme `h2c`, which is why the plain listener takes h2c:

```yaml
    labels:
      - traefik.enable=true
      - traefik.http.routers.burrow-grpc.rule=Host(`grpc.example.com`)
      - traefik.http.routers.burrow-grpc.entrypoints=https
      - traefik.http.routers.burrow-grpc.tls=true
      - traefik.http.routers.burrow-grpc.tls.certresolver=letsencrypt
      - traefik.http.routers.burrow-grpc.service=burrow-grpc
      - traefik.http.services.burrow-grpc.loadbalancer.server.port=8080
      - traefik.http.services.burrow-grpc.loadbalancer.server.scheme=h2c
```

Then, in Burrow:

1. Add `grpc.example.com` as a custom domain of the http service with
   `cert_source: auto`. Behind a proxy on a plain listener the relay needs no
   certificate for it (`cert_status` is `not_needed`); Traefik holds it.
   `auto` takes an administrator (`services:configure:any`); a service owner
   without it uploads a certificate instead, or asks an administrator. The
   Host names card of the service does this in the dashboard.
2. Set the service's upstream protocol to `h2c` (or start the client with
   `burrow grpc 50051`).
3. Keep `BURROW_AUTH_DOMAIN` set, as Coolify does. The relay sends a request
   to a custom domain's service by its `Host` header only when it knows a name
   of its own (an auth domain, a certificate or ACME); without one the
   dispatch stays off.

`BURROW_TRUSTED_PROXIES` already covers Traefik in the Coolify compose file.

::: warning Traefik's `readTimeout` can cut long gRPC streams
Traefik v3 entrypoints have `transport.respondingTimeouts.readTimeout`, which
bounds how long Traefik reads a request, a long client stream included. A
streaming call that sends for longer than the value is reset. Check the
default of your Traefik version and raise it, or set it to `0`, on the entry
point that carries gRPC:

```
--entryPoints.https.transport.respondingTimeouts.readTimeout=0
```

`idleTimeout` and `writeTimeout` are separate settings; check them too for
streams that stay quiet for long.
:::

### Where to set the deadline

The relay adds nothing to a gRPC call without a deadline. To bound calls on
the relay, set `grpc_max_duration_s` on the service (see
[gRPC](/guide/grpc#deadlines)). A proxy in front may cut a call earlier; the
caller then sees `UNAVAILABLE` from the proxy, not from Burrow.

---

## One port 443 {#one-port-443}

By default TLS passthrough by host name needs a port of its own
(`BURROW_TLS_PASSTHROUGH_LISTEN`, for example `:8444`). With
`BURROW_TLS_PASSTHROUGH_ON_HTTPS=true` the relay shares the HTTPS port: the
dashboard, `/svc/`, custom-domain http and gRPC services and the host names of
tcp services all answer on `:443`. The default is `false`, because turning it
on widens how tcp services can be reached.

```yaml
    ports:
      - "443:443"
    environment:
      BURROW_ACME_DOMAIN: burrow.insingo.com
      BURROW_TLS_PASSTHROUGH_ON_HTTPS: "true"
      BURROW_TCP_DOMAIN: tcp.example.com   # optional; *.tcp.example.com in DNS
```

A tcp service with passthrough needs DNS only. Burrow holds no certificate for
it, the service serves its own.

### Routing order

The relay reads the first TLS record of each connection (the ClientHello)
without terminating TLS, and decides in this order:

1. The hello offers ALPN `acme-tls/1`, or has no server name: terminate. ACME
   keeps working.
2. The name is one of the relay's own (the dashboard's host and every name
   under the auth domain, the services domain, the ACME names, the DNS names
   of `BURROW_HTTP_TLS_CERT`, `localhost`, IP addresses): terminate.
3. A custom domain covers the name: terminate.
4. The name belongs to a live `open` or `ip` tcp service (its slug name under
   `BURROW_TCP_DOMAIN`, or its own host name): hand the connection to the
   service raw, through the same accept checks as its port (mode, country
   rules, bans, limits). The connection log row has protocol `tls` and the
   SNI.
5. Anything else: terminate.

A hello that cannot be read is **not refused**. No SNI, an unknown name, a
non-TLS first byte, a hello split over several TLS records or a malformed one
is handed to the TLS terminator with the bytes replayed, so an HTTPS client
works whatever it sends. Only a hello that does not arrive in time, a budget
or a PROXY header violation closes a connection. A private tcp service is
never reached by name.

### Behind a proxy that sends PROXY v2

`BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES` works as on the dedicated listener,
and now applies to `:443` as well. From a listed address every connection must
begin with a PROXY protocol v2 header that names the visitor, **HTTPS and
health checks included**; one without it is closed. From any other address a
header is refused. The visitor's address from the header then applies to
terminated connections too, so the dashboard and http services see the
visitor, not the proxy. The list is never taken from `BURROW_TRUSTED_PROXIES`.

### Budgets and fall-through

Connections that wait for their hello are bounded, separately from the
dedicated listener: 4096 in total, 64 per untrusted source (an IPv4 address,
or an IPv6 /48) and 1024 for trusted proxies. The wait is 5 seconds, or 2
seconds while a budget is more than 75 % full. Over the per-source cap or the
trusted budget a connection is closed with no byte and counted `busy` under
`https-mux`. When the **total** budget is full, the connection is not closed:
it goes straight to the TLS terminator without being read, so a flood of slow
hellos cannot lock HTTPS out. Such a connection is terminated, never passed
through; passthrough by name degrades under a flood, and HTTPS does not.
The dedicated listener still closes such a connection.

### With the dedicated listener

Both can run together. `BURROW_TLS_PASSTHROUGH_LISTEN` stays useful for a
second port, or for a proxy-only path. A tcp service is then reachable on
`:443` and on the dedicated port, and `hostname_only` holds with either. The
dedicated listener does not look at custom domains: a name a custom domain
covers (see below) is terminated on `:443` and still reachable on the
dedicated port. `GET /api/v1/services/{id}/tcp-hostname` reports where a name
answers (`reachable_on`) and what shadows it (`shadowed_by`).

### One owner per host name

A name belongs to either an http service or a tcp service, never both:

- `tcp_hostname` is refused (409) when a custom domain covers it, or when it
  is, or lies under, an ACME name or another name the relay serves itself.
- A custom domain is refused (409, `hostname is a tcp host name`) when it
  equals a `tcp_hostname`, covers one, or is, lies under or above
  `BURROW_TCP_DOMAIN`. It is also refused when it is a relay-own name or lies
  under one.
- Rows that exist from before these checks are resolved at run time in favour
  of the http side: the relay terminates, never hands raw bytes to the wrong
  owner, and logs a warning once per name.
- A custom domain above a tcp host name (an apex `example.com` with
  `rdp.example.com`) is refused as well: the mux would terminate that name.
- The check runs inside the relay process. One relay per database is the
  supported setup.

### Configuration refusals

The relay does not start when:

- `BURROW_TLS_PASSTHROUGH_ON_HTTPS=true` and the dashboard listener does not
  terminate TLS itself. It needs `BURROW_ACME_DOMAIN`, or
  `BURROW_HTTP_TLS_CERT` with `BURROW_HTTP_TLS_KEY`. Behind a proxy that
  terminates TLS, use `BURROW_TLS_PASSTHROUGH_LISTEN` instead.
- `BURROW_TCP_DOMAIN` is the dashboard's host, the services host, an ACME name
  or a parent of one.
- `BURROW_TCP_DOMAIN` is under the dashboard's host while the mux is on: the
  mux terminates every name under it, so no slug name would reach its
  service.
- The mux is on and a slug name under `BURROW_TCP_DOMAIN` is one of the relay's
  own names, for example because `BURROW_HTTP_TLS_CERT` carries a wildcard
  that covers it. A wildcard covers one label, so `*.example.com` and
  `tcp.example.com` is fine.
- `BURROW_HTTP_TLS_CERT` cannot be read.

### Behind Coolify and Traefik

Traefik owns `443` there, so Traefik is the mux and `BURROW_TLS_PASSTHROUGH_ON_HTTPS`
stays off. HTTP and gRPC names are routed as in
[Coolify and Traefik: gRPC](#traefik-grpc). Tcp host names use a TCP router
with `HostSNI`, `tls.passthrough=true` and PROXY v2 to the dedicated listener,
unchanged from before:

```yaml
    environment:
      BURROW_TLS_PASSTHROUGH_LISTEN: ":8444"
      BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES: 10.0.1.5   # Traefik, nothing wider
    labels:
      - traefik.tcp.routers.burrow-sni.entrypoints=https
      - traefik.tcp.routers.burrow-sni.rule=HostSNI(`db.example.com`)
      - traefik.tcp.routers.burrow-sni.tls.passthrough=true
      - traefik.tcp.routers.burrow-sni.service=burrow-sni
      - traefik.tcp.services.burrow-sni.loadbalancer.server.port=8444
      - traefik.tcp.services.burrow-sni.loadbalancer.proxyProtocol.version=2
```

The details, and the newer `serversTransport` form of the PROXY setting, are
in
[Behind a proxy](/guide/remote-desktop-and-tcp#behind-a-proxy-coolify-and-traefik).

**Alternative, advanced: Traefik passes all Burrow names through.** A TCP
router with `HostSNIRegexp` for every Burrow name, `tls.passthrough=true` and
PROXY v2 goes to Burrow's TLS listener, with ACME and the mux on. Burrow then
terminates everything itself: h2, its own certificates by TLS-ALPN-01 (the
`acme-tls/1` protocol passes through) and mTLS. HTTP-01 reaches Burrow only if
Traefik also routes `/.well-known/acme-challenge/` for those names. List
Traefik in `BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES`, and note that Coolify's
Domains tab must not claim the same names.

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

## 8. Client downloads and the first tagged release {#client-release}

Your relay serves the client installers (`/install.sh`, `/install.ps1`) and
redirects `/download/burrow/<os>/<arch>` and `/download/burrow/checksums.txt` to
release files. `burrow update` uses the same addresses. All of them depend on
the files of a **version-tagged release** being there under the right names.

::: warning No tagged release exists yet
Until one does, a relay built from an untagged commit redirects to the rolling
`develop` pre-release, and the installer prints
`This relay runs an untagged build: installing the rolling develop build of burrow.`
The install one-liner works that way, but it hands out whatever `develop`
currently holds. Cut a tagged release before you rely on it.
:::

### What a tagged release must contain

The relay computes the file names from its own version. For a relay at version
`0.7.0` (tag `v0.7.0`) it redirects to
`<client_download_base>/v0.7.0/<archive>`. The release must hold at least these
files, as CI (`.goreleaser.yml`, on `v*` tags) produces them:

| Platform | Archive |
|----------|---------|
| Linux amd64 | `burrow_linux_amd64_0.7.0.tar.gz` |
| Linux arm64 | `burrow_linux_arm64_0.7.0.tar.gz` |
| Linux arm (32-bit) | `burrow_linux_armv7_0.7.0.tar.gz` |
| Linux 386 | `burrow_linux_386_0.7.0.tar.gz` |
| macOS amd64 | `burrow_darwin_amd64_0.7.0.tar.gz` |
| macOS arm64 | `burrow_darwin_arm64_0.7.0.tar.gz` |
| Windows amd64 | `burrow_windows_amd64_0.7.0.zip` |
| Windows 386 | `burrow_windows_386_0.7.0.zip` |
| all of the above | `checksums.txt`, listing the SHA-256 of every archive |

That is eight platform pairs. The pattern is
`burrow_<os>_<arch>_<version>.tar.gz` (`.zip` on Windows), with the version
without the leading `v` and `arm` spelled `armv7`. Each archive holds the
`burrow` (or `burrow.exe`) binary at its root; other files, such as a LICENSE or
README, do no harm, and the release also holds the `burrowd_*` server archives
and signatures. A file under another name, a missing
pair or a missing `checksums.txt` breaks the installer and `burrow update` on
that platform: the relay does not check that the target exists.

The rolling `develop` pre-release has the shorter names
`burrow_<os>_<arch>.tar.gz` / `.zip` for four pairs (`linux/amd64`,
`linux/arm64`, `darwin/arm64`, `windows/amd64`) and its own `checksums.txt`.

Release checklist, for whoever cuts the tag (nothing in the repository tags or
publishes by itself):

1. Before tagging, run the two test suites that are **not** part of CI, on a
   machine with Docker (the second also needs `jq`):
   `bash test/installer/run.sh` (the installer matrix) and
   `bash test/integration/client-cli.sh` (install, sign in and expose a service
   against a relay of this commit, plus an old client against it). Both must
   end without a failure.
2. Tag `v<version>` and let CI publish. Confirm the release lists all eight
   archives and `checksums.txt` under the names above.
3. Deploy a relay built from that tag (its version must be exactly
   `<version>`; builds with a suffix such as `-rc1` or a dirty tree hand out
   `develop`).
4. From a clean machine, run the installer and then `burrow login` and
   `burrow http 3000` against that relay.
5. Check the relay settings (see below) if you changed them.
6. Before the first tag, point the image name in `.goreleaser.yml`
   (`dockers_v2`) at the current account, `ghcr.io/andreas-koehn/burrow`: it still
   names the old owner, `ghcr.io/ankoehn/burrow`. The `:latest` and versioned image tags exist
   only after that release.
7. Run the platform checks in the next list. They need real machines and have
   **not** been run on Windows, macOS or under real systemd.

### Relay settings involved

| Setting | Environment variable | Purpose |
|---------|----------------------|---------|
| `client_download_base` | `BURROW_CLIENT_DOWNLOAD_BASE` | Where the redirects point; default `https://github.com/andreas-koehn/burrow/releases/download`. A fork must set its own, or its relay hands out the upstream project's client. |
| `client_download_dir` | `BURROW_CLIENT_DOWNLOAD_DIR` | Serve the files from a directory instead (below). |
| `min_client_version` | `BURROW_MIN_CLIENT_VERSION` | Oldest client the relay accepts. Set it only to a version that exists as a release, or clients are told to `burrow update` to something they cannot get. |

See [Configuration](/guide/configuration#client-downloads).

### Checks that only a person on a real machine can do

Group them by platform. Each describes what to observe.

**Windows**

- `irm https://<relay>/install.ps1 | iex` on Windows PowerShell 5.1 and on
  PowerShell 7: one entry is added to the user `Path`, and a new terminal finds
  `burrow`; an existing `Path` that holds `%VARIABLES%` stays unexpanded; a
  missing `Path` is created; a second run adds no duplicate; a long `Path` is
  not truncated; a failed run leaves the terminal open.
- The hidden token prompt of `burrow login --token -` in the Windows console.
- `burrow update` while `burrow up` or the service is running; a second update
  while the previous `burrow.exe.old` is still held; the staged file under
  Defender and SmartScreen (it must start within 10 seconds); the hint when the
  directory is not writable.
- `burrow service install` from `%ProgramFiles%\burrow` only: the owner and
  ACL of `%ProgramData%\burrow` (SYSTEM and Administrators), start, stop,
  restart, the command line in `sc qc burrow`, a pre-planted directory is
  refused, the hint in a non-elevated terminal, and that the install from
  `%LOCALAPPDATA%\Programs\burrow` is refused.

**Linux (systemd)**

- `sudo burrow service install` with a path that contains a space; the unit
  file is `0644` and root-owned; the service starts after a reboot without a
  login; it restarts after `kill -9`; `journalctl -u burrow` shows its log; the
  hint printed without `sudo`.

**macOS (launchd)**

- `burrow service install` loads the agent; it starts at login; the log file
  `~/Library/Logs/burrow/burrow.err.log` appears; `sudo` is refused;
  `burrow service uninstall` unloads it.
- A client downloaded in a browser is quarantined (see below); one fetched by
  the installer runs.

### Installing without GitHub {#air-gapped}

For machines that cannot reach the release host, put the archives and
`checksums.txt` of the release (the same file names as above) in a directory on
the relay and set `BURROW_CLIENT_DOWNLOAD_DIR` to it. The relay then serves
`/download/…` itself instead of redirecting, and the installers and
`burrow update` work unchanged. The directory must hold the files directly (not
in a subdirectory), and symbolic links that lead out of it are not followed.

### macOS and the client binary

A binary fetched with `curl`, which is what the installer uses, runs as it is.
One downloaded in a browser is quarantined by macOS until you allow it in
**System Settings → Privacy & Security**. The binaries are not signed or
notarised.

---

## Next steps

- [Connect a client](/guide/connect-client) — install the `burrow` CLI and
  expose your first service.
- [Expose services](/guide/expose-services) — HTTP vs TCP tunnels, apps
  behind a path, stable TCP ports.
- [Access control & security](/guide/access-control) — lock down tunnels with
  API keys, `burrow_login`, or mTLS.
- [Configuration](/guide/configuration) — full `BURROW_*` environment variable
  reference.
- [Operations](/guide/operations) — backups, log levels, upgrading.
