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

::: info Path URLs and mTLS
The only features not available on `/svc/<slug>/` URLs are mTLS and custom
domains. Mutual TLS requires a dedicated TLS handshake on the opt-in
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

- A domain name you control (e.g. `burrow.insingo.com`).

---

## 3. DNS

Create a single A record pointing to your server's IP. No wildcard record or
wildcard certificate is needed; HTTP tunnels are served under
`/svc/<slug>/`.

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
