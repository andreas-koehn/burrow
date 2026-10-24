# Burrow and ngrok

A feature-by-feature look at HTTP/2, gRPC and TLS passthrough, for people who
know ngrok. ngrok column is what its documentation describes; check it for
your plan. The Burrow column is what ships in this release.

| Capability | ngrok | Burrow |
|---|---|---|
| HTTP/2 from visitors | Yes | Yes. TLS ALPN, plus h2c behind a TLS-terminating proxy (`BURROW_HTTP_H2C`, on by default) |
| HTTP/2 to the local service | `upstream.protocol: http2` (h2c for `http://`, h2 for `https://`) | `upstream_protocol` `h2c` or `h2`, plus `https` for HTTP/1.1 over TLS. Per service, set by the client or the API |
| Upstream TLS verification | Opt-in `--upstream-tls-verify`, `--upstream-tls-verify-cas` | Opt-in `upstream_tls_verify`, `upstream_tls_ca_pem`, `upstream_tls_server_name` |
| gRPC on HTTP endpoints | Yes | Yes: streaming, trailers, deadlines (`grpc_max_duration_s`), a per-call log row with `grpc-status`, `burrow_grpc_requests_total`. Needs a custom domain |
| Refusals as gRPC status | Not documented | Yes: access, rate limit, not found, offline tunnel and dial errors reach the caller as a gRPC status |
| grpc-web translation | Not documented | No. Pass-through to an upstream that speaks grpc-web |
| gRPC with AI or content controls | Not applicable | Refused (`PERMISSION_DENIED`) where redaction, guardrails, routing or the anthropic section is on |
| TLS passthrough by SNI on 443 | TLS endpoints (`ngrok tls`) on 443 by host name | Yes, shared with HTTPS on 443 (`BURROW_TLS_PASSTHROUGH_ON_HTTPS`), or on a dedicated port (`BURROW_TLS_PASSTHROUGH_LISTEN`) |
| TLS termination for TLS endpoints at the edge or agent | Yes (`terminate-tls`, agent TLS termination) | No. A tcp service is passthrough or raw only |
| Raw TCP | TCP endpoints on assigned addresses | Ports, host names (passthrough) and `burrow forward` |
| PROXY protocol to the upstream | v1 and v2 | v2. Inbound v2 from trusted proxies, also on 443 |
| Automatic host names | Generated and wildcard domains, certificates included | Tcp: `<slug>.<tcp domain>` (wildcard DNS, no certificate needed). Http: explicit custom domains, with `auto` certificates under ACME. Automatic `<slug>.` http names are not built |
| Auth at the edge | OAuth/OIDC, JWT, basic auth, IP rules, mTLS via traffic policy | `api_key` (read from gRPC metadata), `burrow_login` (browsers only), mTLS on `:8443` vhosts, ip and country rules, rate limits |
| Policy engine | Traffic Policy (CEL expressions, actions) | Fixed per-service settings |
| Load balancing across agents | Endpoint pooling | One tunnel per service |
| Request inspection | Inspector and replay | Inspector and replay for HTTP. gRPC calls are not recorded |
| Upstream connection reuse | Multiplexed agent sessions | One tunnel stream per request or call. No pooled HTTP/2 transport |
| Hosting | Global edge, SaaS | A single self-hosted relay, your own data, SQLite or Postgres, AI and MCP gateways built in |
| UDP | No | No |

Differences to plan for:

- **gRPC needs a name.** A gRPC call cannot use a path prefix, so the service
  needs a custom domain. With `cert_source: auto` Burrow obtains the
  certificate (ACME), or none is needed behind a proxy that holds it. See
  [gRPC](/guide/grpc).
- **One relay.** Burrow does not pool several agents behind one endpoint and
  has no edge network. Latency is the path to your own server.
- **Mux or proxy.** On a host where Burrow owns `443`, one setting shares the
  port between HTTPS and tcp passthrough. Behind Coolify and Traefik the proxy
  is the mux. See [One port 443](/guide/deploy#one-port-443).
