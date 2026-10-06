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

## Start with `burrow doctor`

When the client does not work, run:

```sh
burrow doctor
```

It prints one line per check, with a sentence on how to fix a failure, and
never prints the token. The checks, in order: user config, relay name, relay
discovery over HTTPS, control endpoint (TCP, then TLS), token, client and relay
versions, system clock (within two minutes of the relay's), and, when a
`burrow.yaml` is found, whether each service's local target accepts a
connection.

The exit code is 0 when nothing failed. Otherwise it is the code of the first
failure: 3 not signed in, 4 token rejected, 5 relay unreachable or certificate
not trusted, 6 client too old, 1 anything else. `burrow status` shows what this
machine is signed in to without connecting.

### Not signed in

```
Not signed in. Run: burrow login <your relay address>
```

Exit code 3. No flag, environment variable, `burrow.yaml` or stored sign-in
gave a control endpoint and a token. Run `burrow login burrow.insingo.com`. If
the message says the stored sign-in is for another relay, a stored token is
only ever sent to the relay it was created for: sign in to the relay you mean,
or give a token with `BURROW_TOKEN`.

### The relay rejected this machine's token

```
The relay rejected this machine's token. It may have been revoked. Run: burrow login <relay>
```

Exit code 4. The token was revoked or deleted, or belongs to another relay. Run
`burrow login <relay>` again (add `--force` to replace the stored sign-in
without the question).

### Cannot reach the relay

```
Cannot reach burrow.insingo.com:7000. Check the address and that port 7000 is open. Details: burrow doctor
```

`burrow login`, `burrow update` and `burrow doctor` stop with exit code 5;
`burrow http`, `burrow tcp` and `burrow up` print the line once and keep
retrying. Check the address for typos and that the control port (7000 unless
your relay uses another) is open in the firewall:

```sh
nc -zv burrow.insingo.com 7000
```

A relay without a discovery endpoint (an older relay) is assumed to have its
control endpoint at `<relay host>:7000`. If yours is elsewhere, give it:
`burrow login <relay> --control host:port`.

### The certificate is not trusted

`burrow login`, `burrow update` and `burrow doctor` stop with exit code 5 and
say that the certificate is not trusted, naming who issued it. If the relay uses
its own CA, trust it with `--cacert <ca.pem>` (and `--server-name` when the name
in the certificate differs from the address). `--insecure` skips the check; use
it only against a local dev relay.

### The client is too old

```
This relay needs burrow 0.7.0 or newer. Run: burrow update
```

Exit code 6. The relay sets a minimum client version
(`BURROW_MIN_CLIENT_VERSION`). Run `burrow update`; if the binary's directory is
not writable, the command prints what to run instead.

### Browser sign-in does not work

- **"This relay does not support browser sign-in"** (exit code 2): the relay is
  older. Create a token under **Clients → Tokens** and run
  `burrow login <relay> --token -`.
- **The approval page refuses you**: approving needs an admin, or a role with
  `tokens:manage:own` or `tokens:manage:any`. A user without it can still create
  a token in the dashboard and use `--token -`.
- **Too many open sign-in requests**: the relay allows 5 per source IP and 20 in
  total, each valid for 10 minutes. Behind a reverse proxy without
  `BURROW_TRUSTED_PROXIES`, all clients share one address, so 5 is the limit for
  everybody. See [Configuration](/guide/configuration#behind-a-reverse-proxy).

### `--access login` or `api-key` was not applied

Against an older relay, `burrow http --access login` (or `api-key`) ends with
exit code 1 and says the relay did not apply it. The service entry it created is
open. Open the dashboard and set the access mode there. `--access api-key` also
never creates a key: add one in the dashboard before callers can use the
service.

### `burrow service install` refuses on Windows

The service runs as LocalSystem and refuses a `burrow.exe` that a normal user
can change. The installer puts it in `%LOCALAPPDATA%\Programs\burrow`; copy it
to `%ProgramFiles%\burrow` from an elevated terminal and install from that copy.
See [Keep it running](/guide/connect-client#keep-it-running).

---

## Client cannot connect to the relay

**Symptom:** `burrow connect` fails or times out immediately. (For `burrow http`, `burrow tcp` and `burrow up`, run `burrow doctor` first.)

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
passed via `--token`. (`burrow login` stores a token for you.) Mint a token in the
dashboard under **Clients → Tokens**, or with:

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
curl -H "Authorization: Bearer buk_YOUR_API_KEY" https://burrow.insingo.com/svc/k7p2qx/
```

If a **custom header** is configured (e.g. `X-Api-Key`), send the raw key value — no `Bearer ` prefix:

```sh
curl -H "X-Api-Key: bur_YOUR_TOKEN_HERE" https://burrow.insingo.com/svc/k7p2qx/
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

## mTLS not working on `/svc/<slug>` URLs

**Symptom:** A service is in `mtls` access mode, but requests via
`https://burrow.insingo.com/svc/<slug>/` are not challenged for a client certificate.

**Cause:** This is expected. mTLS verification happens at the TLS handshake
and requires a dedicated TLS connection per service. Path URLs share the
dashboard TLS connection and cannot perform a per-service handshake.

**Fix:** mTLS needs the opt-in host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`),
wildcard DNS and a wildcard certificate. Otherwise move the service to another
access mode.

See [Access control & security](/guide/access-control) for the mTLS constraints.

---

## Old `/svc/` URL returns 404 after a slug change

**Symptom:** A service worked at `https://burrow.insingo.com/svc/k7p2qx/` and now
returns `404`.

**Cause:** The slug was changed ("Edit URL" on the service's page). The old URL
stops working at once, and there is no redirect.

**Fix:** Use the new URL shown on the service's page or printed by the client on
`tunnel registered`, and update any bookmarks, webhooks or callers.

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
