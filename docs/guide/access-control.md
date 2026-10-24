# Access control & security

Burrow lets you decide, per service, who can reach your tunnelled upstream. Four
access modes cover everything from "anyone can call this webhook" to "only my
mTLS client gets in." This page explains each mode, how to configure it, and a
short security checklist for hardening your relay.

---

## Access modes

| Mode | Who can reach the upstream |
|------|---------------------------|
| `open` | Everyone — no authentication |
| `api_key` | Callers presenting a valid API key |
| `burrow_login` | Burrow users who are logged in to the dashboard |
| `mtls` | Clients presenting a trusted TLS client certificate |

### `open`

No gate. The relay forwards every request to your upstream without checking
anything. Use it for public-facing services (static sites, webhooks that carry
their own HMAC signature, etc.).

### `api_key`

The relay validates an API key on every inbound request before forwarding.
Keys are minted per service and stored hashed in the database.

**When to use:** machine-to-machine calls, CI/CD pipelines, or any non-browser
client that can send a custom header.

### `burrow_login`

The relay checks that the caller has an active Burrow session cookie. Requests
without a valid session are redirected to the `/__burrow/login` gate. This is
session-based authentication against the same user directory that drives the
dashboard. After sign-in the visitor returns to the service on `/svc/<slug>/`
only; over the host-routed ingress the visitor lands on the dashboard root.
Over the opt-in host-routed ingress `burrow_login` is therefore not supported.

With a [services domain](/guide/configuration#services-domain)
(`BURROW_SERVICES_DOMAIN`) the dashboard's session cookie never reaches the
services host, and does not count there. The service needs a session of the
services host instead, in the host-only cookie `__Host-burrow_svc_session`
(the `__Host-` prefix means it cannot carry a Domain, so a page on another
host of the site cannot set it):

1. The gate on the services host gives the browser a short-lived nonce cookie
   of this sign-in's own (`__Host-burrow_svc_nonce_<id>`, two minutes; two
   tabs that sign in at once each keep theirs) and sends the visitor to
   `https://<dashboard>/__burrow/handoff?return=<service address>`.
2. The dashboard's host asks for the password if the visitor is not signed in
   to the dashboard, then issues a one-time code (60 seconds, single use, bound
   to the user, the return address and that browser's nonce) and sends the
   browser back to `https://<services domain>/__burrow/callback`.
3. The services host exchanges the code for its own session, but only in the
   browser holding the nonce: a callback link passed to somebody else is
   refused. A previous services session of that browser is ended. The visitor
   goes on to the service. Each handoff is audited as `auth.handoff`.

No password is ever typed on the services host. Signing out there ends the
services session only. The services session belongs to the dashboard session
that handed it off: signing out of the dashboard, or revoking that session
under Sessions, ends it too, and it never lasts longer than that session.
"Sign out everywhere else" ends every services session. The dashboard does not
accept a services session, and the services host accepts nothing else.

**When to use:** internal tools that your team members access via a browser
after logging in to `https://burrow.insingo.com`.

### `mtls`

Requires the opt-in host-routed ingress; not available with path URLs.

Mutual TLS. The relay demands a client certificate signed by a CA you upload.
The TLS handshake itself rejects any connection without a valid cert — the
access check is defense-in-depth.

**When to use:** service-to-service calls where you control both ends and want
cryptographic identity rather than shared secrets.

::: warning mTLS requires the host-routed ingress
mTLS requires the opt-in host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`) and is
not available with path URLs. The `https://burrow.insingo.com/svc/<slug>/`
endpoint shares the dashboard TLS connection and **cannot** carry a per-service
client certificate. The dashboard has no mTLS controls: set the mode through
the REST API. Without the host-routed ingress the API refuses `mtls`.
:::

---

## Access modes of TCP services

The four modes above belong to HTTP services. A TCP service has three others,
and a TCP tunnel is **not** open by default when a current client creates it:

| Mode | Public port | Who can reach the service |
|------|-------------|---------------------------|
| `private` | none | Signed-in Burrow clients that run `burrow forward`: the owner, and roles the owner allows |
| `ip` | yes | Permanent CIDRs plus time-limited grants; everyone else is closed without a byte |
| `open` | yes | Everyone, with connection limits |

A current client (`burrow tcp`, or a `burrow.yaml` entry) creates a new
service `private` when no mode is asked for; a `burrow.yaml` entry with
`remote:` and no `access:` is created `open`. A tcp service from a client older
than v0.13.0 is `open`. The mode of an existing service is set on its page in
the dashboard, or with `PUT /api/v1/services/{id}/access-mode`. A change closes
the tunnel and the client registers again: a client started without `--access`
serves the new mode, and one started with a stricter `--access` stops. The full guide is
[Remote desktop and other TCP services](/guide/remote-desktop-and-tcp).

**Forward policy.** Who may run `burrow forward` to a TCP service is the
service's access policy (`PUT /api/v1/services/{id}/access-policy`): the owner
always, and any user whose role is on the list. An empty list means the owner
only. The list is read on every forward, and a change ends the live forwards
of the users it no longer allows, as does a change of a user's role or a
suspension. The owner's forwards are never ended this way.

**IP grants.** In `ip` mode, `POST /api/v1/services/{id}/ip-grants` allows an
address for 1 to 168 hours (the dashboard offers 1, 8 and 24); `GET` lists the
active grants and `DELETE /api/v1/services/{id}/ip-grants/{grantID}` revokes
one. Permanent CIDRs are the IP/geo allow list of the service; for a TCP
service no entry may be wider than a `/8` (IPv4) or `/32` (IPv6).

**Country rules.** The country allow and block lists of a service, http or tcp
(mode `ip` and `open`), need `BURROW_GEOIP_DB`, a GeoLite2-Country file.
Without it an allow list denies everyone and a country cannot be added. An
unknown country follows the list type (allow list: denied, block list:
allowed). Forwards are not checked. Country lists that were saved earlier for
tcp services now apply. See
[Country rules](/guide/remote-desktop-and-tcp#country-rules).

**Bans, limits and host names.** A tcp service can also ban abusive sources
automatically, limit bandwidth and daily volume, and be reached by host name
through TLS passthrough; see the
[TCP guide](/guide/remote-desktop-and-tcp#automatic-bans).

**Password confirmation.** Changing the access mode, the access policy or the
IP/geo lists, adding a grant and disconnecting a live connection ask a
dashboard session for a recent password confirmation, while no services
domain is set. Requests with an automation token (`bua_…`) skip it: they carry
no cookie that a page could ride on. So such a token, with
`services:configure` or `ipgeo:manage`, can widen access to a service on its
own; keep those permissions off tokens that do not need them. These routes
are not tools of the management MCP server, whatever token it runs with: an
agent cannot widen access to a service.

**Failed sign-ins on port 7000.** After 10 failed sign-ins from one address,
the relay answers further invalid tokens from it with `rate_limited` instead
of `invalid_token`, until one more is allowed every 30 seconds. Every token is
still checked, and a valid token is accepted at once. It is a signal to the
client and in the logs, not a gate: it does not slow down guessing, which the
length of a random token rules out.

**Permissions.** No new permission exists. The mode, the forward policy, the
connection cap and the live connections use `services:configure:own` or `:any`;
the CIDR list and the grants use `ipgeo:manage:own` or `:any`; reading the
connection log and the audit log uses `audit:read`.

**Audit actions.** `service.tcp_access.update`, `ipgeo.update`,
`service.tcp_settings.update`, `service.tcp_hostname.update`,
`service.tcp_hostname_only.update`,
`service.tcp_limits.update`, `tcp.ban.add`, `tcp.ban.remove`,
`service.ip_grant.add`, `service.ip_grant.revoke`,
`service.max_connections.update`, `service.connection.close`,
`service.tcp_port.reclaimed` and `tcp.forward.open` (one row per user and
service per minute; the connection log holds every forward as a `tcp_forward`
row). Refusals are `rejected` rows in the connection log and the
`access.denied` webhook event.

---

## Configuring the access mode

### Dashboard

Open the service detail page, click **Configure**, and select the mode from the
**Access** dropdown.

### REST API

```sh
PUT /api/v1/services/{id}/access-mode
```

```sh
# Set to api_key
curl -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-mode \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"access_mode": "api_key"}'

# Set to open
curl -X PUT https://burrow.insingo.com/api/v1/services/abc123/access-mode \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"access_mode": "open"}'
```

The token needs `services:configure:own` for a service of its own user, or
`services:configure:any` for any service.

---

## API key details

### Minting a key

Via the dashboard (service detail page → **API Keys** → **Add key**), or via
the API:

```sh
POST /api/v1/services/{id}/api-keys
```

```sh
curl -X POST https://burrow.insingo.com/api/v1/services/abc123/api-keys \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "ci-pipeline"}'
```

The token needs `services:configure:own` or `services:configure:any`.

The response body includes the raw key value. Copy it — it is shown only once.

### Sending the key — default (`Authorization: Bearer`)

By default, callers send the key in the standard `Authorization` header:

```sh
curl https://burrow.insingo.com/svc/k7p2qx/api/data \
  -H "Authorization: Bearer buk_YOUR_API_KEY"
```

The relay strips the `Bearer ` prefix before validating.

### Sending the key — custom header (`api_key_header`)

You can configure a custom header name (e.g. `X-Api-Key`) in the service
settings. When a custom header is set, callers send the **raw key value** with
no prefix:

```sh
curl https://burrow.insingo.com/svc/k7p2qx/api/data \
  -H "X-Api-Key: buk_YOUR_API_KEY"
```

::: info No `Bearer` prefix on custom headers
The relay calls `strings.TrimSpace(value)` directly on the custom header value.
Do **not** include `Bearer ` — the call will fail with `401 invalid api key`.
:::

### Error responses

| Situation | Status | Body |
|-----------|--------|------|
| Header missing entirely | `401` | `{"error":"missing api key"}` |
| Header present but key invalid | `401` | `{"error":"invalid api key"}` |

---

## Users, roles and automation tokens

Everything above is about who may reach a tunnelled service. This section is
about who may operate the relay itself: the dashboard and the JSON API under
`/api/v1`.

### Roles

Every user has one role. Two are built in:

| Role | Holds |
|------|-------|
| `admin` | every permission |
| `user` | `tunnels:read:own`, `tunnels:manage:own`, `tokens:manage:own`, `services:configure:own`, `sessions:manage:own`, `ai:read:own`, `ai:configure:own`, `quotas:read:own`, `inspector:read:own`, `inspector:replay:own`, `automation:tokens:manage:own`, `mtls:manage:own`, `ipgeo:manage:own` |

A role's permissions bind everything its users do, in the dashboard as much as
through an automation token: every route of the API asks for a permission, and
a dashboard session holds what its user's role holds. The table under
[Automation tokens](#automation-tokens) says which permission each area asks
for. A role that lacks it gets `403`. A role without any permission can sign
in, read its own account (`GET /api/v1/me`), change its password and sign out,
and nothing else: it gets no client token and no gateway key.

An admin can define **custom roles** (dashboard: **Settings → Roles**; API:
`POST /api/v1/roles`) from the permission catalog
(`GET /api/v1/roles/permissions`), give one to a user, and name one in a
service's `burrow_login` access policy:

```sh
# Give a user the custom role "auditor"
curl -X PATCH https://burrow.insingo.com/api/v1/users/USER_ID \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"role": "auditor"}'
```

A custom role should include `sessions:manage:own`, otherwise its users cannot
list or end their own browser sessions (the dashboard then leaves **Sessions**
out of Settings), `automation:tokens:manage:own` if its users make automation
tokens, and at least one of the read
permissions `tunnels:read:own`/`:any`, `services:configure:own`/`:any`,
`ai:read:own`/`:any` or `inspector:read:own`/`:any`, which lets the dashboard
refresh live.

The dashboard's user dialogs offer the built-in roles and, to someone who may
read roles (`roles:read` or `roles:manage`), every custom role with its
description. A role name that is neither built in nor an existing custom role
answers `400`.

The dashboard follows the permissions, not the role name: a page or a button
whose request the role may not make is left out, and a page opened by link
says which permission is missing.

Four rules keep roles from becoming a way around the permissions:

- **Nobody hands out what they lack.** You can give a user only a role whose
  permissions your own role holds, and write into a role only permissions your
  own role holds. A role with `users:manage` cannot make anyone an admin. An
  automation token is measured by what it effectively holds (see below), not
  by the role of the user who minted it: a token that provisions users must
  itself declare every permission of the roles it hands out.
- **Nobody manages what is stronger than themselves.** Changing, suspending or
  deleting a user requires holding every permission of the role that user holds
  now. Editing or deleting a role requires holding what the role holds now. An
  admin passes everywhere. A custom "user manager" role therefore cannot touch
  an admin account, and a role editor cannot shrink a role stronger than its
  own in order to manage its holders.
- **A role in use is not deleted.** `DELETE /api/v1/roles/{name}` answers
  `409` while a user holds the role or an access policy names it, and says how
  many. Move the users to another role and take the role out of the policies
  first.
- **A permission ending in `:own` is for your own resources**, `:any` for
  everyone's. This holds for an automation token as well: an admin's token that
  declares only `:own` permissions stays with its user's own services, tokens
  and lists. One exception: rate limits are not tied to a user. With
  `quotas:read:own`, `GET /api/v1/rate-limits` lists every configured limit,
  and `GET /api/v1/rate-limits/usage` answers for any subject of the scopes
  `api_key`, `role` and `service`. Only the usage of a model, and of a gateway
  key that is not yours, takes `quotas:read:any`.

::: warning Three permissions are as good as admin rights
`backup:run`, `ai:providers:manage` and `settings:manage` reach secrets or the
whole database. A backup download is the whole database. A provider can be
pointed at any address, and a model sync then sends a stored upstream
credential there. A test email sends the SMTP password to the host the
settings name. Give these three only to people and tokens you would give admin
rights. Restoring a backup replaces the database and takes every permission,
whatever else the caller holds.
:::

### Automation tokens

An automation token (`bua_…`, minted under **Settings → Automation tokens** or
with `POST /api/v1/automation/tokens`) acts for the user who minted it. It carries a
fixed list of permissions, chosen when it is minted.

A token may call a route only if **both** hold:

1. the token declares one of the permissions the route asks for, and
2. the role of the token's user holds that permission **now**.

Together these are the token's **effective permissions**. So a token never does
more than it declares, and it loses a permission the moment its user's role
loses it. When a token hands something out (a role, a permission in a role, a
new token), the effective permissions decide. A token cannot mint a token with
a permission it does not declare itself, nor one that outlives it: a token
with an expiry mints only tokens that expire no later than itself. Only a
successor that is to outlive its parent needs a parent without an expiry. The
permissions of a token cannot be changed: mint a new one and revoke the old one.

Mint a token with the permissions it needs through the API:

```sh
curl -X POST https://burrow.insingo.com/api/v1/automation/tokens \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "ci-deploy",
        "permissions": ["services:configure:own", "tokens:manage:own"],
        "expires_at": "2027-01-31T00:00:00Z"
      }'
```

`name` is required. `permissions` may hold only what your role holds (and, when
a token mints, what that token declares). Left out (or `null`), the new token
inherits its minter's permissions at that moment: everything the role of a
dashboard session holds, or exactly the effective permissions of a minting
token. `"permissions": []` declares nothing. `expires_at` is optional; without it the token does not expire. Any
other field answers `400`. The answer carries the token once, in `plaintext`.

The dashboard's **New automation token** dialog asks for a name, an expiry and
the access: a preset (Read-only, Operator, Full) or Custom, with the list of
permissions to change; the ones you do not hold are shown but cannot be ticked.
It sends that list as `permissions`. Against a relay without
`GET /api/v1/automation/presets` it asks for a name and an expiry only, and the
token inherits your permissions as they are at that moment.

::: warning Upgrading to v0.10: tokens are held to their permissions
Before v0.10 a token minted by an admin passed most checks whatever it
declared, and many routes asked a token for nothing at all. From v0.10 on
every route asks. A token that worked before and now answers

```json
{"error": "services:configure:own or services:configure:any required"}
```

with status `403` is missing one of the permissions named in the message.
Mint a new token that declares it, then revoke the old one.

To see what your existing tokens declare, list them:
`GET /api/v1/automation/tokens` answers each token with its `permissions`. It
takes `automation:tokens:manage:own` or `:any`, which both built-in roles
hold. The dashboard's **Automation tokens** page names the preset a token's list
matches, or says Custom with the number of permissions.
:::

What each area asks for. A dashboard session needs a role that holds one of
the permissions; a token must declare it as well:

| To … | Permission |
|------|------------|
| read or change services, their API keys, access mode, access policy, slug and custom domains | `services:configure:own` or `services:configure:any` |
| pre-provision a service (`POST /services`) | `services:configure:any` |
| read or change a service's IP/geo lists, read the geo status | `ipgeo:manage:own` or `ipgeo:manage:any` |
| read or change a service's AI config and upstream credential, clear its cache | `ai:configure:own` or `ai:configure:any` |
| read captured requests of a service / replay them | `inspector:read:own` or `:any` / `inspector:replay:own` or `:any` |
| read AI providers | `ai:read:own`, `ai:read:any` or `ai:providers:manage` |
| create, change or delete AI providers and their model catalogs | `ai:providers:manage` (reaches the upstream credentials: give it like admin rights) |
| read tool sources and their tools (`GET /mcp/sources`, `GET /mcp/sources/{slug}/tools`) | `mcp:sources:read` or `mcp:sources:manage` |
| create, change, sync or delete tool sources, approve their tools and change tool overrides (a source sends a stored upstream credential to the address it names: give it like admin rights). A tunnel source also needs the right to configure the service it binds | `mcp:sources:manage` |
| read synthetic models, the gateway's endpoints, cache, redaction and guardrail settings | `ai:read:own`, `ai:read:any`, `ai:configure:own` or `ai:configure:any` |
| change synthetic models, cache, redaction and guardrail settings | `ai:configure:any` |
| list your own gateway keys / revoke one of them | `ai:read:…` or `ai:configure:…` / `ai:configure:own` or `ai:configure:any` |
| read rate limits and usage / change rate limits | `quotas:read:own` or `quotas:read:any` / `quotas:manage:any` |
| read the cost summary and export | `quotas:read:any` |
| read the pricing table / change it | `quotas:read:own`, `quotas:read:any` or `cost:pricing:manage` / `cost:pricing:manage` |
| read and manage budgets | `budgets:manage` |
| read the audit log and the connection logs | `audit:read` |
| manage webhooks | `webhooks:manage` |
| run, list, download, verify and delete backups | `backup:run` (a download is the whole database: give it like admin rights) |
| restore a backup (`POST /backups/restore`) | every permission there is: a role that holds them all (the admin role does) and, for a token, one that declares them all |
| read `/metrics`, the database status and the API viewer | `metrics:read` |
| read the MCP status and tool list | `mcp:tools:read` |
| list, create and revoke your own client tokens, read the connect info | `tokens:manage:own` or `tokens:manage:any` |
| list and end your own browser sessions | `sessions:manage:own` or `sessions:manage:any` |
| list your own live tunnels / the clients of everyone | `tunnels:read:own` or `tunnels:read:any` / `tunnels:read:any` |
| manage automation tokens | `automation:tokens:manage:own` or `automation:tokens:manage:any` |
| read roles / create, change and delete custom roles | `roles:read` or `roles:manage` / `roles:manage` |
| read the permission catalog (`GET /roles/permissions`) | `roles:read`, `roles:manage` or `automation:tokens:manage:own` / `:any` |
| read users / create, change and delete users | `users:read` or `users:manage` / `users:manage` |
| read and change settings | `settings:manage` (reaches the SMTP password: give it like admin rights) |

Three routes are about the caller's own account and open to every signed-in
caller, whatever its role holds or its token declares: `GET /api/v1/me`,
`POST /api/v1/auth/change-password` (which asks for the current password) and
`POST /api/v1/auth/logout`. Signing in, the client's side of `burrow login`,
client discovery and the API description (`/api/v1/openapi.yaml`, `.json`)
need no credentials at all. Everything else asks for a permission.

Some things take a person in the dashboard, whatever a token declares. The
role of that person needs the permission all the same:

| In the dashboard only | The role holds |
|-----------------------|----------------|
| create a gateway key | `ai:configure:own` or `ai:configure:any` |
| look at, approve or deny a client sign-in (`burrow login`) | `tokens:manage:own` or `tokens:manage:any` |
| read the attempt log of a gateway request | `ai:read:any` |
| the refresh stream of the dashboard pages (`GET /api/v1/events`, sessions only) | `tunnels:read:own`/`:any`, `services:configure:own`/`:any`, `ai:read:own`/`:any` or `inspector:read:own`/`:any` |
| the live stream of a service's request inspector | `inspector:read:own` or `inspector:read:any` |

Setting `gateway_only` when creating an AI provider (`POST /api/v1/ai/providers`)
needs permission to configure the service as well. Without it the request
answers `403`; before v0.10 it answered `201` and left the flag off.

### Revoking a client token

Revoking a client token (`DELETE /api/v1/tokens/{id}`, or **Revoke** in the
dashboard) disconnects every client that is connected with it at once; its
tunnels go down with it. The same happens to all clients of a user who is
suspended or deleted. A suspended user's clients cannot reconnect until the
user is active again.

---

## Security checklist

### Firewall

Open only the ports Burrow needs. With ACME (recommended):

| Port | Purpose |
|------|---------|
| `80` | ACME HTTP-01 challenge + HTTPS redirect |
| `443` | Dashboard, REST API, and path-routed tunnels |
| `8443` | Host-routed ingress (opt-in, off by default; mTLS) |
| `7000` | Client control channel (TLS) |

If you use raw TCP tunnels in mode `open` or `ip`, also open the specific port
you pin in `remote:` (e.g. `9001`). No need to open the full `9000`–`9100`
range unless you expose many dynamic TCP services. `private` services need no
port.

::: warning Keep `8443` closed unless you enable the host-routed ingress
`BURROW_HTTP_PROXY_LISTEN` is empty by default, so nothing listens on `8443`.
If you turn it on, restrict the port at the network level to known source IPs
unless you rely on it for mTLS.
:::

### Strong admin password

Never leave the default or a weak password on the admin account. Pass the
secret via an environment file or Docker secret — not a shell variable:

```env
BURROW_ADMIN_EMAIL=admin@burrow.insingo.com
BURROW_ADMIN_PASSWORD_FILE=/run/secrets/burrow-admin-password
```

The `_FILE` form reads the value from the file at startup. See the
[Configuration reference](/guide/configuration) for all `_FILE`-capable vars.

### Keep the image updated

The `develop` channel rebuilds on every push. Pull regularly to pick up
security fixes:

```sh
docker pull ghcr.io/andreas-koehn/burrow:develop
docker compose up -d
```

### Back up the database

Burrow stores all configuration, users, and audit logs in a single SQLite file
(`burrow.db` by default). Take regular backups:

```sh
# CLI backup (writes a timestamped archive to BURROW_BACKUP_DIR)
burrowd backup

# Or via the API
curl -X POST https://burrow.insingo.com/api/v1/backups \
  -H "Authorization: Bearer bua_YOUR_AUTOMATION_TOKEN"
```

The token needs `backup:run`.

See [Operations](/guide/operations) for restore instructions and retention
settings.

### Secure cookies

If TLS is terminated by an upstream proxy rather than by `burrowd` itself, set:

```env
BURROW_HTTP_SECURE_COOKIES=true
BURROW_TRUSTED_PROXIES=10.0.0.1/32
```

Without `BURROW_HTTP_SECURE_COOKIES=true` the session cookie will not carry the
`Secure` flag, which means browsers will refuse to send it on HTTPS pages.

With Secure cookies the dashboard's cookies are named `__Host-burrow_session`
and `__Host-burrow_csrf`. Browsers accept a `__Host-` cookie only with
`Secure`, `Path=/` and no `Domain`, so a page on another host of the same site
(the services host, a tunnelled app on a sibling name) cannot set or shadow
the dashboard's session with a `Domain=` cookie. Over plain http the names are
`burrow_session` and `burrow_csrf`. A session signed in before v0.12.1 keeps
working under its bare name until it expires; a bare session created after
the first start of v0.12.1 is refused (see
[Upgrading to v0.12.1](/guide/operations#upgrade-v0-12-1)). The next sign-in
writes the new names and clears the old ones. The sign-in forms (the dashboard's, the
gate's and the handoff's) and the password confirmation refuse a request
another origin sent (login CSRF).

::: tip ACME handles TLS for you
With `BURROW_ACME_DOMAIN` set, `burrowd` terminates TLS itself. You do **not**
need `BURROW_HTTP_SECURE_COOKIES` or a reverse proxy for HTTPS — the cookie
flag is set correctly by default.
:::

---

## Related pages

- [Deploy on a server](/guide/deploy) — firewall setup and ACME configuration
- [Expose services](/guide/expose-services) — tunnel types and URL shapes
- [Remote desktop and other TCP services](/guide/remote-desktop-and-tcp) — private, ip and open modes
- [Configuration](/guide/configuration) — full `BURROW_*` env var reference
- [Operations](/guide/operations) — backup, restore, and log settings
