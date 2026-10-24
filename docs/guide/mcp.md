# Manage Burrow from an agent (MCP)

Burrow has a built-in [Model Context Protocol](https://modelcontextprotocol.io)
server. An MCP client such as Claude Code, Cursor or VS Code connects to it and
can then read and change what its token allows: services, AI providers and
models, gateway keys, budgets, the audit log, users and the rest of the API.

The endpoint is part of every relay. There is nothing to switch on.

```
https://<your-relay>/mcp/burrow
```

It is on the same address as the dashboard, so a relay that is reachable in the
browser is reachable for an MCP client. Use exactly this path, without a
trailing slash: with the dashboard present, `POST /mcp/burrow/`, any other
sub-path and any method other than GET, POST and DELETE on the path are
answered with the dashboard's HTML, not with an error. `GET` and `DELETE` on
`/mcp/burrow` with a valid token are answered with `405` and `Allow: POST`:
there is no event stream and no session to end. Without a valid token they get
`401` like every other request, because the token is checked before the
method.

---

## 1. Mint a token {#token}

The endpoint takes an **automation token** (`bua_…`) in the `Authorization`
header. The session cookie of the dashboard is not accepted.

A token carries a list of permissions. Over MCP it sees exactly the tools its
permissions open, and never more than its owner's role allows: a permission
counts only when it is on the token's list **and** granted by the owner's
current role. A tool or operation outside that set is not listed, and calling
it is answered as if it did not exist. If the role is later reduced, the token
is reduced with it at once.

In the dashboard, open **Settings → Automation tokens**, pick a preset and
create the token:

| Preset | Permissions |
|--------|-------------|
| Read-only | every permission whose name contains `:read`, plus `metrics:read` and `mcp:tools:read`. It reads everything that has a read permission, which leaves out services and budgets |
| Operator | every permission except `users:manage`, `roles:manage`, `tokens:manage:any`, `automation:tokens:manage:any`, `settings:manage` and `backup:run` |
| Full | every permission |

Or pick your own list. The token is shown once; store it now, it cannot be
read again.

Through the API, log in and `POST /api/v1/automation/tokens` with the
permissions. `GET /api/v1/automation/presets` returns the preset lists:

```sh
curl -c cookies.txt -X POST https://burrow.insingo.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"changeme"}'

curl -b cookies.txt -X POST https://burrow.insingo.com/api/v1/automation/tokens \
  -H "X-CSRF-Token: $(grep -E '[[:space:]](__Host-)?burrow_csrf[[:space:]]' cookies.txt | head -1 | awk '{print $NF}')" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "claude-code",
        "permissions": ["tunnels:read:own", "audit:read", "metrics:read"]
      }'
```

The answer holds the token once, in `plaintext`. Add
`"expires_at": "2027-01-01T00:00:00Z"` to the body for a token that ends on
its own.

::: warning Without a list the token inherits everything
A token created without `permissions` inherits every permission of the user who
mints it at that moment. Give an agent a list. An explicit empty list
(`"permissions": []`) declares nothing: the token sees `me_get` and the three generic
tools, and the generic tools find only the operations every token may call.
:::

You can only put permissions on a token that your own role has.

---

## 2. Connect a client {#clients}

Replace the address and the token in the examples. **Settings → MCP** in the dashboard shows the same snippets with your relay's address filled in, ready to copy.

### Claude Code

```sh
claude mcp add --transport http burrow https://burrow.insingo.com/mcp/burrow \
  --header "Authorization: Bearer $BURROW_TOKEN"
```

To share the server with a project without sharing the token, put this in
`.mcp.json` at the project root and set `BURROW_TOKEN` in your environment:

```json
{
  "mcpServers": {
    "burrow": {
      "type": "http",
      "url": "https://burrow.insingo.com/mcp/burrow",
      "headers": {
        "Authorization": "Bearer ${BURROW_TOKEN}"
      }
    }
  }
}
```

### Cursor

`.cursor/mcp.json` in the project, or `~/.cursor/mcp.json` for every project:

```json
{
  "mcpServers": {
    "burrow": {
      "url": "https://burrow.insingo.com/mcp/burrow",
      "headers": {
        "Authorization": "Bearer ${env:BURROW_TOKEN}"
      }
    }
  }
}
```

### VS Code

`.vscode/mcp.json` in the workspace. VS Code asks for the token the first time
the server starts and keeps it in its secret store:

```json
{
  "servers": {
    "burrow": {
      "type": "http",
      "url": "https://burrow.insingo.com/mcp/burrow",
      "headers": {
        "Authorization": "Bearer ${input:burrow-token}"
      }
    }
  },
  "inputs": [
    {
      "type": "promptString",
      "id": "burrow-token",
      "description": "Burrow automation token",
      "password": true
    }
  ]
}
```

### Any other client

Any client that speaks MCP's Streamable HTTP transport and can send a header
works: point it at the URL and add `Authorization: Bearer bua_…`.

### Clients that sign in with OAuth

A client that supports MCP authorization needs only the URL. Without a token
it is answered `401` with `WWW-Authenticate: Bearer resource_metadata="…"`,
finds Burrow's authorization server from there, and sends you to the
dashboard to approve it. Its token is good at `/mcp/burrow` only. What it may
do is the scope you approve (`burrow:read`: the read permissions of your
role; `burrow:write`: everything your role may do), and never more than your
role grants at the moment of each call. Revoking the connected app ends the
token at once. An automation or client token minted through it expires no
later than the access token (1 hour). Everything else it creates through the
API, such as service API keys, webhooks or users, outlives the token and the
connected app, as with an automation token: review or delete those
separately. OAuth needs a public name for the relay (`BURROW_AUTH_DOMAIN`
or `BURROW_ACME_DOMAIN`); without one the relay takes automation tokens only.
Set a [services domain](/guide/configuration#services-domain)
(`BURROW_SERVICES_DOMAIN`) before you rely on OAuth: without one, tunnelled
apps share the consent page's origin and only your password protects an
approval.

**The approval page.** The browser opens `/oauth/consent` on the dashboard.
It names the app, shows in large type the host your browser goes back to after
you decide, and says what the app asks for. Check that host: approve only an
app you just started connecting. An app that returns to `localhost` or
`127.0.0.1` gets an extra warning, because only an app on your own computer
can receive that answer.

- At `/mcp/burrow` an app that asks for `burrow:write` gets a **Read-only
  access** switch, so you can give it less. An app that asks for
  `burrow:read` is shown as read-only and gets no switch: the page never gives
  more than the app asked for, and the relay refuses an approval that would
  (400).
- At `/mcp` and `/mcp/<source>` you tick the tool sources the app may use, and
  **Read-only tools only** is on by default. Your role must allow configuring
  the AI gateway.
- Without a [services domain](/guide/configuration#services-domain) the page
  asks for your password before **Approve**.

**Settings → Connected apps** lists the apps you approved, with what each may
reach, and revokes one at once. A holder of `oauth:clients:manage` can switch
to **All users**.

**Settings → OAuth clients** (`oauth:clients:manage`) registers an app that
cannot publish its own client metadata document: give it a name and its
redirect addresses, one per line (https, http on a loopback address with any
port, or an app scheme such as `com.example.app:/callback`). The page shows
the client ID to enter in the app; there is no secret. Deleting a client ends
its connected apps.

---

## Tools {#tools}

Two kinds of tool: 20 **curated tools** for the common tasks, and three
**generic tools** that reach every other operation of the API. A token lists
only the curated tools its permissions open; the generic tools are listed for
every token.

### Curated tools

| Tool | What it does | Kind |
|------|--------------|------|
| `me_get` | Who this token acts for: the user, the role and the permissions it holds | read |
| `services_list` | List services visible to the caller | read |
| `service_get` | Service detail (config and live state) | read |
| `known_clients_list` | Every machine that ever connected with a client token | read |
| `providers_list` | List AI providers | read |
| `provider_get` | Get one provider | read |
| `provider_metrics_get` | Per-provider metrics over the trailing 24 hours | read |
| `ai_models_list` | List synthetic models | read |
| `gateway_keys_list` | List gateway keys | read |
| `ai_requests_list` | Recent requests through the AI gateway, newest first | read |
| `inspector_request_get` | Get a captured request envelope | read |
| `cost_summary_get` | Aggregated cost summary | read |
| `budgets_list` | List budgets | read |
| `audit_events_list` | Query audit events | read |
| `connection_logs_list` | List per-tunnel connection log entries, newest first | read |
| `provider_models_sync` | Read the model list from a direct provider's upstream | delete (needs `confirm`) |
| `budget_create` | Create a budget | write |
| `provider_delete` | Delete a provider | delete |
| `ai_model_delete` | Delete a synthetic model | delete |
| `gateway_key_delete` | Revoke a gateway key | delete |

A service's slug is changed through the generic tools (`serviceSlugUpdate`),
not a curated tool: a new slug moves the service's address, and for a TCP
service the target of `burrow forward`.

The kind is sent to the client as MCP tool annotations (`readOnlyHint`,
`destructiveHint`, `idempotentHint`), so a client can ask before it runs a tool
that changes or deletes something.

### Any other operation: find, describe, call

The three generic tools work in this order:

1. `burrow_find_operations` with `query` (words from the name, path or summary;
   `""` lists everything), optionally `kind` (`read`, `write` or `delete`) and
   `limit` (1 to 100, default 25). It lists only operations the token may call,
   with `operation_id`, `tool` (the curated tool's name, or `null`), `kind`,
   `method`, `path` and `summary`.
2. `burrow_describe_operation` with `operation_id`: what the operation does,
   the permissions it needs and its arguments as a JSON Schema
   (`input_schema`).
3. `burrow_call_operation` with `operation_id` and `arguments`.

Prefer a curated tool when there is one.

**Arguments** have one shape for every tool: path and query parameters at the
top level, the request body under `body`. For example, a budget:

```json
{
  "operation_id": "budgetCreate",
  "arguments": {
    "body": {"scope": "global", "daily_usd": 5, "action_on_exceed": "throttle_zero"}
  }
}
```

**`dry_run`.** Every operation that is not a read accepts `dry_run: true`. The
arguments and the token's permission are checked and the request that would be
sent is shown; nothing is sent and nothing changes. A conflict the relay finds
only while it runs (a name already taken) shows up on the real call.

**`confirm`.** An operation that removes something (a `DELETE`, and the few
others marked destructive, such as `providerModelsSync`) needs `confirm`. The
value is the last path parameter of the operation (`confirm: "zai"` for
`/ai/providers/zai`); for an operation without a path parameter it is the
operation id. `burrow_describe_operation` names it in its `confirm` field. A
call with another value is refused and nothing is sent.

**Results.** A result is a JSON object, as `structuredContent` and as text. A
JSON array from the API comes as `{"items": [...]}`. A result above 64 KiB
(65536 bytes) is cut: it says `truncated`, how many items there were, and which
query parameters narrow the answer. Use those. An answer that is not JSON comes
as `{"text": "..."}`.

**Errors** are results with `isError` set. A call the API refuses carries the
API's status and message, for example `409 Conflict: burrow_login requires a
configured auth_domain`. Arguments that do not fit the schema are refused before
anything is sent, with the name of the argument.

A call leaves the rows the same change leaves through the dashboard or the REST
API, under the same user, and next to them one `mcp.tool.call` row. The row
names the curated tool, or `burrow_call_operation/<operationId>` for a generic
call, with the outcome (`ok`, `error`, `denied` or `dry_run`), the client's
address and user agent (each cut to 256 bytes), and the credential that made
the call: `token_id` for an automation token, `oauth_grant_id` for an OAuth
connection. The rows the API writes for the call carry the same `token_id` or
`oauth_grant_id` in their payload. Arguments and results are not recorded. See
**Settings → Audit log**.

### What an agent cannot do {#not-tools}

Some operations are never tools, whatever the token holds: an agent can be
steered by text it reads (a tool result, a model answer, a sync error), and
these writes would let such text re-route a credential or switch off
inspection, widen access or plant secrets and instructions. They are:
creating and changing a provider and its upstream, binding a service's
upstream credential, the SMTP settings, custom domains, webhooks, the
guardrail and redaction settings and rules, a service's AI config, the
retention settings, a service's gateway-only switch, creating or changing a
tool source or approving its tools, registering an OAuth client, creating a
service (it sets the access mode), a service's or tunnel's access mode, a
tcp service's settings (its PROXY protocol header and idle timeout), changing a gateway key, minting a client token or a
service API key, creating, changing and deleting users and roles, and
creating or changing a synthetic model (its `system_text` reaches other
users' requests). Their reads stay tools. Make these changes in the dashboard
or with the REST API. The full list is in the
[API reference](../reference/api.md).

::: warning Text from upstreams reaches the agent
Some answers carry text that a tool source's upstream or a model wrote, and a
model may follow instructions hidden in it. Burrow limits this where it can:
for a token, `last_sync_error` of a tool source and the message of a failed
sync are cut to a fixed class (`upstream error`, `upstream unreachable`,
`tools skipped` and the like), and the names of skipped tools are withheld;
the dashboard shows the full text. Captured request and response bodies
(`inspector_request_get`) and audit rows are returned as they are, because
reading them is what those tools are for. A token that holds `inspector:read:own`,
`inspector:read:any` or `audit:read` can therefore hand upstream text to the model. Leave those
permissions off a token for an agent that does not need them.
:::

::: warning Secrets end up in the conversation
An operation that creates a key or a token returns the secret in its result.
The client shows it to the model, and it stays in the transcript. Use these
where that is acceptable, or leave their permission off the token.
:::

---

## Limits {#limits}

| | |
|---|---|
| Requests per token | 300 per token and minute. Beyond that: `429` with `Retry-After`. |
| Requests per address | 1200 per client address and minute, counting only requests that carry an `Authorization: Bearer` header. Beyond that: `429` with `Retry-After`. Agents behind one NAT share it. |
| Request size | 1 MiB. Beyond that: `413`. Answered after the token is checked: a request without a valid token gets `401` first. |
| Batches | A JSON-RPC batch (a request body that is an array) is refused with `400`. Send one request per POST. Also answered after the token is checked. |
| Tool call duration | A call is ended after 30 seconds; the result is the error `timed out`. |
| Result size | 64 KiB of JSON per result; longer answers are cut and say so. |
| Protocol | MCP revisions `2026-07-28` (stateless, no handshake), `2025-11-25`, `2025-06-18` and `2025-03-26` over Streamable HTTP. An `initialize` for `2024-11-05` is answered with a revision the server offers. Not the older HTTP+SSE transport, not stdio. |
| Sessions | None. Every request stands alone; `Mcp-Session-Id` is ignored and there is no event stream to hold open. |
| Features | Tools only. No resources, no prompts. Every call is an API call under the same permissions and the same audit as the REST API. |
| Sign-in | An automation token in a header, or an OAuth access token issued for `/mcp/burrow`. The JSON API itself never accepts an OAuth token. |

### Browsers

A `POST` or `DELETE` that is not same-origin is refused with `403` before the
token is looked at, and no CORS header is sent. That is every request with an
`Origin` header other than the relay's own (a sibling subdomain included) and
every request with `Sec-Fetch-Site: same-site` or `cross-site`. Command-line and desktop clients send no
`Origin` and are unaffected. A client that sets an `Origin` header itself must
use the relay's own origin.

### Errors a tool call returns

A failed tool call is a normal result with `isError` set, and a text the model
can read:

| Text | Meaning |
|------|---------|
| `invalid argument <name>: …` | An argument is missing or has the wrong type. The argument's name is given, never its value. |
| `unknown operation <id>; find it with burrow_find_operations` | The operation does not exist, or the token may not call it. |
| `confirm must be "<value>" to …; nothing was sent` | A deleting operation without the right `confirm`. |
| `<status> <reason>: <message>` | The API refused the call, for example `403 Forbidden` or `409 Conflict`. |
| `the relay is starting; try again` | The relay has not finished starting. |
| `timed out` | The call took longer than 30 seconds. |
| `internal error` | Anything else. The cause is in the relay's log, never in the answer. |

A curated tool the token may not call is answered exactly like a tool that does
not exist: `unknown tool`.

---

## Coming from earlier versions {#upgrade}

The twelve hand-written tools are gone. The tools are built from the API's
OpenAPI description now, so every operation is reachable. Replace the old names
like this:

| Old tool | Now |
|----------|-----|
| `tunnels_list` | `burrow_call_operation` with `tunnelsList` |
| `services_list` | `services_list` |
| `services_get` | `service_get` |
| `services_set_access_mode` | none: the dashboard or the REST API (`PUT /api/v1/services/{id}/access-mode`) |
| `api_keys_list` | `burrow_call_operation` with `serviceApiKeysList` |
| `api_keys_create` | none: the dashboard or the REST API |
| `api_keys_delete` | `burrow_call_operation` with `serviceApiKeyDelete` |
| `tokens_list` | `burrow_call_operation` with `clientTokensList` |
| `tokens_mint` | none: the dashboard or the REST API |
| `users_list` | `burrow_call_operation` with `usersList` |
| `audit_search` | `audit_events_list` |
| `metrics_snapshot` | `burrow_call_operation` with `metricsGet` |

Arguments changed shape too: path and query parameters at the top level, a body
under `body`. Lists come back as `{"items": [...]}`. Ask
`burrow_describe_operation` for the exact arguments.

### `BURROW_MCP_LISTEN`

Earlier versions had a separate listener (`BURROW_MCP_LISTEN`, usually `:7800`)
that no MCP client could connect to. It is gone; nothing listens on 7800.

- `BURROW_MCP_LISTEN`, `BURROW_MCP_TOKEN` and their `_FILE` forms are still
  accepted and do nothing. The relay logs one warning at start naming the ones
  that are set, never a value. A `_FILE` path is not read and need not exist.
  Remove them, and remove a published port `7800`.

---

## Troubleshooting {#troubleshooting}

| What you see | Why | What to do |
|--------------|-----|------------|
| `401` with `WWW-Authenticate: Bearer` | No token, a mistyped or revoked token, an expired token, or the token's user is suspended. The client token of `burrow connect` and a gateway key do not work here. | Mint a new automation token. |
| Connected, but only `me_get` and the three `burrow_…` tools | The token was minted with an empty permission list, or your role no longer grants what is on it. | Mint a token [with permissions](#token). |
| A tool is missing | The token lacks the permission the tool needs, or the owner's role no longer grants it. | Call `me_get` to see the permissions the token holds; mint a token with the missing one. |
| `unknown operation <id>` | The operation does not exist, or the token may not call it (the same answer for both). | Find it with `burrow_find_operations`; if it is not listed, the token lacks its permission. |
| `confirm must be "…"` | A deleting operation was called without the value it names. | Pass `confirm` with the value in the message; `burrow_describe_operation` shows it. |
| `the relay is starting; try again` | The relay answers MCP before its API is ready. | Try again in a few seconds. |
| `400 batch requests are not supported` | The body is a JSON-RPC array. | Send one request per POST. |
| `413` | The body is larger than 1 MiB. | Send less. |
| `403 cross-origin request refused` | The `POST` or `DELETE` is not same-origin: an `Origin` other than the relay's own (also a sibling subdomain), or `Sec-Fetch-Site: same-site` or `cross-site`. | Use a client that runs outside the browser, or one that sends no `Origin` or the relay's own. |
| `429` | More than 300 requests in a minute with one token, or 1200 from one address. | Wait for `Retry-After`; give each agent its own token. |
| The client gets the dashboard's HTML | The URL is not exactly `/mcp/burrow` (a trailing slash, another path), or a reverse proxy sends that path somewhere else. | Use the exact path; forward it to the same place as the dashboard. |

## Related pages

- [Access control & security](/guide/access-control) — access modes and API keys
- [Configuration](/guide/configuration) — full `BURROW_*` env var reference
- [HTTP API](/reference/api) — the REST API the tools are built on
