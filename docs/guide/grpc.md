# gRPC and HTTP/2

Burrow serves gRPC through an http service: unary, server, client and
bidirectional streaming, with trailers, no body buffering and correct
deadlines. The relay speaks HTTP/2 to the caller and, per service, HTTP/1.1,
HTTP/1.1 over TLS, h2c or h2 to the local service.

## Quick start

On the machine that runs the gRPC server:

```sh
burrow grpc 50051
```

`burrow grpc` is `burrow http` with the upstream protocol `h2c`: the relay
speaks plain HTTP/2 to `127.0.0.1:50051`. For a server that speaks TLS use
`burrow grpc https://localhost:50443` (upstream protocol `h2`). See the
[CLI reference](/reference/cli#grpc).

Then give the service a host name. A gRPC client cannot use the
`/svc/<slug>/` prefix, because the method is the request path
(`/pkg.Service/Method`). The name is a **custom domain** of the http service:

```sh
curl -X POST https://burrow.insingo.com/api/v1/services/SERVICE_ID/domains \
  -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -d '{"hostname":"grpc.example.com","cert_source":"auto"}'
```

Point `grpc.example.com` at the relay in DNS (an A record), or at the proxy in
front of it. The address for the caller is `grpc.example.com:443`.

### Certificates: `cert_source`

| `cert_source` | Certificate | When |
|---------------|-------------|------|
| `auto` | None uploaded. Under ACME the relay obtains it itself (HTTP-01 on `:80` or TLS-ALPN-01 on `443`) when the domain is bound and again at start. On a plain dashboard listener, a proxy in front holds the certificate and none is needed. | Default choice for gRPC. |
| `uploaded` (default) | `cert_pem` and `key_pem` are sent with the domain. | A wildcard domain, or a certificate you manage. |

- `auto` is **for administrators**: it needs `services:configure:any`. A user
  with `services:configure:own` gets 403 (`automatic certificates need an
  administrator; upload a certificate instead`) and uploads a certificate,
  which proves control of the name. The dashboard offers only the upload to
  such users.
- `auto` names are capped: at most `BURROW_ACME_CUSTOM_MAX` on the relay
  (default 100) and `BURROW_ACME_CUSTOM_PER_USER` per service owner (default
  10). `0` allows none. Over a cap the write gets 429 naming the variable.
  Uploaded domains do not count. See
  [Configuration](/guide/configuration#acme-automatic-tls).
- A wildcard custom domain needs `uploaded`.
- A name under another service's wildcard custom domain, a wildcard over
  another service's name, and nested wildcards across services are refused
  (409, `hostname overlaps a wildcard custom domain of another service…`),
  whatever the `cert_source`. A service's own names may overlap.
- `auto` is refused (400) when the dashboard serves a file certificate without
  ACME: nobody could serve that name. Use `uploaded`, or ACME.
- `auto` takes no certificate. Sending `cert_pem` or `key_pem` with it is
  refused (400).
- The response carries `cert_source` and `cert_status`: `ready` (uploaded, or
  an auto certificate the relay holds), `pending` (still being obtained, or
  the relay cannot) and `not_needed` (auto behind a proxy that terminates TLS).
  A failed order is logged and audited as `service.custom_domain.cert_failed`,
  once per host name and hour. Make sure DNS points at the relay and that
  `:80` or `443` is reachable before you bind the name.
- At start the relay queues the orders of every auto domain, two seconds apart.
- The relay sends a request to a custom domain's service by its `Host` header
  once it knows a name of its own: `BURROW_AUTH_DOMAIN`, ACME, or a file
  certificate. Without one it leaves custom domains to the host-routed ingress.
  **Behind Traefik (a plain dashboard listener) set `BURROW_AUTH_DOMAIN`**, or
  gRPC calls to the custom domain reach the dashboard's router instead of the
  service. Coolify's template sets it.
- Single-label names and IP addresses are refused as custom domains (400). An
  IDN must be given as punycode.

In the dashboard, the **Host names** card of an http service lists its custom
domains with the certificate source, the certificate status and the gRPC
address to copy (`grpc.example.com:443`), and adds or removes them.

Adding or changing a custom domain asks for a recent password confirmation and
is not an MCP tool, as before. A name that a tcp service uses is refused (409);
see [One owner per host name](/guide/deploy#one-port-443).

Behind Coolify and Traefik, see
[Coolify and Traefik: gRPC](/guide/deploy#traefik-grpc).

## Call it

```sh
grpcurl -H "authorization: Bearer buk_YOUR_API_KEY" \
  grpc.example.com:443 grpc.health.v1.Health/Check
```

With a server that offers reflection, `grpcurl grpc.example.com:443 list` works
too.

An `open` service needs no header. For the other access modes see
[Access control](#access-control).

## Upstream protocol

The upstream protocol says how the relay speaks to the local service. It is set
per http service, with exactly four values:

| Value | The relay speaks | Use for |
|-------|------------------|---------|
| `http1` (default) | HTTP/1.1, plain | Web apps |
| `https` | HTTP/1.1 over TLS (ALPN `http/1.1`) | A local HTTPS app |
| `h2c` | HTTP/2 prior knowledge, plain | A gRPC server without TLS |
| `h2` | HTTP/2 over TLS (ALPN `h2` only); the request fails if the service does not negotiate h2 | A gRPC server with TLS |

Where it comes from:

- The client names one at registration (`burrow grpc`, `burrow http
  --upstream-protocol …`, an `https://` or `h2c://` target, or
  `upstream_protocol` in `burrow.yaml`). That value applies to its tunnel and
  the API shows it as `client_upstream_protocol`. It is not stored.
- Otherwise the stored setting applies, which you change with
  `PUT /api/v1/services/{id}/http-settings`.
- `effective_upstream_protocol` in `GET …/http-settings` is the one in force.

In the dashboard, the **Protocol** card of an http service sets the upstream
protocol, the TLS fields for `https` and `h2` and the gRPC maximum duration,
and shows the protocol the client set and the one in effect. It also says when
`burrow_login` is the access mode (browsers only), when the inspector is on
(gRPC calls are not recorded), when AI controls are switched on (gRPC is
refused) and when the relay has h2c off.

A native gRPC call to a service whose effective protocol is `http1` or `https`
is answered by the relay with `UNAVAILABLE` and the message `service upstream
is HTTP/1.1; set upstream_protocol h2c or h2 for gRPC`. Nothing is upgraded
silently and nothing is probed. A WebSocket upgrade always goes upstream as
HTTP/1.1 (plain for `h2c`, TLS for `h2`).

The AI gateway and the MCP gateway reach a service by the same protocol, so a
service set to `h2c` or `h2` keeps working through them.

The relay opens one tunnel stream per request or call. A client older than
this release still serves h2c and h2 services, since it only pipes bytes, but
it cannot choose a protocol: set it in the dashboard or API. A client that asks
for a protocol from a relay that is too old to know it registers nothing and
stops with "relay too old".

### Upstream TLS: https and h2

For `https` and `h2` the relay does the TLS handshake over the tunnel stream.
Local services mostly have self-signed certificates and the hop already runs
inside the tunnel's TLS, so verification is **off by default**.

| Field | Meaning |
|-------|---------|
| `upstream_tls_verify` | `false` (default). `true` verifies against the system roots. |
| `upstream_tls_ca_pem` | Optional PEM CA. When set, verification is on against that CA only. Write only; the read returns `upstream_tls_ca_sha256`. At most 64 KiB, a PEM `CERTIFICATE` block. |
| `upstream_tls_server_name` | Optional DNS name for SNI and verification. Default: the host of the service's local address when that is a name. With an IP address no SNI is sent. An IP is refused here. |

A handshake, certificate or ALPN failure is a dial failure: 502 for HTTP,
`UNAVAILABLE` for gRPC, with `closed_error` and reason `upstream_tls` in the
connection log. The handshake is bounded to 10 seconds.

```sh
curl -X PUT https://burrow.insingo.com/api/v1/services/SERVICE_ID/http-settings \
  -H "Authorization: Bearer bua_YOUR_TOKEN_HERE" \
  -H "Content-Type: application/json" \
  -d '{"upstream_protocol":"h2","upstream_tls_verify":true,"upstream_tls_server_name":"app.internal","grpc_max_duration_s":600}'
```

The write needs `services:configure:own` or `services:configure:any`. It does
not ask for a recent password confirmation, since nothing in it widens who can
reach the service. It is **not an MCP tool**, because it sets a trust anchor and
can switch verification off; the read is. It is audited as
`service.http_settings.update` with the state before and after (the CA as a
fingerprint). It applies only to http services (409 for a tcp service).

## Deadlines

- `grpc-timeout` is forwarded unchanged. The relay's own deadline for the
  call is `grpc-timeout` plus one second. When it fires before the response
  started, the caller gets `DEADLINE_EXCEEDED` as a trailers-only reply; after
  that, the stream is reset. A gRPC server usually ends the call itself when
  the forwarded `grpc-timeout` runs out (grpc-go resets the stream); once the
  call's timeout has passed, the relay answers that reset with
  `DEADLINE_EXCEEDED` too, not `UNAVAILABLE`.
- `grpc_max_duration_s` (0 = off, otherwise 1 to 86400) caps every call,
  streams included. A `grpc-timeout` that is missing, malformed or longer than
  the cap is rewritten down to the cap, so the upstream knows its deadline too.
- A call with no `grpc-timeout` and no cap has no relay deadline. Dead peers
  are found by HTTP/2 pings and the tunnel heartbeat. An absurdly large
  `grpc-timeout` means no deadline.
- The relay's idle timeout for proxied requests does not apply to gRPC calls.

## Refusals as gRPC status

For `application/grpc` and `application/grpc-web` requests, any answer that is
not 200 and has no `grpc-status` becomes a trailers-only reply: HTTP 200, the
request's content type, `grpc-status` and `grpc-message`, no body. This covers
refusals by the relay (ip or country rules, access, rate limit, not found,
gateway-only, dial error) and non-200 answers of the upstream. `Retry-After`
is kept.

| HTTP | gRPC status |
|------|-------------|
| 400 | `INTERNAL` (13) |
| 401 | `UNAUTHENTICATED` (16) |
| 403 | `PERMISSION_DENIED` (7) |
| 404 | `UNIMPLEMENTED` (12) |
| 429 | `RESOURCE_EXHAUSTED` (8) |
| 502, 503, 504 | `UNAVAILABLE` (14) |
| any other | `UNKNOWN` (2) |

429 is not the usual `UNAVAILABLE`: a rate limit is not a retryable outage.

- **An offline tunnel** (the service exists, its client is not connected) is
  `UNAVAILABLE`, not `UNIMPLEMENTED`, so clients retry or wait. An unknown
  host or path is `UNIMPLEMENTED`.
- **`burrow_login`** answers a gRPC call with `UNAUTHENTICATED` and "this
  service needs a browser sign-in; use access mode api_key for gRPC". The mode
  stays allowed for a service that serves browsers and gRPC both.
- **The upstream protocol is `http1` or `https`:** `UNAVAILABLE`, see above.
- **A request on `/svc/` with a bad slug** gets `UNIMPLEMENTED`.
- A call carrying `application/connect+…` keeps its HTTP status; only `grpc`
  and `grpc-web` get gRPC-shaped replies.

## Access control

`api_key` reads the key from metadata exactly as from a header:
`authorization: Bearer …`, or the service's configured header. mTLS works only
where the relay terminates TLS itself for the service (the host-routed ingress
on `:8443`), not on a custom domain and not behind Traefik. See
[Access control](/guide/access-control).

The IP and country rules, the rate limit and the daily quota apply per call.

## grpc-web

grpc-web requests (`application/grpc-web…`) take the same unbuffered path and
the same gRPC-shaped refusals. Burrow does **not** translate grpc-web to gRPC:
no body-framed trailers, no text mode, no CORS preflight handling. An upstream
that speaks grpc-web itself (Connect, Envoy, `grpcwebproxy`) works through
Burrow on any upstream protocol. The connection log reads the final status of
a binary grpc-web response from its trailers frame; for text mode it keeps the
HTTP outcome.

## gRPC on services with AI controls is refused

The AI gateway reads, redacts, judges and rewrites request bodies. That cannot
run on a stream of gRPC messages. Letting such a call bypass the controls
would switch them off silently, and the content type is chosen by the caller,
so a spoofed `application/grpc-web` header would be a way around redaction. A
gRPC-like call (`grpc`, `grpc-web` or `connect`) to a service where redaction,
request or answer guardrails, routing with backends or the anthropic section is
**switched on** is therefore refused with `PERMISSION_DENIED`:

> this service applies AI controls (redaction, guardrails, routing or the anthropic section) that cannot run on gRPC; remove them or serve gRPC from a separate service

A section that is present but switched off does not count. A service with only
the inspector, the cache or nothing serves gRPC unbuffered. Such calls skip
the chain: no body limit, no meter, no inspection, and **the inspector never
records gRPC calls**. The service's upstream credential is still added as a
header, and the rate limit and quota still count each call.

## Observability

The connection log has one row per call, with:

- `protocol`: `grpc` or `grpc-web` for those calls, `h2` or `http/1.1` (the
  inbound protocol) for other http rows;
- `grpc_method`: the request path, at most 256 bytes;
- `grpc_status`: the call's `grpc-status`, `0` for OK, empty for other rows.

`GET /api/v1/connection-logs` and its ndjson export return `protocol`,
`grpc_method` and `grpc_status` (`null` for other rows); the CSV export has the
columns `protocol`, `sni`, `grpc_method` and `grpc_status`. In the dashboard,
the Traffic table has a gRPC column (code and name, the method on hover) and
the row details show the method and status.

The status is read from a trailers-only reply, an announced trailer or a
trailer sent after the body. The codes 2, 4, 13, 14 and 15 make the row
`closed_error`, as a 5xx does. A stream that ends without any `grpc-status` is
`closed_error` with reason `grpc_no_status`. A refusal by the relay is a row
too, with the code the caller saw. A call to an offline service has no tunnel
to log against and writes no row (as an offline HTTP request never did); the
relay log has it at debug level.

Prometheus has `burrow_grpc_requests_total{service,code}` with the code by name
(`OK`, `UNAVAILABLE`, …) or `none`. The `burrow_http_requests_total` family
keeps its labels; there `status` is the HTTP status the relay or upstream chose
before the conversion. The relay log has one debug line per call: service,
method, code and duration. A webhook that fires on an access denial still sees
the HTTP status. See [Operations](/guide/operations#metrics).

The status view of the client (`burrow grpc`, `burrow http`) shows `grpc N` in
place of the HTTP status.

## Limits and caveats

- One tunnel stream per call. A pooled HTTP/2 transport per tunnel is not
  built.
- A response with a `Content-Length` and unannounced trailers cannot carry the
  trailers to an HTTP/1.1 caller. gRPC servers stream and HTTP/2 callers work.
- Not built: grpc-web translation, gRPC payload inspection, redaction or
  metering, server push, WebSocket over HTTP/2, HTTP/3.
- Compare with ngrok: [Burrow and ngrok](/guide/compare-ngrok).
