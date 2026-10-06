# Operations

Day-2 tasks for a running Burrow relay: backups, upgrades, certificates, and logs.

---

## Backups

### In-app backup

The dashboard **Settings → Backup** panel and the `burrowd backup` command both
produce a timestamped archive of the SQLite database. Archives land in the
directory set by `BURROW_BACKUP_DIR` (default: `<database_path>.backups`).

```sh
burrowd backup
```

Restore from an archive with:

```sh
burrowd restore <archive-file>
```

::: warning Stop the server before restoring
`burrowd restore` writes directly to the database file. Bring the container
down before running restore to avoid corruption.
:::

### Volume snapshot (Docker)

Back up the `burrow_data` Docker volume with a one-liner. The container does
not need to be stopped for a read-consistent snapshot — SQLite's WAL mode keeps
the database readable while the copy runs.

```sh
docker run --rm \
  -v burrow_data:/data:ro \
  -v "$(pwd)/backups":/out \
  alpine \
  tar czf /out/burrow-backup-$(date +%Y%m%d-%H%M%S).tar.gz -C /data .
```

This writes a compressed archive to `./backups/` on the host. Schedule it with
`cron` or your infrastructure's snapshot tool.

::: tip Restore a volume snapshot
```sh
# Stop the relay first
docker compose down

# Restore the archive into the volume
docker run --rm \
  -v burrow_data:/data \
  -v "$(pwd)/backups":/out \
  alpine \
  tar xzf /out/burrow-backup-<timestamp>.tar.gz -C /data

# Start the relay again
docker compose up -d
```
:::

---

## Upgrades

### Docker (recommended)

Pull the latest `:develop` image and restart the stack with zero downtime:

```sh
docker compose pull
docker compose up -d
```

The relay container is replaced in-place. Active client connections disconnect
briefly and reconnect automatically.

::: info Image visibility
The GHCR package starts **private** after the first push. Make it public once at
https://github.com/users/andreas-koehn/packages/container/burrow/settings — or run
`docker login ghcr.io` before pulling on hosts that cannot reach a public
package.
:::

::: tip Pin to a digest for reproducible deployments
```yaml
# docker-compose.yml
services:
  burrowd:
    image: ghcr.io/andreas-koehn/burrow:develop@sha256:<digest>
```
:::

### Binary install

Download the latest rolling binary from the `develop` release page:

```
https://github.com/andreas-koehn/burrow/releases/tag/develop
```

Replace the running `burrowd` binary and restart the service.

### Build from source

```sh
git pull
go build ./cmd/server
go build ./cmd/client
```

Then restart `burrowd` with the new binary.

---

## Certificates

When `BURROW_ACME_DOMAIN` is set, Burrow manages the full certificate
lifecycle via [CertMagic](https://github.com/caddyserver/certmagic) (backed by
Let's Encrypt):

- **Issuance**: certificates are obtained before any listener starts.
- **Renewal**: CertMagic auto-renews well before expiry — no cron job needed.
- **Storage**: accounts and certificates are written to `BURROW_ACME_STORAGE`
  (default: `<dir(database_path)>/acme`). Include this directory in backups.

There is nothing to operate for certificates under normal conditions.

::: warning Renewal requires port 80
CertMagic uses the HTTP-01 challenge. Port `80` must remain reachable from the
public internet at all times — including during renewals. See
[Deploy](/guide/deploy) for firewall rules.
:::

::: details Testing with Let's Encrypt staging
Point `BURROW_ACME_CA` at the staging CA to avoid rate-limit errors while
validating a new setup:

```env
BURROW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory
```

Staging certificates are signed by an untrusted root, so browsers will warn.
Switch back to the default (production) CA once everything works.
:::

---

## Logs

Stream live logs from a running Docker stack:

```sh
docker compose logs -f
```

Scope to the relay container only:

```sh
docker compose logs -f burrowd
```

### Log format and level

Control verbosity and format via environment variables:

```env
BURROW_LOG_LEVEL=info      # debug | info | warn | error
BURROW_LOG_FORMAT=text     # text | json
```

Switch to `json` format for structured log ingestion (e.g. into Loki, Datadog,
or CloudWatch):

```env
BURROW_LOG_FORMAT=json
BURROW_LOG_LEVEL=warn
```

::: tip Debugging a connection issue
Set `BURROW_LOG_LEVEL=debug` temporarily. This logs individual proxy requests,
TLS handshakes, and ACME challenge activity. Revert to `info` in production —
debug output is verbose.
:::

---

## Health check

The relay exposes a health endpoint on the dashboard listener:

```sh
curl -sf https://burrow.insingo.com/healthz
```

A `200 OK` response indicates the server is up. Wire this into your load
balancer or uptime monitor.

---

## Retention

Log and connection-record retention is configured in **Settings → General** in
the dashboard. There is no env var — the setting is stored in the database and
applies immediately without a restart.

---

*Next: [Troubleshooting](/guide/troubleshooting) — common problems and fixes.*
