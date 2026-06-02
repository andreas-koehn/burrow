# Expose services

Burrow supports two tunnel types: **`tcp`** for raw TCP traffic and **`http`**
for HTTP/HTTPS applications. Each type has its own routing model, URL shape, and
access-control surface.

---

## (A) TCP tunnel — SSH, Postgres, and other raw protocols

A TCP tunnel forwards any TCP connection to a port on the relay. The upstream
sees raw bytes — no HTTP parsing, no header injection.

### `burrow.yaml`

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE
services:
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001
```

- **`type: tcp`** — selects raw TCP forwarding (this is also the default when
  `type` is omitted).
- **`remote: 9001`** — requests a fixed port on the relay. Without this, the
  relay assigns a port from the auto-pool (`BURROW_PORT_MIN`–`BURROW_PORT_MAX`,
  default `9000`–`9100`).

### Connect

```sh
burrow connect --config burrow.yaml
```

The tunnel is reachable at **`burrow.insingo.com:9001`** for as long as the
client stays connected.

```sh
# SSH example
ssh -p 9001 user@burrow.insingo.com

# Postgres example
psql "host=burrow.insingo.com port=9001 user=myuser dbname=mydb"
```

::: warning Firewall and Docker Compose
TCP tunnel data ports must be open in your firewall and published in your
Compose file. If you are running the relay in Docker, add the port to your
`compose.yaml`:

```yaml
ports:
  - "80:80"
  - "443:443"
  - "7000:7000"
  - "9001:9001"   # ← add one line per fixed TCP port you use
```

Without this, the port is reachable inside the container network but not from
the internet.
:::

::: tip Stable port across reconnects
Set `remote:` to the same integer every time. Without it, the relay picks a
port from the auto-pool and may assign a different one after a disconnect.
:::

---

## (B) HTTP app over HTTPS at `/t/<id>`

An `http` tunnel wraps your application in an HTTP reverse proxy. The relay
terminates TLS, forwards requests to your local process, and rewrites
`Location` redirect headers.

### `burrow.yaml`

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE
services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http
```

- **`type: http`** — enables HTTP reverse-proxy mode.
- **No `remote:` needed** — HTTP tunnels do not use a raw port; they are
  routed by subdomain or path prefix.

### Connect

```sh
burrow connect --config burrow.yaml
```

After connecting, open the dashboard at `https://burrow.insingo.com`. Your
tunnel appears under **Tunnels**. The tunnel detail panel shows the assigned
**subdomain ID** (e.g. `abc123`).

### Two ways to reach the tunnel

| Entry point | URL |
|-------------|-----|
| Subdomain routing | `https://abc123.burrow.insingo.com/` |
| Path routing (same origin) | `https://burrow.insingo.com/t/abc123/` |

Both reach the same upstream. Use the path-routing URL when sharing a single
hostname is simpler (e.g. for webhooks, embeds, or API consumers who cannot
set a wildcard DNS record).

::: info Path routing and base paths
The relay strips the `/t/<id>` prefix before forwarding and rewrites
`Location` redirect headers so most apps work without configuration. However,
if your application hardcodes its root path (e.g. it generates links as `/`,
not relative to its mount point), consider setting a base path in the
application's own config before exposing it via path routing.

Apps that serve from their own root with no path prefix work correctly under
subdomain routing (`https://abc123.burrow.insingo.com/`).
:::

::: info mTLS is not available on path-routed URLs
The `/t/<id>` route shares the dashboard TLS handshake and cannot present a
per-service client CA. If you need mTLS, use the subdomain entry point
(`https://abc123.burrow.insingo.com/`) via the `:8443` listener. See
[Access control](/guide/access-control) for details.
:::

---

## (C) API with an API-key layer

This is the most common pattern for exposing a backend API to external callers
without requiring a Burrow account.

### 1. Declare the tunnel

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE
services:
  - name: my-api
    local: 127.0.0.1:8080
    type: http
```

### 2. Connect

```sh
burrow connect --config burrow.yaml
```

### 3. Set the access mode to `api_key`

In the dashboard, open the tunnel's detail panel and navigate to **Access
policy**. Select **API key** and save.

::: details Alternatively, use the REST API
```sh
curl -X PUT https://burrow.insingo.com/api/v1/services/<id>/access-policy \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -d '{"access_mode": "api_key"}'
```
:::

### 4. Mint an API key

In the dashboard, open the tunnel detail panel and click **Add API key**. Give
it a name and copy the generated value — it is shown once.

::: details Alternatively, use the REST API
```sh
curl -X POST https://burrow.insingo.com/api/v1/services/<id>/api-keys \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -d '{"name": "ci-runner"}'
```
:::

### 5. Call the API

**Default header — `Authorization: Bearer <key>`**

```sh
curl https://burrow.insingo.com/t/abc123/health \
  -H "Authorization: Bearer buk_YOUR_API_KEY"
```

**Custom header (e.g. `X-Api-Key`) — raw key, no `Bearer` prefix**

If the dashboard access-policy is configured with a custom header name (e.g.
`X-Api-Key`), callers send the key value directly:

```sh
curl https://burrow.insingo.com/t/abc123/health \
  -H "X-Api-Key: buk_YOUR_API_KEY"
```

::: warning No `Bearer` prefix on custom headers
When a custom header name is set, send only the raw key — the relay calls
`strings.TrimSpace` on the value and validates it as-is. Adding a `Bearer `
prefix will cause a `401 Unauthorized`.
:::

Missing or invalid keys receive:

```
HTTP/1.1 401 Unauthorized
{"error":"missing api key"}
```

or

```
HTTP/1.1 401 Unauthorized
{"error":"invalid api key"}
```

For a full description of all access modes (`open`, `api_key`, `burrow_login`,
`mtls`) and their configuration options, see [Access control](/guide/access-control).

---

## Choosing between path routing and subdomain routing

| | Subdomain `https://<id>.burrow.insingo.com/` | Path `/t/<id>/` |
|---|---|---|
| Requires wildcard DNS | Yes | No |
| mTLS support | Yes | No |
| Works as webhook target | Yes | Yes |
| App needs base-path config | Usually no | Sometimes |
| Same origin as dashboard | No | Yes |

Use subdomain routing when you control DNS and want the cleanest URL. Use
path routing when you need a single domain or are behind a load balancer that
does not support wildcard certificates.
