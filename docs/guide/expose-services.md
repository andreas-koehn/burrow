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

## (B) HTTP app over HTTPS at `/svc/<slug>/`

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
  served under a path on the relay's domain.

### Connect

```sh
burrow connect --config burrow.yaml
```

The client prints the service URL on `tunnel registered`. You can also open the
dashboard at `https://burrow.insingo.com`; the service appears under **Tunnels**
with its URL.

### The service URL

Every HTTP service is served at:

```
https://burrow.insingo.com/svc/k7p2qx/
```

`k7p2qx` is the slug. Burrow suggests one; you can change it in the "New
service" dialog and on the service's page ("Edit URL"). Changing the slug breaks
the old URL at once.

One A record and one certificate are enough; there is no wildcard DNS.

The relay strips the `/svc/<slug>` prefix before forwarding and rewrites
`Location` redirect headers so most apps work without configuration.

### Apps behind a path

- An app that loads assets from absolute paths (`/static/app.js`) needs
  base-path support. Look for a setting such as `--base /svc/<slug>/` or a
  "base URL" or "public path" option in the app's own configuration.
- Path-routed apps share the dashboard's origin, so only expose apps you trust.
  Burrow removes its own cookies before forwarding, but a page served under
  `/svc/` can still call the dashboard API as the signed-in user.

::: info mTLS is not available on path URLs
`/svc/<slug>/` shares the dashboard TLS handshake and cannot present a
per-service client CA. mTLS needs the opt-in host-routed ingress. See
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
curl https://burrow.insingo.com/svc/k7p2qx/health \
  -H "Authorization: Bearer buk_YOUR_API_KEY"
```

**Custom header (e.g. `X-Api-Key`) — raw key, no `Bearer` prefix**

If the dashboard access-policy is configured with a custom header name (e.g.
`X-Api-Key`), callers send the key value directly:

```sh
curl https://burrow.insingo.com/svc/k7p2qx/health \
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

## (D) Model providers at `/ai/<provider>/v1`

A provider is a model backend with its own base URL,
`https://<auth_domain>/ai/<provider>/v1`, which any OpenAI-compatible client
can use with a Burrow API key. Providers are managed in the dashboard under
**AI Gateway → Providers**.

There are two kinds:

- **Tunnel** — an HTTP service in API-key mode behind a Burrow client (a local
  Ollama or vLLM, for example).
- **Direct** — a hosted API that the relay calls itself (OpenRouter, z.ai, or
  any other OpenAI-compatible HTTPS endpoint).

Errors that Burrow produces under `/ai/` are JSON of the form
`{"error":{"message":"…","type":"burrow_error","code":"…"}}`. Everything else
is the upstream's own status, headers and body, with `Burrow-Request-Id` and
`Burrow-Provider` added.

- Requests under `/ai/` are recorded as usage events and in the inspector.
  They write no connection-log rows.
- The IP / geo policy of the provider's backing service applies to `/ai/`
  requests and is checked before the API key.

### Tunnel providers

Follow section (C) to expose the model server with the `api_key` access mode,
then choose **New provider → A service behind a Burrow client** and pick the
service.

::: warning Switching a service to API-key mode does not create a provider
Services that were already in API-key mode when the relay was upgraded were
registered as providers once, at that first start. A service that is created
or switched to API-key mode later has no provider until you add one.
:::

### Direct providers {#direct-providers}

1. Put the upstream's API key into the relay's environment as
   `BURROW_UPSTREAM_KEY_<SLOT>` (or `BURROW_UPSTREAM_KEY_<SLOT>_FILE`), for
   example `BURROW_UPSTREAM_KEY_OPENROUTER=sk-or-…`, and restart the relay.
   The key is never entered in the dashboard or sent through the API. See
   [Configuration](/guide/configuration#advanced) for the slot naming rule
   and for who can use a slot.
2. Choose **New provider → A hosted API**, pick a preset (OpenRouter, z.ai) or
   **Other OpenAI-compatible API**, and check the name, the slug, the base URL
   and the credential slot. The base URL must be an `https` URL up to and
   including the API's version path, for example
   `https://openrouter.ai/api/v1`.
3. Open the provider, add an API key on its **API keys** tab, and use the base
   URL and the example on its **Connect** tab.

::: details Alternatively, use the REST API
```sh
curl -X POST https://burrow.insingo.com/api/v1/ai/providers \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -d '{"kind": "direct", "slug": "openrouter", "name": "OpenRouter",
       "base_url": "https://openrouter.ai/api/v1", "credential_slot": "OPENROUTER"}'
```
:::

While the slot is not set, or is set to an empty value, the provider is shown
as **not configured** and answers `503` with the code
`provider_not_configured`.

What the relay does with a direct provider's traffic:

- **Credentials.** The caller's Burrow API key and cookies stop at the relay.
  The upstream receives the slot's value in the configured header
  (`Authorization: Bearer <key>` by default).
- **Rejected credential.** An upstream `401` or `403` is answered as `502`
  with the code `upstream_auth_failed`: the caller's key was fine, the
  relay's was not.
- **Redirects are not followed.** An upstream redirect is answered as `502`
  with the code `upstream_redirect`, so the credential never travels to
  another host. Fix the base URL instead.
- **Cookies.** `Set-Cookie` headers of the upstream are dropped; `/ai/` shares
  the dashboard's origin.
- **Private addresses.** The relay refuses to connect to a base URL that
  resolves to a private, loopback or link-local address, both when the
  provider is saved (`400`) and on every connection (`502` with the code
  `upstream_unavailable`). Set `BURROW_AI_ALLOW_PRIVATE_UPSTREAMS=true` for a
  self-hosted OpenAI-compatible server on your own network.
- **Cost.** When a direct provider's response states what the request cost,
  that figure is recorded and used on the cost pages. A cost stated by a
  tunnel provider is ignored, because whoever runs the tunnel controls it;
  its cost is always computed from the pricing table.
- **Models.** **Sync models** on the provider's **Models** tab reads the
  upstream's model list; `GET /ai/<provider>/v1/models` is answered from the
  stored list. A tunnel provider's `/v1/models` is forwarded to the local
  server unless models were added by hand.
