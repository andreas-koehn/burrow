# Remote desktop and other TCP services

This guide shows how to reach a remote desktop (RDP), SSH, a database or any
other TCP service on a machine behind your router, through a Burrow relay.
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

`burrow tcp` is private by default, so this is the same as
`burrow tcp 3389 --name rdp --access private`. The command stays in the
foreground and prints the way in:

```
This service is private: the relay opened no public port. Reach it from another machine with: burrow forward t3f9a1c07be 13389
```

The word after `forward` is the service's **slug**. The relay makes it for
you. It is also shown on the service's page in the dashboard.

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

`access: private` is worth writing out. **An entry without `access:` asks the
relay for nothing and keeps the stored mode. A service created that way is
`open`, on a public port.** Do not leave `access:` out for RDP, SSH, VNC or
databases. `access: private` together with `remote:` is refused, because a
private service has no port.

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
burrow forward t3f9a1c07be 13389
```

Replace `t3f9a1c07be` with your service's slug. Leave the window open. The
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
schtasks /Create /TN "Burrow forward rdp" /SC ONLOGON /RL LIMITED /TR "\"%LOCALAPPDATA%\Programs\burrow\burrow.exe\" forward t3f9a1c07be 13389"
```

Or list the forward in a `burrow.yaml` and run `burrow up`:

```yaml
forwards:
  - target: t3f9a1c07be   # slug, own service name or id
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
burrow forward t3f9a1c07be 13389
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
- **More open** (private to ip or open): the client has to be restarted with
  `--access <mode>`. A client that was started with a stricter `--access` stops
  with `service is <mode> in the dashboard; restart with --access <mode> to
  serve it`.

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
do not apply to TCP services. A CIDR on a TCP service may not be wider than a
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

On port 7000, each source address may fail 10 sign-ins and then gets one more
chance every 30 seconds. A valid token is still accepted from a limited
address.

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
The next connection fails until the user has a valid token again.

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
| `tcp.forward.open` | a user opened a forward. **Sampled:** one row per user and service per minute |

The connection log has every connection:

- `tcp_proxy` rows for visitors of open and ip services, with source address,
  bytes and duration.
- `tcp_forward` rows for forwards, with the user, the token name and the
  forwarder's address.
- Refusals as `rejected` rows with a reason: `ip_geo`, `rate_limited`,
  `source_limit`, `tunnel_full`, `pending_full`, `no_address`, `forbidden` (a
  forward the user may not make) and `user_limit`. They are **sampled**: one
  row per service, reason and address per minute, so a scan does not fill the
  log. The metric `burrow_connections_total` counts all of them.

The webhook event `access.denied` is sent for TCP refusals too, at most one
per service and reason per minute. See [Operations](/guide/operations).

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
  proxy: Coolify's Traefik carries the dashboard, not raw TCP.
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
`--access <mode>`, or set the dashboard back. See
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
