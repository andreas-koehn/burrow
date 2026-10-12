# Manage Burrow from an agent (MCP)

Burrow has a built-in [Model Context Protocol](https://modelcontextprotocol.io)
server. An MCP client such as Claude Code, Cursor or VS Code connects to it and
can then list tunnels and services, change a service's access mode, create and
delete API keys, mint client tokens, read users, search the audit log and read
the metrics.

The endpoint is part of every relay. There is nothing to switch on.

```
https://<your-relay>/mcp/burrow
```

It is on the same address as the dashboard, so a relay that is reachable in the
browser is reachable for an MCP client. Use exactly this path, without a
trailing slash: with the dashboard present, `POST /mcp/burrow/`, any other
sub-path and any method other than GET, POST and DELETE on the path are
answered with the dashboard's HTML, not with an error. `GET` and `DELETE` on
`/mcp/burrow` are answered with `405` and `Allow: POST`: there is no event
stream and no session to end.

---

## 1. Mint a token {#token}

The endpoint takes an **automation token** (`bua_…`) in the `Authorization`
header. The session cookie of the dashboard is not accepted.

A token carries a list of permissions. Over MCP it can see and call exactly the
tools whose permission is **on that list and granted by your current role**. A
tool outside that set is not shown, and calling it is answered as if it did not
exist.

::: warning Mint the token through the API
A token created on the dashboard page **Settings → Automation tokens** declares no permission, so it has an
empty permission list. It is accepted, and it sees no tools. Until the page
lets you pick permissions, mint the token with the call below.
:::

Log in, then create the token with the permissions the agent should have:

```sh
curl -c cookies.txt -X POST https://burrow.insingo.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"changeme"}'

curl -b cookies.txt -X POST https://burrow.insingo.com/api/v1/automation/tokens \
  -H "X-CSRF-Token: $(grep burrow_csrf cookies.txt | awk '{print $NF}')" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "claude-code",
        "permissions": ["tunnels:read:any", "audit:read", "metrics:read"]
      }'
```

The answer holds the token once, in `plaintext`. Store it now; it cannot be
read again. Add `"expires_at": "2027-01-01T00:00:00Z"` to the body for a token
that ends on its own.

You can only put permissions on a token that your own role has. Three useful
sets:

| Purpose | `permissions` |
|---------|---------------|
| Look, do not touch | `tunnels:read:any`, `users:read`, `audit:read`, `metrics:read` |
| Operate services | the above plus `services:configure:own` |
| Everything the tools offer | the above plus `tokens:manage:own` |

`services:configure:own` covers reading services and changing them; there is no
read-only permission for services yet. If your role is later reduced, the
token is reduced with it at once.

---

## 2. Connect a client {#clients}

Replace the address and the token in the examples.

### Claude Code

```sh
claude mcp add --transport http burrow https://burrow.insingo.com/mcp/burrow \
  --header "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN"
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
works: point it at the URL and add `Authorization: Bearer bua_…`. Clients that
can only sign in through OAuth cannot connect yet.

---

## Tools {#tools}

| Tool | What it does | Permission | Kind |
|------|--------------|------------|------|
| `tunnels_list` | Lists live tunnels; optional `filter` on the name | `tunnels:read:any` | read |
| `services_list` | Lists services | `services:configure:own` | read |
| `services_get` | One service with its access policy and key count | `services:configure:own` | read |
| `services_set_access_mode` | Sets `open`, `api_key` or `burrow_login` on a service | `services:configure:own` | write, repeatable |
| `api_keys_list` | Lists the API keys of a service, without the keys | `services:configure:own` | read |
| `api_keys_create` | Creates an API key for a service; **returns the key once** | `services:configure:own` | write |
| `api_keys_delete` | Deletes an API key | `services:configure:own` | **destructive** |
| `tokens_list` | Lists your own client tokens, without the tokens | `tokens:manage:own` | read |
| `tokens_mint` | Issues a client token for you; **returns the token once** | `tokens:manage:own` | write |
| `users_list` | Lists users | `users:read` | read |
| `audit_search` | Searches the audit log | `audit:read` | read |
| `metrics_snapshot` | The current metrics in Prometheus text format | `metrics:read` | read |

The "Kind" column is sent to the client as MCP tool annotations
(`readOnlyHint`, `destructiveHint`, `idempotentHint`), so a client can ask
before it runs a tool that changes or deletes something.

A tool with `:own` in its permission acts on what belongs to the token's user;
an admin's token reaches every service. `mtls` cannot be set through MCP: it
needs a CA certificate, which is uploaded through the dashboard or the REST
API.

::: warning Secrets end up in the conversation
`api_keys_create` and `tokens_mint` return a secret in their result. The client
shows it to the model, and it stays in the transcript. Use them where that is
acceptable, or leave their permission off the token.
:::

Every call is written to the audit log as `mcp.tool.call` with the tool and the
outcome (`ok`, `error` or `denied`), under the user the token belongs to. See
**Settings → Audit log**.

---

## Limits {#limits}

| | |
|---|---|
| Requests per token | 300 per token and minute. Beyond that: `429` with `Retry-After`. |
| Requests per address | 1200 per client address and minute, counting only requests that carry an `Authorization: Bearer` header. Beyond that: `429` with `Retry-After`. Agents behind one NAT share it. |
| Request size | 1 MiB. Beyond that: `413`. Answered after the token is checked: a request without a valid token gets `401` first. |
| Batches | A JSON-RPC batch (a request body that is an array) is refused with `400`. Send one request per POST. Also answered after the token is checked. |
| Tool call duration | A call is ended after 30 seconds; the result is the error `timed out`. |
| Protocol | MCP revisions `2026-07-28` (stateless, no handshake), `2025-11-25`, `2025-06-18` and `2025-03-26` over Streamable HTTP. An `initialize` for `2024-11-05` is answered with a revision the server offers. Not the older HTTP+SSE transport, not stdio. |
| Sessions | None. Every request stands alone; `Mcp-Session-Id` is ignored and there is no event stream to hold open. |
| Features | Tools only. No resources, no prompts. |
| Sign-in | A static token in a header. No OAuth. |

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
| `invalid arguments: <parameter>` | An argument is missing or has the wrong type. The parameter's name is given when known, never its value. |
| A fixed sentence, such as `invalid access mode` or `not found` | The input was wrong; the sentence says how. |
| `forbidden` | The token's user may not touch that object. |
| `timed out` | The call took longer than 30 seconds. |
| `internal error` | Anything else. The cause is in the relay's log (tool name, error types, a driver code), never in the answer. |

A tool the token may not call is answered exactly like a tool that does not
exist: `unknown tool`.

---

## Coming from `BURROW_MCP_LISTEN` {#upgrade}

Earlier versions had a separate listener (`BURROW_MCP_LISTEN`, usually `:7800`)
that no MCP client could connect to. It is gone; nothing listens on 7800.

- `BURROW_MCP_LISTEN`, `BURROW_MCP_TOKEN` and their `_FILE` forms are still
  accepted and do nothing. The relay logs one warning at start naming the ones
  that are set, never a value. A `_FILE` path is not read and need not exist.
  Remove them, and remove a published port `7800`.
- Tool names use an underscore where they had a dot: `tunnels.list` is now
  `tunnels_list`.
- Tools that list something return an object (`{"tunnels": [...]}`), not a bare
  array.

---

## Troubleshooting {#troubleshooting}

| What you see | Why | What to do |
|--------------|-----|------------|
| `401` with `WWW-Authenticate: Bearer` | No token, a mistyped or revoked token, an expired token, or the token's user is suspended. The client token of `burrow connect` and a gateway key do not work here. | Mint a new automation token. |
| Connected, but no tools | The token has no permission list (it was created in the dashboard), or your role no longer grants what is on it. | Mint a token [with permissions](#token). |
| `unknown tool` for a tool from the table | The token may not call it. | Add its permission to a new token. |
| `400 batch requests are not supported` | The body is a JSON-RPC array. | Send one request per POST. |
| `413` | The body is larger than 1 MiB. | Send less. |
| `403 cross-origin request refused` | The `POST` or `DELETE` is not same-origin: an `Origin` other than the relay's own (also a sibling subdomain), or `Sec-Fetch-Site: same-site` or `cross-site`. | Use a client that runs outside the browser, or one that sends no `Origin` or the relay's own. |
| `429` | More than 300 requests in a minute with one token, or 1200 from one address. | Wait for `Retry-After`; give each agent its own token. |
| The client gets the dashboard's HTML | The URL is not exactly `/mcp/burrow` (a trailing slash, another path), or a reverse proxy sends that path somewhere else. | Use the exact path; forward it to the same place as the dashboard. |

## Related pages

- [Access control & security](/guide/access-control) — access modes and API keys
- [Configuration](/guide/configuration) — full `BURROW_*` env var reference
- [HTTP API](/reference/api) — the REST API the tools are built on
