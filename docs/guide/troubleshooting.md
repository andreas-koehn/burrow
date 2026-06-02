# Troubleshooting

Common issues, what causes them, and how to fix them.

---

## Image pull denied

```
Error response from daemon: pull access denied for ghcr.io/ankoehn/burrow
```

**Cause:** The GHCR package is published as private by default on the first push.

**Fix — option A (recommended):** Make the package public at:
`https://github.com/users/ankoehn/packages/container/burrow/settings`

**Fix — option B:** Authenticate before pulling:

```sh
echo $GITHUB_TOKEN | docker login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
docker pull ghcr.io/ankoehn/burrow:develop
```

::: tip
Once public, anonymous pulls work from any machine — no credentials needed.
:::

---

## Let's Encrypt certificate not issued

`burrowd` blocks at startup until certificates are obtained. If it hangs or exits immediately, one of the following is the cause.

### DNS is not resolving

ACME HTTP-01 requires Let's Encrypt to reach your server over the public internet.
Before setting `BURROW_ACME_DOMAIN`, verify:

```sh
dig +short burrow.insingo.com
# must return your server's public IP
```

### Port 80 is not reachable

Let's Encrypt validates via `http://burrow.insingo.com/.well-known/acme-challenge/...`.
Port 80 must be open in your firewall and not bound by another process.

```sh
# check if something else holds :80
ss -tlnp | grep ':80'
```

See [Deploy](/guide/deploy) for the full firewall checklist.

### Test with the staging CA first

::: warning
Every failed real-CA attempt counts against Let's Encrypt rate limits. Use staging until your setup is confirmed to work.
:::

```env
BURROW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory
```

Staging issues untrusted certificates, so browsers will warn — that is expected.
Remove the override once staging succeeds.

### File-cert variables left set

`BURROW_ACME_DOMAIN` is mutually exclusive with `BURROW_TLS_CERT/KEY`,
`BURROW_HTTP_TLS_CERT/KEY`, and `BURROW_HTTP_PROXY_TLS_CERT/KEY`. Unset all
file-cert variables when switching to ACME.

---

## Dashboard returns 502 or won't load

### Scenario: reverse proxy in front of burrowd

If you have nginx/Caddy/Traefik forwarding to `burrowd`, check:

1. The backend address matches where `burrowd` actually listens (`localhost:8080` by default, `localhost:443` with ACME).
2. `proxy_pass` / `reverse_proxy` is pointed at the correct port.

::: info
With ACME enabled (`BURROW_ACME_DOMAIN` set), burrowd promotes the dashboard from `:8080` to `:443` automatically — only when using the stock default. If you customised `BURROW_HTTP_LISTEN`, it stays on your custom port.
:::

### Scenario: container not healthy

```sh
docker compose ps          # check State
docker compose logs burrowd --tail 50
```

If you see `address already in use`, another process holds the port — stop it or change `BURROW_HTTP_LISTEN`.

### Scenario: ACME startup blocked

burrowd calls `certmagic.ManageSync`, which blocks all listeners until certificates are ready. If DNS or port 80 is not configured, the dashboard never starts. See [Let's Encrypt certificate not issued](#let-s-encrypt-certificate-not-issued) above.

---

## Login redirect loop or cookie not working

**Symptom:** After a successful login the browser is redirected back to the login page in a loop.

**Cause:** The session cookie is set with `Secure` and `SameSite=Strict`, but it
is not reaching the browser with the `Secure` flag because TLS is terminated by
an upstream proxy.

**Fix:** Set both of these environment variables:

```env
BURROW_HTTP_SECURE_COOKIES=true
BURROW_TRUSTED_PROXIES=<your-proxy-IP-or-CIDR>
```

::: info
When ACME is enabled, burrowd terminates TLS itself — there is no upstream
proxy, so this issue does not apply. Secure cookies work automatically.
:::

---

## Client cannot connect to the relay

**Symptom:** `burrow connect` fails or times out immediately.

Work through these checks:

### 1. Verify the server address

The relay control channel listens on port **7000**, not 443 or 8080.

```yaml
# burrow.yaml
server: burrow.insingo.com:7000
```

### 2. Verify port 7000 is reachable

```sh
nc -zv burrow.insingo.com 7000
# Expected: Connection to burrow.insingo.com 7000 port [tcp/*] succeeded!
```

If it fails, open port 7000 in your server firewall. See [Deploy](/guide/deploy).

### 3. Certificate trust issues (dev-cert or staging CA)

If the server was started with `burrowd serve --dev-certs` or a self-signed cert,
pass `--insecure` (testing only) or provide the CA via `--cacert`:

```sh
burrow connect --config burrow.yaml --insecure
# or
burrow connect --config burrow.yaml --cacert /path/to/ca.pem
```

::: warning
`--insecure` disables TLS verification. Use only in isolated dev environments.
:::

### 4. Token is invalid or missing

Make sure the token is set in `burrow.yaml` (`token: bur_YOUR_TOKEN_HERE`) or
passed via `--token`. Mint a token in the dashboard under **Settings → Tokens**,
or with:

```sh
burrowd token --email admin@example.com --name my-laptop
```

---

## API key returns 401

**Symptom:** Calling a service with access mode `api_key` returns:

```json
{"error":"missing api key"}
```
or
```json
{"error":"invalid api key"}
```

### Check which header the service expects

The **default** header is `Authorization` with a `Bearer ` prefix:

```sh
curl -H "Authorization: Bearer buk_YOUR_API_KEY" https://burrow.insingo.com/t/abc123/
```

If a **custom header** is configured (e.g. `X-Api-Key`), send the raw key value — no `Bearer ` prefix:

```sh
curl -H "X-Api-Key: bur_YOUR_TOKEN_HERE" https://burrow.insingo.com/t/abc123/
```

::: tip
Check the service detail page in the dashboard to see which header is configured and copy a working `curl` example.
:::

### Verify the key is active

API keys can be revoked. Go to the service detail page → **API Keys** to confirm the key is still listed and not revoked.

---

## Raw TCP tunnel port unreachable

**Symptom:** `nc -zv burrow.insingo.com 9001` times out even though the tunnel appears connected.

### 1. Port is not published

For Docker deployments, the TCP port must be published in `compose.yaml`:

```yaml
services:
  burrowd:
    ports:
      - "9001:9001"   # or the full range: "9000-9100:9000-9100"
```

### 2. Firewall is blocking the port

Open the specific port (or range) on your server:

```sh
# ufw example
ufw allow 9001/tcp
```

### 3. Wrong port — tunnel uses auto-assigned port

If `remote:` is not set in `burrow.yaml`, the port is auto-assigned from the
`BURROW_PORT_MIN`–`BURROW_PORT_MAX` range (default 9000–9100). Check the actual
assigned port in the dashboard under the tunnel detail, then use `remote: 9001`
in `burrow.yaml` to pin a stable port:

```yaml
services:
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001
```

---

## mTLS not working on `/t/{id}` path

**Symptom:** mTLS access mode appears configured, but requests via
`https://burrow.insingo.com/t/<id>/` are not challenged for a client certificate.

**Cause:** This is expected. mTLS verification happens at the TLS handshake
(`GetConfigForClient`) and requires a dedicated TLS connection per service. The
`/t/{id}` path-routing endpoint shares the dashboard TLS connection and cannot
perform a per-service handshake.

**Fix:** Use the subdomain endpoint on port 8443, which has a dedicated TLS listener:

```
https://abc123.burrow.insingo.com/
```

See [Access control & security](/guide/access-control) for the full mTLS setup guide.

---

## Subdomain routing not working (`AUTH_DOMAIN` not set)

**Symptom:** Tunnels are created successfully, but `https://abc123.burrow.insingo.com/`
returns a DNS error or hits the wrong host.

**Checks:**

1. **Wildcard DNS**: Your DNS must have a wildcard `A` record pointing `*.burrow.insingo.com` to your server IP.

2. **`BURROW_AUTH_DOMAIN`**: When ACME is enabled, `auth_domain` is inferred from `BURROW_ACME_DOMAIN` automatically — you do not need to set it separately. Without ACME, set it explicitly:

   ```env
   BURROW_AUTH_DOMAIN=burrow.insingo.com
   ```

3. **Proxy ingress port**: The HTTP tunnel ingress listens on `:8443` by default. Ensure port 8443 is published and reachable, or use the `/t/{id}` path-routing alternative on the dashboard origin (port 443).

---

## Enabling debug logs

When none of the above resolves the issue, enable verbose logging:

```env
BURROW_LOG_LEVEL=debug
BURROW_LOG_FORMAT=json
```

Then stream and filter logs:

```sh
docker compose logs -f burrowd 2>&1 | grep -i "error\|warn\|acme\|cert"
```

::: details What to include in a bug report
- `burrowd version` output
- Full startup log (debug level) up to the first error
- The `BURROW_*` env vars in use (redact passwords and tokens)
- Whether ACME or file-cert mode is active
- Client OS + `burrow version` output
:::
