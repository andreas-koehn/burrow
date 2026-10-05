# Introduction

Burrow is a self-hosted tunnel relay. You run one server process (`burrowd`) on a
public machine; developers run a small client binary (`burrow`) on their
laptops or CI runners. The relay bridges the two, exposing local services on a
real public URL — without punching holes in firewalls or touching DNS records
for every service.

Everything is in two binaries. No sidecar agents, no third-party accounts, no
data leaving your infrastructure.

---

## Relay and client

| Component | Binary | Where it runs |
|-----------|--------|---------------|
| Relay | `burrowd` | Your public server |
| Client | `burrow` | Developer machine / CI |

**`burrowd`** opens several listeners (control channel, dashboard, proxy ingress)
and manages the tunnel database. **`burrow`** connects out to the relay over a
single TLS control channel on `:7000`, declares which local ports to expose, and
keeps the connection alive.

Because the client dials outward, there is nothing to configure on a NAT or
corporate firewall for outbound traffic.

---

## What is a tunnel?

A tunnel is a named, persistent mapping from a local address on your machine to
a public endpoint on the relay. Once the client is connected, traffic arriving
at the public endpoint is forwarded to your local process — live, in real time.

Tunnels come in two types.

---

## Tunnel types

### HTTP tunnels

The relay acts as a reverse proxy. Your local HTTP server is reachable at
`https://burrow.insingo.com/svc/<slug>/`. One A record and one certificate are
enough; there is no wildcard DNS to set up.

::: info
The relay strips the `/svc/<slug>` prefix before forwarding and rewrites
`Location` redirect headers so relative redirects work correctly.
:::

### TCP tunnels

The relay opens a raw TCP port in the range `BURROW_PORT_MIN`–`BURROW_PORT_MAX`
(default `9000`–`9100`) and proxies bytes verbatim. Use this for SSH, databases,
or any non-HTTP protocol.

```yaml
# burrow.yaml
services:
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001   # pin a stable port; 0 = auto-assigned
```

The service is then reachable at `burrow.insingo.com:9001`.

---

## Access modes

Every HTTP tunnel has an access policy controlling who can reach it.

| Mode | Who can access |
|------|----------------|
| `open` | Anyone — no authentication |
| `api_key` | Callers that present a valid API key |
| `burrow_login` | Burrow users who are logged in to the dashboard |
| `mtls` | Clients that present a valid mutual-TLS certificate |

The default is `open`. Change it from the service's detail page in the dashboard
or via `PUT /api/v1/services/{id}/access-policy`.

::: tip
`api_key` is the lightest-weight option for machine-to-machine access. Mint keys
from the dashboard or `POST /api/v1/services/{id}/api-keys`.
:::

::: warning
`mtls` requires a dedicated TLS handshake per service and is only available on
the opt-in host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`). Path-routed requests
(`/svc/<slug>`) share the dashboard's TLS session and cannot carry a per-service
client certificate.
:::

---

## Built-in automatic HTTPS

Set two environment variables and `burrowd` handles TLS end-to-end — no nginx,
no Certbot, no manual renewal:

```env
BURROW_ACME_DOMAIN=burrow.insingo.com
BURROW_ACME_EMAIL=admin@example.com
```

On startup, `burrowd` obtains a certificate from Let's Encrypt, promotes the
dashboard from `:8080` to `:443`, starts a `:80` listener for HTTP-01 challenges
and HTTPS redirects, and applies the same certificate to the host-routed proxy
ingress if you enabled it. The control channel on `:7000` uses it too.

::: info
When ACME is enabled, `BURROW_AUTH_DOMAIN` is inferred automatically from the
first domain in `BURROW_ACME_DOMAIN`. You do not need to set it separately.
:::

ACME is mutually exclusive with manually-supplied `BURROW_TLS_CERT/KEY` vars.
During local development use `burrowd serve --dev-certs` to generate
self-signed certificates instead.

---

## Architecture

```
  Developer machine                  Relay (burrow.insingo.com)
  ─────────────────                  ──────────────────────────

  ┌──────────────┐   TLS :7000       ┌──────────────────────────────┐
  │ burrow       │◄──control chan────►│ burrowd                      │
  │ (client)     │                   │                              │
  │              │   tunnel bytes    │  :443   dashboard + API      │
  │ :3000 (app)  │◄══════════════════│  :443   /svc/<slug>/ routes  │
  │ :22   (ssh)  │                   │                              │
  └──────────────┘                   │  :9001  TCP tunnel port      │
                                     └──────────────────────────────┘
                                              ▲          ▲
                                     browser / curl   SSH client
                                     GET /svc/k7p2qx/ :9001
```

Inbound traffic from the internet arrives at the relay. For HTTP tunnels the
relay looks up the tunnel by the slug in the `/svc/<slug>` path prefix, then
forwards the request over the persistent control-channel connection to the client
process, which hands it off to your local service. For TCP tunnels the relay
splices the raw connection.

---

## The dashboard

`burrowd` ships a built-in web dashboard at the relay's HTTP origin
(`https://burrow.insingo.com` when ACME is on). From the dashboard you can:

- Create and manage services (tunnels)
- Mint and revoke client tokens and API keys
- Configure access policies
- Manage users and roles
- View connection logs, audit logs, and AI gateway usage
- Configure SMTP, retention, and other server settings

No separate admin tool is needed.

---

## Next steps

- [Quickstart](/guide/quickstart) — get a tunnel running in under five minutes
- [Deploy on a server](/guide/deploy) — production Docker Compose setup with ACME
