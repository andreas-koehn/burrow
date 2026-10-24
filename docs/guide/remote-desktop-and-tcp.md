# Remote desktop and other TCP services

This guide shows how to reach a remote desktop (RDP), SSH, a database or any
other TCP service on a machine behind your router, through a Burrow relay. It
starts with the RDP walkthrough and goes on to everything a TCP service can
do: [creating it in the dashboard](#create-a-tcp-service),
[host names by TLS passthrough](#reach-it-by-host-name-tls-passthrough),
[PROXY protocol](#proxy-protocol-to-the-local-target),
[country rules](#country-rules), [automatic bans](#automatic-bans),
[bandwidth and volume limits](#bandwidth-and-daily-volume) and
[scoped tokens](#scoped-client-tokens).
The examples use RDP on port 3389. The same steps work for SSH (22), VNC
(5900) and Postgres (5432).

The short version: run `burrow tcp 3389 --name rdp` at home, run
`burrow forward <slug>` on the machine you are sitting at, and point your RDP
client at `localhost:13389`. The relay opens no public port, and only people
you allow can connect.

---

## Pick a mode

Every TCP service has an access mode. `burrow tcp` creates new services as
`private`.

| Mode | Public port | Who can connect | Use it for |
|------|-------------|-----------------|------------|
| `private` (default) | none | Burrow users who run `burrow forward`: the owner, plus roles the owner allows | RDP, SSH, VNC, databases. **Recommended.** |
| `ip` | yes | Addresses on a list (CIDRs) and addresses you allow for a few hours | A service that other programs must reach directly, from known networks |
| `open` | yes | Anyone on the internet | Services that are meant to be public and protect themselves |

Use `private` unless you have a reason not to. A private service is not on the
internet at all: there is no port to scan, no brute-force noise in your logs,
and nothing to patch in a hurry when a new RDP flaw is published. A visitor
needs a Burrow account and a client token first.

Use `ip` when a program that cannot run `burrow forward` must connect, for
example a build server that always comes from the same address, or a phone
you allow for one afternoon. Use `open` only for a service that is public by
design.

You can change the mode later on the service's page in the dashboard. A
change closes the tunnel and the home client registers again under the new
mode. See [A mode change](#changing-the-mode).

---

## Private: from home to anywhere

### At home (Ubuntu)

Turn on remote desktop on the machine you want to reach. Either one works:

- **GNOME Remote Desktop** (Ubuntu desktop): open **Settings → System →
  Remote Desktop**, switch on **Remote Desktop**, and set a user name and a
  strong password for it.
- **xrdp**: `sudo apt install xrdp`, then `sudo systemctl enable --now xrdp`.
  It signs in with the account's Linux password, so that password must be
  strong.

Both listen on port 3389. Check it from the same machine:

```sh
ss -ltn | grep 3389
```

Sign in to your relay once, then publish the port:

```sh
burrow login burrow.insingo.com
burrow tcp 3389 --name rdp
```

`burrow tcp` creates a new service private. Unlike
`burrow tcp 3389 --name rdp --access private`, the command without `--access`
asks for no mode, so it keeps serving if you later change the mode in the
dashboard. The command stays in the foreground and prints the way in:

```
This service is private: the relay opened no public port. Reach it from another machine with: burrow forward k7p2qx 13389
```

The word after `forward` is the service's **slug**. The relay makes it for
you: six letters and digits, like `k7p2qx`. A tcp service that existed before
v0.13.0 got a longer one, `t` and ten hex digits (for example `t3f9a1c07be`),
and keeps it. The slug is also shown on the service's page in the dashboard.

::: tip The home client connects to 127.0.0.1
Burrow reaches the desktop on `127.0.0.1:3389` of the same machine. You can
block port 3389 from your home network in the firewall; Burrow does not need
it.
:::

#### Keep it in a file

Put the service in `burrow.yaml` so that `burrow up` starts it the same way
every time:

```yaml
services:
  - name: rdp
    local: 127.0.0.1:3389
    type: tcp
    access: private
```

An entry without `access:` asks the relay for nothing: a service that exists
keeps the mode stored in the dashboard, and a new one is created `private`.
Older clients and older relays create it `open`; against a relay too old for
tcp access modes, this client does not register the entry at all (add
`access: open` if you want a public port there). Writing `access: private`
makes the wish explicit: the client then stops when the dashboard says
something more open. `access: private` together with `remote:` is refused,
because a private service has no port. An entry with `remote:` and no
`access:` is created `open`, as before: a fixed public port is what it asks
for.

#### Run it as a service

To keep the tunnel up after you log out or restart, install `burrow up` as a
system service. It uses the stored sign-in and the `burrow.yaml` above:

```sh
sudo "$(command -v burrow)" service install
burrow service status
burrow service logs
```

See [Keep it running](/guide/connect-client#keep-it-running) for macOS and
Windows.

### On the road (Windows)

Install Burrow on the laptop and sign in. The laptop needs its own client
token; it is made when you sign in.

```powershell
burrow login burrow.insingo.com
burrow forward k7p2qx 13389
```

Replace `k7p2qx` with your service's slug. Leave the window open. The
command listens on `127.0.0.1:13389` and sends every connection to the relay,
which sends it on to your home machine. Port 13389 is the default. It keeps
the machine's own remote desktop on 3389 free.

In a second window, or from the Start menu:

```powershell
mstsc /v:localhost:13389
```

Sign in with the user name and password of the home machine.

The first connection shows a certificate warning, because the desktop's
certificate is for another name than `localhost`. That is expected. Compare
the fingerprint with the one the home machine shows if you want to be sure.

If you own the service you can also use its name, `burrow forward rdp
13389`, or its id.

#### Run the forward in the foreground or as a task

For a one-off session, run `burrow forward` in a terminal and close it when
you are done.

To start it when you sign in to Windows, make a scheduled task:

```bat
schtasks /Create /TN "Burrow forward rdp" /SC ONLOGON /RL LIMITED /TR "\"%LOCALAPPDATA%\Programs\burrow\burrow.exe\" forward k7p2qx 13389"
```

Or list the forward in a `burrow.yaml` and run `burrow up`:

```yaml
forwards:
  - target: k7p2qx   # slug, own service name or id
    local_port: 13389     # default 13389
    bind: 127.0.0.1       # default 127.0.0.1
```

A file may hold `forwards:` alone. `burrow service install` runs the same
file as a Windows service; see [Keep it running](/guide/connect-client#keep-it-running)
for the rules (the binary must be below `Program Files`).

The local port stays open when the relay is away. A connection made then is
closed at once, and the forward starts working again when the relay is back.
You do not need to restart it.

### macOS and Linux clients

The forward is the same on every system:

```sh
burrow login burrow.insingo.com
burrow forward k7p2qx 13389
```

Then connect to `localhost:13389`:

- **macOS:** in Windows App (Microsoft Remote Desktop), add a PC named
  `localhost:13389`.
- **Linux:** `xfreerdp /v:localhost:13389 /u:youruser`, or Remmina with the
  server `localhost:13389`.

For SSH, forward the service and use the local port:
`burrow forward <slug> 2222`, then `ssh -p 2222 user@localhost`.

### Who may forward

- The **owner** of the service may always forward.
- **Other users** may forward when their role is in the service's access
  policy. On the service's page this is the list called **Who may forward**.
  The API is `PUT /api/v1/services/{id}/access-policy`.
- **An empty policy means the owner only.** An administrator is not let in by
  role alone: the owner has to allow the `admin` role like any other.
- The policy is read again on every forward. Taking a role out of it stops
  that role's next connection at once.
- A suspended user, a revoked or expired token, and a closed session cannot
  forward.

A person who may not forward gets the same answer as a person who asks for a
service that does not exist (`not_found`). The relay does not say which it
was, so nobody can use it to find out which services exist.

Limits for people who forward:

| Limit | Value |
|-------|-------|
| One client session | 10 opens at once, then 30 a minute; at most 16 open at a time |
| One non-owner, one service | 4 forwards at the same time |
| The owner, one service | 4 forwards at the same time; they do not count against the service's connection cap, so the owner is never locked out by other users |
| One service, all users | the service's connection cap (default 32) and 16 pairings waiting |

A forward that hits a limit gets `rate_limited` and waits five seconds before
it tries again.

### Changing the mode

On the service's page, **Access mode** takes `open`, `ip` or `private`.
Changing it closes the tunnel and the home client registers again.

- **Stricter** (open to ip to private): applies when the client reconnects,
  which happens by itself.
- **More open** (private to ip or open): a client started without `--access`
  (or a `burrow.yaml` entry without `access:`) serves the new mode when it
  reconnects, by itself. A client that was started with a stricter `--access`
  stops with `service is <mode> in the dashboard; restart with --access <mode>
  to serve it`. The dashboard asks you to confirm, in red, before it makes a
  service more open.

The rule behind this: the client never widens access on its own, and the
dashboard can always tighten it. If you start `burrow tcp` with `--access
open` and the dashboard says private, the service stays private, and the
client says so.

---

## IP mode

In `ip` mode the relay opens a public port and checks every connection
before it contacts your home machine. A refused connection is closed without
a byte, and the home client never sees it.

A connection is let in when its source address is on one of two lists:

- **Permanent CIDRs**, the allow list on the service's page.
- **Grants**, addresses allowed for a limited time.

A block entry beats an allow entry. **An empty list with no grants lets
nobody in.** An address that expires stops getting new connections; a
connection that is already open stays.

### Permanent CIDRs

Add networks or single addresses (a bare IP is the same as `/32`). Countries
are a separate list; see [Country rules](#country-rules). A CIDR on a TCP service may not be wider than a
`/8` of IPv4 or a `/32` of IPv6; `0.0.0.0/0` is refused. If you want to let
everyone in, use mode `open` and say so. Block entries may be wider.

Changing the list is audited as `ipgeo.update`.

### Allow my IP for a few hours

On the service's page, **Allow my IP** adds a grant for the address the relay
sees you coming from. Choose 1, 8 or 24 hours. The API takes any whole number
of hours up to 7 days (168).

- The grant is a single address (`/32`, or `/128` for IPv6).
- **Widen to /24** (IPv4) or **/64** (IPv6) when your provider changes your
  address often. A /24 is 256 addresses; widen only when you trust the
  network.
- You can also enter an address or CIDR by hand, up to a `/16` of IPv4 or a
  `/48` of IPv6.
- A service holds at most 32 active grants. You can see the time left and
  revoke a grant at any time.
- Adding a grant asks you to confirm your password again. Revoking does not.

### Things that go wrong

**IPv4 and IPv6 do not match.** A grant covers one address family. The
dashboard shows which one it used (`ipv4` or `ipv6`). If you click **Allow my
IP** over IPv6 and your RDP client then connects over IPv4, the connection is
refused. Add the other address by hand, or turn IPv6 off on the client for the
connection. Check what a refused visitor looked like in the connection log:
the `source_ip` is the address the relay saw.

**Shared NAT.** Everyone behind the same office router, hotel network or
mobile carrier shares one public address. A grant for that address lets in
all of them. Prefer `private` on shared networks.

**Docker's userland proxy.** When the relay runs in Docker and the port is
published through Docker's userland proxy, every visitor can arrive from the
Docker gateway address (for example `172.18.0.1`). Then the relay sees the
gateway, not the visitor. Two consequences:

- An allow entry for the gateway lets in everyone who can reach the port.
- The per-source limits (below) are shared by all visitors.

The dashboard warns when a grant would be for a private, loopback, link-local
or `100.64.0.0/10` address. If the connection log of a service in `ip` mode
shows only a gateway address, do not use `ip` there; use `private`, or fix the
Docker network so that the real address arrives.

**Forwards skip the IP check.** A user who is allowed to forward reaches an
`ip` service through `burrow forward` from any address, without a grant. The
list covers the public port only. Who may forward is decided by the access
policy.

---

## Open mode

`open` is how TCP tunnels worked in the first versions of Burrow, and it is
what an old client gets. The public port takes connections from anyone.

The risks are the same as for any service on the internet:

- Scanners find the port within hours, and RDP and SSH get password
  guessing from then on.
- A flaw in the service is exposed to everyone before you can patch it.
- The relay limits how fast one address can connect, but it cannot tell a
  guess from a login.

The dashboard shows a red banner for `open`. It adds a warning when the local
port is 3389, 22, 5900 or 5432, the ports for RDP, SSH, VNC and Postgres, and
`burrow tcp` prints the same warning. Use `private` or `ip` for these.

If you do run an open service, give it a strong password, turn on its own
account lockout, keep it patched, and watch the connection log.

---

## Create a TCP service

`burrow tcp` creates the service the first time it runs. You can also create
it first, in the dashboard or with the API, and start the client later. The
service then has its mode, port and settings before any client connects.

**Dashboard:** **Services → New service → Create a TCP service instead**. Enter
a name, pick the mode, and for `ip` or `open` optionally a public port. The
dialog then prints the command to run at home:

```sh
burrow tcp 3389 --name rdp
```

and the matching `burrow.yaml` entry. The service's page has a card for every
setting described below.

**API:** `POST /api/v1/services` with `"type": "tcp"`:

```sh
curl -X POST https://burrow.insingo.com/api/v1/services \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"tcp","name":"rdp","access_mode":"private"}'
```

| Field | Notes |
|-------|-------|
| `name` | Required. Letters, digits, `.`, `_` and `-`, up to 64, starting with a letter or digit. The client's `--name` must match it. |
| `access_mode` | `open`, `ip` or `private` (default `private`) |
| `tcp_port` | Optional public port, pinned to the service. It must be inside `BURROW_PORT_MIN`..`BURROW_PORT_MAX`. A port held by any other service gives 409, even by an offline automatic hold. |
| `proxy_protocol` | Optional, `v2`. See [PROXY protocol](#proxy-protocol-to-the-local-target). |
| `idle_timeout_s` | Optional, 0 or 60 to 86400. See [Idle timeout](#idle-timeout). |

Creating a service needs `services:configure:any`, and it is not an MCP tool.
The owner is the caller. Then run `burrow tcp <port> --name <name>` at home:
the client finds the service by name and serves it in the stored mode.

---

## Reach it by host name (TLS passthrough)

A service in mode `open` or `ip` can also be reached by a host name on one
shared TLS port, next to its own public port, or instead of it (see
[By port, by host name, or both](#by-port-by-host-name-or-both)). The relay reads the server name
(SNI) from the first TLS record, picks the service with that name, and passes
the connection on. It terminates no TLS: the target serves its own
certificate for that name.

Turn it on in the relay's environment:

| Variable | Purpose |
|----------|---------|
| `BURROW_TLS_PASSTHROUGH_LISTEN` | The listener, for example `:8444`. Empty (default) = off. |
| `BURROW_TCP_DOMAIN` | Optional. Every `open` or `ip` tcp service is then reachable as `<slug>.<domain>`. Needs a wildcard DNS record `*.tcp.example.com` pointing at the relay (or at the proxy in front of it). |
| `BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES` | Addresses allowed to send a PROXY v2 header. Default empty. See [Behind a proxy](#behind-a-proxy-coolify-and-traefik). |

`BURROW_TCP_DOMAIN` must not be the dashboard's host or a parent of it, nor
the services host.

**Custom host names.** A service can also have a name of its own, for example
`db.example.com`: `PUT /api/v1/services/{id}/tcp-hostname` with
`{"tcp_hostname":"db.example.com"}`, or the **Host name** card in the
dashboard. Setting a name needs `services:configure:any` (an administrator);
an owner with `services:configure:own` can only clear it. A name is unique and
must be a host name with at least two labels. The relay refuses names under
the tcp domain, the dashboard's domain or the services domain, and names a
custom domain already covers. A change asks for a recent password
confirmation and is not an MCP tool. `GET` on the same route lists the names
and says whether the service is `reachable` this way.

**What is refused.** The connection is closed without a byte when:

- the service is `private` (a private service has no name here, ever);
- the name is unknown, or the hello has no SNI;
- the first byte is not TLS;
- the ClientHello does not arrive in 5 seconds, or is larger than 16 KiB.

A hello spread over several TCP segments works. A hello split over several
TLS **records** cannot be read and is closed (some very large post-quantum
hellos and unusual clients do this). Refusals before a service is known are
only in the relay log, as minute summaries under `tls-passthrough`, not in the
connection log.

An accepted connection goes through the same checks as the public port: the
mode (`ip` lists and grants), country rules, bans, limits and the idle
timeout. Its row in the connection log has protocol `tls` and the SNI.

**What works.** Only protocols that begin with a TLS ClientHello that carries
the name: HTTPS, TLS-wrapped TCP (stunnel, `openssl s_client -servername`),
and PostgreSQL 17 with `sslnegotiation=direct`. **RDP does not work this
way:** the client sends an X.224 request first and starts TLS afterwards, so
there is no name in the first bytes. RDP and plain SSH stay on the port or on
`burrow forward`.

### By port, by host name, or both

An `open` or `ip` service is reached **by its port and by host name** by
default (the host name only while the listener runs and the service has a
name). To reach it **by host name only**, turn on **Host name only** on the
**Host name** card, or `PUT /api/v1/services/{id}/tcp-hostname` with
`{"hostname_only": true}`. The relay then closes the service's public port
and opens none: only a TLS client that names the host gets in. A service that
needs no public port at all is `private` and reached with `burrow forward`.

- It can be turned on only while the relay runs the passthrough listener
  (`BURROW_TLS_PASSTHROUGH_LISTEN`) and the service has a name there: its
  slug name under `BURROW_TCP_DOMAIN`, or a custom host name. Otherwise the
  API answers 400 with the reason, and the only name of such a service
  cannot be cleared while it is on.
- The client registers again on its own when the setting changes (the
  session that served the service is closed, as for a new mode). `burrow`
  then says the service has no public port.
- If the relay later has no name for the service (the listener or the tcp
  domain was removed from its configuration), it opens the port again: a
  service is never left with no way in.
- A pinned port (`--remote`, or the port given on create) stays the
  service's for when the port is on again; an automatic one goes back to the
  pool.
- `false` brings the port back. The change asks for a recent password
  confirmation, is audited as `service.tcp_hostname_only.update`, and is not
  an MCP tool.

### Behind a proxy: Coolify and Traefik

Coolify's Traefik owns port 443. A TCP router with `HostSNI` and
`tls.passthrough=true` sends matching names to Burrow and leaves every other
name to Coolify's HTTP routers. Add the labels to the Burrow service and
publish nothing else:

```yaml
services:
  burrow:
    environment:
      BURROW_TLS_PASSTHROUGH_LISTEN: ":8444"
      BURROW_TCP_DOMAIN: tcp.example.com
      # Traefik's address on the Docker network, nothing wider.
      BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES: 10.0.1.5
    labels:
      - traefik.enable=true
      - traefik.tcp.routers.burrow-sni.entrypoints=https
      - traefik.tcp.routers.burrow-sni.rule=HostSNI(`db.example.com`)
      # or every slug name: HostSNIRegexp(`^.+\.tcp\.example\.com$`)
      - traefik.tcp.routers.burrow-sni.tls.passthrough=true
      - traefik.tcp.routers.burrow-sni.service=burrow-sni
      - traefik.tcp.services.burrow-sni.loadbalancer.server.port=8444
      - traefik.tcp.services.burrow-sni.loadbalancer.proxyProtocol.version=2
```

`https` is the name of Coolify's TLS entrypoint; use yours if it differs.
Since Traefik 3.5.2 the PROXY setting moved to a TCP `serversTransport`
(`proxyProtocol.version: 2`); check the Traefik documentation for your
version.

- **Trusted proxies and the header.** A connection from an address in
  `BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES` **must** begin with a PROXY v2
  header that names the visitor; without one it is closed. A header from any
  other address is closed too. With the list empty (default), no header is
  believed and the TCP peer is the visitor. Only version 2 is read. The list
  is never taken from `BURROW_TRUSTED_PROXIES`; set it to Traefik's address
  only, not to a whole private range.
- **Always turn the header on in Traefik.** Without it every connection from
  Traefik is closed, and if it were let in, every visitor would be Traefik's
  address: `ip` mode and the per-source limits would apply to all of them
  together.
- **DNS.** One record for each custom name, and the wildcard record for the
  tcp domain, pointing at the server that runs Traefik.
- The certificate is the target's. Traefik does not decrypt anything.

**Direct port mapping.** Without a proxy in front, publish the listener
yourself and leave the trusted list empty:

```yaml
    ports:
      - "8444:8444"      # or "443:443" with BURROW_TLS_PASSTHROUGH_LISTEN=":443"
```

Open the port in the firewall (`ufw allow 8444/tcp`). Use 443 only on a host
where nothing else needs it, which is not the case when ACME listens there.

### Unauthenticated waiting is bounded

The relay holds at most 1024 connections that wait for their hello, and 32 per
untrusted source (an IPv4 address, or an IPv6 /48). Trusted proxies have their
own budget of 1024 and no per-source cap. When a budget is more than 75 % full
the wait shrinks from 5 to 2 seconds. A connection over a cap is closed at
once.

---

## PROXY protocol to the local target

By default the local target sees every connection come from `127.0.0.1`, the
home client. A service can ask the home client to put a PROXY protocol **v2**
header in front of each connection, with the address of the visitor (or, for a
forward, of the forwarder). Nginx, HAProxy, Traefik and Postgres proxies can
read it. The target must be set up to expect it: a target that does not read
the header sees garbage and drops the connection.

Turn it on in one of these places:

- the service's **Connection settings** card in the dashboard;
- `PUT /api/v1/services/{id}/tcp-settings` with `{"proxy_protocol":"v2"}`
  (`""` turns it off);
- `burrow tcp 8443 --proxy-protocol v2`;
- `proxy_protocol: v2` on a service in `burrow.yaml` (`burrow up`).

The relay's setting applies from the next connection with no restart. The
header is written when **either** the relay's setting or the client's own is
`v2`; the dashboard shows when the client also sends it. `burrow connect
--config` does not send the header and prints a warning.

::: warning Upgrade the client first
A home client older than this release ignores the setting and writes no
header. The relay therefore asks for the client's `proxy_protocol_v2`
capability: while the service asks for v2 and the connected client lacks it,
every visitor and forward is refused (reason `proxy_unsupported`) and the
dashboard shows a warning. Update the client, then turn the setting on.
:::

---

## Protocol and host name in the logs

The connection log and the live connection list show two more columns:

- **protocol:** `tls`, `ssh`, `rdp`, `http` or `unknown`, from the client's
  first bytes. The classifier only watches the bytes as they pass, so it never
  delays a connection. A visitor that sends nothing is `unknown`.
- **sni:** the server name of a TLS ClientHello, for the public port as well
  as for the passthrough listener.

Both are in the CSV and NDJSON exports (the last two columns are `protocol`
and `sni`). Refused connections have neither.

---

## Idle timeout

A connection with no bytes in either direction for the idle timeout is closed,
with reason `idle_timeout`. It is off by default. Set it from 60 seconds to 24
hours (`idle_timeout_s` 60 to 86400, or minutes in the dashboard) in
**Connection settings**, or with `PUT /api/v1/services/{id}/tcp-settings`. A
change applies to open connections too. The relay checks every 5 seconds, so a
connection closes up to 5 seconds late. A connection that only moves data one
way is not idle, and neither is one that waits for its share of the service's
bandwidth limit: a throttled connection is never closed as `idle_timeout`.

---

## Country rules

Country allow and block lists for a tcp service work in mode `ip` and `open`,
and the same lists work for http services.

1. Download a MaxMind-compatible **GeoLite2-Country** `.mmdb` file (a free
   MaxMind account gives you one; `geoipupdate` keeps it fresh).
2. Set `BURROW_GEOIP_DB=/data/GeoLite2-Country.mmdb` (the older name
   `BURROW_GEO_DB_PATH` still works; `BURROW_GEOIP_DB` wins). There is no build
   tag any more: every build reads the file.
3. Add countries on the service's **Countries** card, or in
   `PUT /api/v1/services/{id}/ip-geo`.

The relay checks the file once a minute and loads it again when it changes.
Replace it by renaming a new file over it (as `geoipupdate` does); a broken file
keeps the old database in use and shows its error in `GET /api/v1/geo/status`.

**Rules.**

- **Unknown country follows the list type.** An address with no record, a
  failed lookup, and any address while no database is loaded count as
  "unknown": an **allow** list **denies** it and a **block** list **allows**
  it.
- **Fail closed without a database.** A country allow list lets nobody in
  while no database is loaded. A country block list cannot block anything.
  The relay logs a warning at start when rules exist and nothing enforces them,
  and `GET /api/v1/geo/status` reports `enabled`, `configured`, `error` and
  `rules_not_enforced`. Adding a country to a list is refused with 400 while
  no database is loaded; clearing always works.
- **Order in `ip` mode:** block CIDRs, block countries, then allow CIDRs,
  grants and allow countries. In `open` mode only the country lists apply.
- **Forwards are not checked.** They are authenticated.
- The relay caches a source's country for 5 minutes, so a reloaded database
  reaches a known source after that.

::: warning Stored country lists now apply
Before this version the country lists of tcp services were saved but never
enforced, and the country rules of http services were not enforced either. They
now apply to both. A service that already has a list may start refusing
visitors after the upgrade: check `GET /api/v1/geo/status` and the lists
first. For http services a failed lookup used to let the visitor in; it is now
"unknown" and follows the list type.
:::

---

## Automatic bans

Auto-ban closes a source that keeps opening connections and dropping them
at once, which is what scanners and brute-force tools do. It is off by
default; turn it on in the **Limits** card or with `PUT
/api/v1/services/{id}/tcp-limits`.

| Setting | Default | Range |
|---------|---------|-------|
| `autoban.short_s` | 3 | 1 to 300: a connection that ends sooner counts as short |
| `autoban.count` | 10 | 2 to 100: short connections that trigger a ban |
| `autoban.window_min` | 10 | 1 to 1440: how long they are counted for |
| `autoban.ban_min` | 60 | 1 to 43200 minutes (30 days) |

A source is an IPv4 address or an IPv6 /64. A banned source is closed at accept
with no byte (reason `banned` in the connection log), its open visitors are
closed, and the ban survives a restart. Only `open` and `ip` visitors can be
banned: **forwards are never banned**, and a connection counts only when the
**visitor ended it** within `short_s`. A connection the relay ended (idle,
quota, dashboard), one that failed to pair, and one that the home client closed
at once (a dead local target) never count. In `ip` mode, a source that a
permanent allow entry or a grant that has not expired lets in is trusted: its
short connections are not counted, so it is never banned automatically. A
source let in by its country only is counted.

Bans are listed on the **Bans** card and at `GET
/api/v1/services/{id}/tcp-bans`. Add one by hand with `POST` (`source`,
`ban_min`, optional `note`; at most 4096 active per service) and lift one with
`DELETE ...?source=`, which asks for a recent password confirmation. A `POST`
for a source that is banned already extends or renews its ban, never shortens
it: a ban that would end sooner is refused with 409, and the way to lift or
shorten a ban is the `DELETE` (then ban again). `POST` is an MCP tool because
a ban only narrows access; the `DELETE` is not.
Turning auto-ban off keeps the existing bans until they expire or are lifted.
An automatic ban sends the `tcp.banned` webhook (at most 30 per service per
minute) and every ban is in the audit log as `tcp.ban.add`; lifting is
`tcp.ban.remove`.

**Caveats.** A ban hits everyone who shares the source address:

- **Shared NAT.** An office, a hotel or a mobile carrier is one address.
  One misbehaving client bans the rest.
- **VPS providers** often hand a whole IPv6 /64 to one customer, and several
  customers may share a /64. The relay bans the /64.
- **Docker's userland proxy** shows every visitor as the gateway address, so one
  ban would lock out all visitors. Fix the address first (see
  [Things that go wrong](#things-that-go-wrong)).
- **Monitors** that connect, check the port and close (load balancer health
  checks, uptime probes) look exactly like a scanner. In `ip` mode, give their
  address a permanent allow entry or a grant: the relay then never counts
  their short connections and never bans them automatically (a ban already
  made, or one by hand, still holds until it is lifted). In `open` mode there
  are no allow entries: keep the count high, or leave auto-ban off.

---

## Bandwidth and daily volume

Both limits are in the **Limits** card and in `PUT
/api/v1/services/{id}/tcp-limits`. Saving them asks for a recent password
confirmation, and they are not MCP tools. 0 means off. Changes apply to open
connections at once.

| Setting | Range | Meaning |
|---------|-------|---------|
| `rate_limit_kbps` | 0 or 64 to 10 000 000 | Kilobits per second (k = 1000, so 1 kbps is 125 bytes/s) **per direction**, shared by all connections of the service |
| `daily_bytes` | 0 or 1 MiB to 2^50 | Bytes in plus out per UTC day |

`rate_limit_kbps` is one bucket for each direction: an echo at the limit moves
the limit both ways.

**Daily volume.** At 80 % the relay sends the `tcp.quota.warning` webhook once per
service and day (not again after a restart that day). At the limit every open
visitor is closed (reason `quota`), and new visitors are refused until
00:00 UTC. **Forwards** are authorized (the owner, or a role the access policy
names): their bytes count towards the volume, but the volume never refuses or
closes them, so strangers on an `open` port cannot lock the owner out. A
limit raised or removed lets traffic in at once. The count may overshoot by one buffer (up to 32 KiB), the stored figure lags up to a minute,
and a crash can lose up to a minute of counted bytes. The card shows today's
usage; `GET tcp-limits` returns `usage_today` and `quota_reached`.

::: warning The bandwidth applies to the owner too
Bandwidth and volume count every connection of the service, forwards included.
The bandwidth bucket is shared: on an `open` service strangers can slow your
forwards down. The daily volume does not stop forwards, but once strangers
have used it up the service takes no new visitors until 00:00 UTC, and your own
forwards can carry it past `daily_bytes`. Use `ip` or `private` for services
you rely on.
:::

---

## Scoped client tokens

A client token can be limited to one job:

| Scope | May | May not |
|-------|-----|---------|
| `all` (default) | register tunnels and open forwards | |
| `forward` | open forwards | register tunnels (`scope_denied`) |
| `publish` | register tunnels | open forwards (`scope_denied`) |

Use `forward` for the laptop you travel with, and `publish` for the machine
at home. A lost laptop then cannot publish anything.

- `burrow login <relay> --scope forward` asks for the scope. The person who
  approves the sign-in sees it, can pick another on the approval page, and the
  token gets that. The client prints the scope it got.
- A token made in the dashboard (**Clients → Tokens**) or with `POST
  /api/v1/tokens` takes `scope`. `--scope` cannot be used with `--token -`.
- The relay checks the scope at every registration and every forward, so a
  reconnect reads it again.

::: warning Upgrade the client
An older relay ignores the scope and issues an `all` token (the new client says
so). An older client does not know `scope_denied`: with a `forward` token its
`burrow tcp` retries instead of stopping. Update both before you rely on
scopes.
:::

---

## Not covered

- **UDP.** Only TCP is carried. Services that need UDP (RDP's UDP transport,
  games, DNS) fall back to TCP or do not work.
- **Peer-to-peer.** Every byte goes through the relay.
- Several tunnels for one service, and any inspection of TCP payloads beyond
  the first-bytes label.

---

## Limits and ports

### Limits on visitors (open and ip)

The relay applies these before it contacts your home machine. A visitor over a
limit is closed at once.

| Limit | Value | Reason in the log |
|-------|-------|-------------------|
| New connections from one source | 10 a minute, burst of 5 | `rate_limited` |
| Open connections from one source | 4 | `source_limit` |
| Open visitors of one service | `max_connections`, default 32, at most 256 | `tunnel_full` |
| Visitors waiting to be paired | 16 | `pending_full` |
| Pairing time | 5 seconds; the connection is closed if the home client does not answer | `no_stream` |

A source is one IPv4 address, or one IPv6 `/64`. Each service tracks at most
4096 sources at a time. Sources that have been quiet are dropped; if the table
is full of active ones, a new source is refused (`rate_limited`) rather than
let in.

`max_connections` is set with `PUT /api/v1/services/{id}/max-connections`
(`0` means the default of 32). It is a service setting, not a flag of
`burrow tcp`. Changing it is audited as `service.max_connections.update`.

On port 7000, after 10 failed sign-ins from one address the relay answers
further invalid tokens from it with `rate_limited` instead of `invalid_token`,
and allows one more failure every 30 seconds. Every token is still checked,
and a valid token is accepted at once: this is a signal, not a gate (see
[Access control](/guide/access-control#access-modes-of-tcp-services)).

### Ports

An `open` or `ip` service gets a port from `BURROW_PORT_MIN` to
`BURROW_PORT_MAX` (default 9000 to 9100). A `private` service gets none.

- **A port stays with its service**, also while the client is offline, so the
  address you gave out does not move.
- **`--remote <port>` pins a port.** A pinned port is freed only when you
  delete the service or the service itself asks for another port. No other
  service can take it.
- **Other holds can be reclaimed.** An explicit `--remote` takes a port from an
  offline service whose port was assigned automatically. When the whole range
  is held, the relay reclaims the automatic port of the offline service that
  was seen longest ago. Each takeover is audited as
  `service.tcp_port.reclaimed`.
- **A private service gives back an automatic port.** A pinned port is kept, so
  going back to `open` or `ip` gets the same number.
- **A user may hold a limited number of ports.** `BURROW_TCP_PORTS_PER_USER`
  (default 10, `0` for no limit; administrators are never limited). When it is
  reached, a new service is refused with "port limit reached" until you delete
  one you no longer use. See [Configuration](/guide/configuration).
- **One client serves a service.** A second `burrow tcp` for a name that is in
  use gets `service_busy` from the same token and `name_in_use` from another
  token.

---

## Security notes

**NLA and strong passwords stay mandatory.** Burrow carries the bytes of your
remote desktop; it does not replace its login. Keep Network Level
Authentication on for RDP. Use a long unique password for the account you
connect to, or a key for SSH. The account is the last thing between a
visitor and your machine. `private` keeps strangers out; it does not make a
weak password safe against anyone who has a Burrow account on that relay and
whom you let forward.

**The relay's certificate is pinned.** The connection from the client to the
relay (port 7000) is encrypted with TLS. `burrow login` reads the SHA-256
fingerprint of the relay's control certificate from the relay's HTTPS
dashboard and stores it, and the client then accepts port 7000 only with that
certificate. See [Connect a client](/guide/connect-client). Two flags turn
this off:

- `--insecure` skips every check of the relay's certificate and prints a
  warning on every run. Anyone on the path can read your token and your
  traffic. Use it only for a local development relay.
- A private forward **refuses to run under `--insecure`**, because it would
  send a remote desktop through an unverified connection. If you really mean
  it, add `--insecure-forward`:
  `burrow --insecure --insecure-forward forward <slug>`.

**Revoking a token ends its forwards.** Revoking a client token, suspending
the user or deleting the user closes the live forwards of that token at once.
The next connection fails until the user has a valid token again. Taking a
role out of the service's access policy, or giving a user a role the policy
does not name, ends that user's live forwards to the service too. The owner's
forwards are not ended this way.

**The dashboard can disconnect.** The service's page lists live connections,
visitors and forwards, with the address or the user and token name, the time
and the live byte counts. **Disconnect** ends one. It asks for your password
and is audited.

### Audit and logs

Audit actions for TCP services (Settings → Audit log):

| Action | When |
|--------|------|
| `service.tcp_access.update` | the access mode of a TCP service changed (`from`, `to`) |
| `ipgeo.update` | the permanent CIDR lists changed (the old and new lists) |
| `service.ip_grant.add` | a grant was added |
| `service.ip_grant.revoke` | a grant was revoked |
| `service.max_connections.update` | the connection cap changed |
| `service.connection.close` | someone disconnected a live connection |
| `service.tcp_port.reclaimed` | a port was taken from an offline service |
| `service.tcp_settings.update` | PROXY protocol or idle timeout changed (`from`, `to`) |
| `service.tcp_hostname.update` | the custom host name changed |
| `service.tcp_hostname_only.update` | the service was set to host name only (no public port), or back (`from`, `to`) |
| `service.tcp_limits.update` | bandwidth, daily volume or auto-ban settings changed |
| `tcp.ban.add` | a source was banned, automatically or by hand (`by`) |
| `tcp.ban.remove` | a ban was lifted |
| `tcp.forward.open` | a user opened a forward. **Sampled:** one row per user and service per minute |

The connection log has every connection:

- `tcp_proxy` rows for visitors of open and ip services, with source address,
  bytes and duration.
- `tcp_forward` rows for forwards, with the user, the token name and the
  forwarder's address.
- Refusals as `rejected` rows with a reason: `ip_geo`, `rate_limited`,
  `source_limit`, `tunnel_full`, `pending_full`, `no_address`, `forbidden` (a
  forward the user may not make), `user_limit`, `banned`, `quota` (visitors
  only) and `proxy_unsupported`. They are **sampled**: one
  row per service, reason and address per minute, so a scan does not fill the
  log. The metric `burrow_connections_total` counts all of them.
- A connection the relay ended says why in its reason: `closed_by_dashboard`
  (someone pressed Disconnect), `access_revoked` (a forward the user may no
  longer make, after a change of the access policy, the user's role or
  status), `session_closed` (the forwarder's or the home client's session
  ended), `idle_timeout`, `quota` (the daily volume was reached; visitors
  only) or `banned` (its source was banned). A connection that one side closed has no reason.
- Every row also has the `protocol` and the `sni` when they are known.

The webhook event `access.denied` is sent for TCP refusals too, at most one
per service and reason per minute. `tcp.banned` and `tcp.quota.warning` are
the two events of the limits. See [Operations](/guide/operations).

---

## Performance

A forward is **TCP inside yamux inside TCP**: your client connects over TLS to
the relay, which relays to your home machine over the same kind of
connection. Every packet makes two hops, and a lost packet stalls everything
behind it on that connection (head-of-line blocking).

- **RDP over UDP is not used.** Modern RDP clients try UDP for video and
  fall back to TCP when it does not work. Through Burrow only TCP exists. The
  desktop is usable but less smooth on a poor link than a direct RDP over UDP.
- **Latency adds up.** The round trip is your latency to the relay plus the
  relay's latency to your home. Choose a relay close to one of the two ends.
- **The window limits throughput.** One TCP connection can carry at most one
  window of data per round trip. Throughput is at most the window divided by the round-trip time: for
  example, a 64 KB window at 100 ms is about 5 Mbit/s.
  Slow screen updates and slow file copies on a far-away relay come from this.
- **What helps:** a nearer relay, a lower colour depth and no wallpaper or
  animations in the RDP client, and the RDP client's "LAN" or "broadband"
  profile turned down.

---

## Open and ip services on Coolify or Docker

Skip this section for `private` services: they need no port.

When the relay runs in a container, a public port is reachable only when you
publish it. Add every data port you use to the Compose file, and open the
same ports in the host's firewall:

```yaml
services:
  burrow:
    ports:
      - "7000:7000"
      - "9001:9001"   # one line per fixed TCP port; or "9000-9100:9000-9100"
```

```sh
ufw allow 9001/tcp
```

- Use fixed ports (`--remote 9001`) so that you know which lines to add.
- On Coolify, add the ports in the Compose file of the service, not in the
  proxy: Coolify's Traefik carries the dashboard, not raw TCP. TLS traffic by
  host name is the exception; see
  [Behind a proxy](#behind-a-proxy-coolify-and-traefik).
- Docker's userland proxy hides the visitor's address. See
  [Things that go wrong](#things-that-go-wrong), and check the connection log
  after the first connection.

---

## Troubleshooting

**`not_found` ("tcp service ... not found, or you may not reach it").** The
target is wrong, or you are not allowed. Check the slug on the service's page;
ask the owner to allow your role under **Who may forward**. The relay gives
the same answer for both. The owner may use the service's name as the
target; everyone else uses the slug or the id.

**`offline` ("... is offline: no client serves it right now").** The service
exists but nothing at home is connected, or it did not answer within five
seconds. Check that `burrow tcp` or `burrow service status` is running at
home, and that the machine is awake.

**`rate_limited`.** You or your service hit a limit from
[Limits](#limits-and-ports), or a visitor was refused. `burrow forward` waits
five seconds and then tries again. Look at the connection log for the reason.

**`access_invalid`, "restart with --access".** The dashboard has a mode that is
more open than the one your client asked for. Restart the client with
`--access <mode>` or without `--access`, or set the dashboard back. See
[Changing the mode](#changing-the-mode). Other `access_invalid` texts say that
the relay cannot apply a mode: "a tcp tunnel needs a name to have an access
mode", or "this relay cannot apply an access mode to a tcp tunnel".

**`service_busy` or `name_in_use`.** `service_busy` means the same token still
has an older session on the service; the client retries and it goes away in
a moment. `name_in_use` means another machine serves that name, or the name
belongs to a service of another type. Choose another `--name`.

**"relay too old".** The relay does not know TCP access modes or forwards.
`burrow tcp --access private` then serves nothing and opens no port, on
purpose. `burrow forward` stops with "relay too old". Update the relay, or,
for a public port, run `--access open` knowing what that means.

**"local port ... is in use".** Something already listens on 13389 (often a
second `burrow forward`). Pick another: `burrow forward <slug> 13390`, or set
`local_port` in `burrow.yaml`. A forward may not use the `local:` address of a
service in the same file.

**The remote desktop client connects and then closes.** Check from the
laptop that the forward is up (`burrow forward` shows `connected`), then check
at home that the desktop listens: `ss -ltn | grep 3389`. If the home machine
runs GNOME Remote Desktop, make sure a desktop session is signed in or that
remote login is on.

**Nothing is reachable on an `ip` service from my address.** Check the
address family of the grant and the `source_ip` of the refused row in the
connection log. See [Things that go wrong](#things-that-go-wrong).

See [Troubleshooting](/guide/troubleshooting) for relay and sign-in problems.
