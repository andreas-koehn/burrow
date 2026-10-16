# Expose services

Burrow supports two tunnel types: **`tcp`** for raw TCP traffic and **`http`**
for HTTP/HTTPS applications. Each type has its own routing model, URL shape, and
access-control surface.

---

## The short way: `burrow http` and `burrow tcp`

After `burrow login` (see [Connect a client](/guide/connect-client)), one
command exposes a local port:

```sh
burrow http 3000
burrow tcp 22
```

`<target>` is what is listening locally: `3000` means `127.0.0.1:3000`;
`localhost:3000` and `192.168.1.20:8080` name a host and port; `http://localhost:3000`
means the same as `localhost:3000`. A path or an `https://` address is
rejected: HTTPS upstreams are not supported.

| Flag | Applies to | Meaning |
|------|-----------|---------|
| `--name <name>` | both | Service name; default `<hostname>-<port>` |
| `--slug <slug>` | `http` | Path under `/svc/`; 3 to 40 lowercase letters, digits and hyphens. Used only when the service is created |
| `--access open\|login\|api-key` | `http` | Access mode. Used only when the service is created |
| `--remote <port>` | `tcp` | Fixed public port; default any free port |

```sh
burrow http 3000 --name my-app --slug my-app --access login
burrow tcp 22 --remote 9001 --name ssh
```

The default name is stable for a machine and port, so running the same command
again reuses the same service, its URL and its access settings. `--slug` and
`--access` apply **only when the service is created**; the client never changes
an existing service. If the service already exists with other values, the
client says so and names the dashboard page where they are changed.

### Access {#access}

A service created by `burrow http` is **`open` by default**: anyone who has the
URL can use it, with no login and no key. The status view says "anyone with the
URL". The three values of `--access` map to the relay's access modes:

| `--access` | Relay access mode | Who can use the service |
|------------|-------------------|-------------------------|
| `open` | `open` | anyone with the URL |
| `login` | `burrow_login` | people who log in to this Burrow relay |
| `api-key` | `api_key` | callers with an API key for the service |

`--access api-key` creates **no key**. The service refuses every request until
you create a key in the dashboard (the client prints the page).

Setting `login` or `api-key` at creation needs permission to configure
services (`services:configure:own` or `services:configure:any`); asking for
`open` needs none. Against a relay that is too old to apply the wish,
`burrow http --access login` (or `api-key`) ends with exit code 1 and tells you
so. The service entry it left on the relay is open: set its access mode in the
dashboard. `--access open` works against any relay. See
[Access control](/guide/access-control) for what each mode does.

An open `/svc/` app shares the dashboard's origin, and its scripts can call the dashboard API as the signed-in user, so expose only apps you trust (see [Apps behind a path](/guide/expose-services#apps-behind-a-path)).

The examples below use `burrow.yaml` with `burrow connect --config`, which
still works unchanged. The same files work with `burrow up` (which can also read
the stored sign-in, so `server` and `token` may be left out); see
[Connect a client](/guide/connect-client#burrow-yaml-and-burrow-up).

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

A provider serves models under its own base URL,
`https://<auth_domain>/ai/<provider>/v1`, which any OpenAI-compatible app
can use with a gateway key. Providers are managed in the dashboard under
**AI Gateway → Providers**.

There are two kinds:

- **Tunnel** — an HTTP service in API-key mode behind a Burrow client (a local
  Ollama or vLLM, for example).
- **Direct** — a hosted API that the relay calls itself (OpenRouter, z.ai, or
  any other OpenAI- or Anthropic-format HTTPS API).

Errors that Burrow produces under `/ai/` are JSON of the form
`{"error":{"message":"…","type":"burrow_error","code":"…"}}`. Everything else
is the upstream's own status, headers and body, with `Burrow-Request-Id` and
`Burrow-Provider` added.

- Requests under `/ai/` are recorded as usage events. The request inspector
  (**AI Gateway → Requests**) captures them only for a provider whose service
  has it switched on: the dashboard has no switch for this yet, so set
  `inspector.enabled` to `true` with `PUT /api/v1/services/<id>/ai-config`,
  sending the current configuration with that field changed. They write no
  connection-log rows.
- The IP / geo policy of the provider's backing service applies to `/ai/`
  requests and is checked before the gateway key.

### Tunnel providers

Choose **New provider → A model server on one of my machines**. Give the
provider a name, pick the model server (Ollama on 11434, LM Studio on 1234, or
another port or address) and the API format it speaks. **Continue** creates a
service that only the AI gateway reaches and the provider on top of it, and
shows the command to run on the machine:

```sh
burrow http 11434 --name <name>
```

The dialog waits and turns to **Online** once a client serves the provider. It
can be closed before that; the provider's page shows the state.

Once it is online the dialog offers **Create a model** and **Create a gateway
key**, both filled in for this provider. Apps send the gateway key. A model
is a name of your own that apps ask for on the gateway's base URLs
(`/openai/v1` and `/anthropic`, shown in full on the **AI Gateway** overview)
instead of the provider's.

A model with several targets tries them in a routing order. **Ordered** (the
default) keeps the order you configured. **Fastest** puts the target whose
provider has the lowest p95 latency over the last 24 hours first (the figure
the providers page shows, at most a minute old). **Cheapest** puts the lowest
blended price first (three parts input to one part output, per million tokens,
from the pricing table). A target is priced only when its model is in the
pricing table (bundled bare names, or an entry added in the pricing editor); a
target priced at 0, such as a local model, always comes first. Targets without
a measurement or a price, and latencies above 120 seconds, go last in
configured order. After a restart the latency figure is empty for the first
requests, which then use the configured order. Fallback is
unchanged: if the first target fails, the next one is tried.

To use a service that already exists, follow section (C) to expose the model
server with the `api_key` access mode, then choose **Use a service that already
exists** in the same dialog and pick the service.

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
   **Other API**, and check the name, the slug, the API format, the base URL
   and the credential slot. The base URL must be an `https` URL up to and
   including the API's version path, for example
   `https://openrouter.ai/api/v1`. The slot field lists the slots that are
   set on the relay; for a name that is not set it shows the variable to add.
3. Open the provider. Its **Overview** tab shows the base URL and an example
   under **Use it in an app**. Apps send a gateway key: create one under
   **AI Gateway → Gateway keys**.

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

- **Credentials.** The caller's gateway key and cookies stop at the relay.
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
- **Models.** **Sync models** on the provider's **Catalog** tab reads the
  upstream's model list; `GET /ai/<provider>/v1/models` is answered from the
  stored list. A tunnel provider's `/v1/models` is forwarded to the local
  server unless models were added by hand.

### System text for models and gateway keys {#system-text}

A model and a gateway key can each carry a **system text** (plain text, at
most 8192 bytes): house rules every app gets and none can remove. Burrow puts
it in front of whatever system content the app sent, in this order: the
model's text, a blank line, the key's text, then the app's own. The app's
content is never removed, reordered or rewritten.

Where the text goes depends on the format of the request:

- **Chat Completions** (`/v1/chat/completions`): one new `system` message at
  the start of `messages`. A `system` or `developer` message of the app
  follows it.
- **Responses** (`/v1/responses`): in front of `instructions`, or as
  `instructions` when the app sent none.
- **Messages** (`/v1/messages`): in front of `system`. A string `system`
  stays a string; when the app sent blocks, the text is a new first text
  block; without `system` it becomes the string.

Good to know:

- The guardrails inspect what the app sent, before the text is added.
- The text is part of the cache key: two requests that differ only in system
  text never share a cached answer.
- The request inspector shows the request as it went upstream, with the text
  in place. The app does not see it.
- It is not added to embeddings, to counting tokens, or to the image, audio
  and rerank endpoints. A model's text applies when the app names the model;
  a key's text applies on every request that key makes to a chat, responses
  or messages endpoint, including `/ai/<provider>/v1` and direct
  `<provider>/<model>` addresses. A service API key has no text.
