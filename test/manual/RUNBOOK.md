# Burrow e2e full harness — manual runbook

Walk-through checklist that proves every v0.1–v0.5 UI surface works against the live 4-container stack from Plan 1. Each section:
1. Sets up state (URL, login, prerequisite).
2. Describes the click-path.
3. Asserts expected UI.
4. Captures findings (✅ pass / ⚠ gotcha / ❌ defect).

Plan 3's Playwright suite codifies each section as an automated spec.

## Conventions

- **Browser:** any Chromium-based (Chrome, Edge, Brave). Firefox/Safari out of scope.
- **Admin dashboard:** `http://localhost:8080/`.
- **Visitor-facing surfaces:** `http://localhost:8080/svc/<slug>/` (path URL on the dashboard origin) and, because this harness turns on the opt-in host-routed ingress, `https://<slug>.test.local:8443/` (wildcard cert mounted into the relay container). HTTP services get a random slug at registration time — read it from `docker logs burrow-e2e-full-relay-1 | grep "http tunnel registered"`).
- **TCP tunnels:** `http://localhost:<port>/...` where `<port>` is the `--remote` value (9002 for tcp-echo, 9003/9004 for multi-svc-a/b).
- **Login:** `admin@e2e.local` / `e2e-pass` (seeded by `relay-full.sh`).
- **Reset between sections:** `curl -X POST http://localhost:8080/api/v1/internal/test-reset` (build-tagged endpoint from Plan 1 T18). Truncates audit, tokens, sessions, services + per-service rows, webhooks, connection-logs, rate limits, budgets, etc. — preserves seeded admin + schema migrations. ⚠ After /test-reset, cached tokens on `/run/burrow/token-{ai,tcp,multi}` become invalid until the relay re-mints them; the cleanest follow-up is `docker compose restart` to re-seed.

## Pre-flight

Bring up the stack and verify the proven smoke shape before starting:

```bash
docker compose -f test/harness/compose.full.yml up -d --wait

# Discover the AI service's slug:
AI=$(docker logs burrow-e2e-full-relay-1 2>&1 \
  | grep "http tunnel registered" | tail -1 \
  | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
echo "AI slug: $AI"

# Proven 6-surface smoke (must all pass before starting):
curl -fsS http://localhost:8080/healthz                       # [1/6] dashboard
curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
     -fsS -X POST -H "content-type: application/json" \
     -d '{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
     "https://$AI.test.local:8443/v1/chat/completions"        # [2/6] AI HTTP tunnel SSE
curl -fsS http://localhost:9002/healthz                       # [3/6] TCP echo
curl -fsS http://localhost:9003/healthz                       # [4/6] multi svc-a
curl -fsS http://localhost:9004/healthz                       # [5/6] multi svc-b
curl -fsS -X POST http://localhost:8080/api/v1/internal/test-reset  # [6/6] 204
```

⚠ The original plan's pre-flight references `curl http://localhost:9001/healthz`. That is incorrect: the AI tunnel is `--type http` and is host-routed on :8443, not port-bound on :9001. Use the `$AI.test.local:8443` variant above.

Optional — add to `/etc/hosts` (or `C:\Windows\System32\drivers\etc\hosts` on Windows; requires admin) for browser-driven testing of host-routed surfaces:
```
127.0.0.1 relay.test.local client-ai.test.local client-tcp.test.local client-multi.test.local test.local
```

Optional — trust the test CA at the OS level to eliminate browser warnings:
- Linux: `sudo cp test/harness/certs/ca.crt /usr/local/share/ca-certificates/burrow-test-ca.crt && sudo update-ca-certificates`
- macOS: `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain test/harness/certs/ca.crt`
- Windows: import `certs\ca.crt` into "Trusted Root Certification Authorities" via certmgr.msc

## Table of contents

1. [Bootstrap (login, password change, sidebar nav)](#1-bootstrap)
2. [Tunnels (3 clients visible, bytes flow)](#2-tunnels)
3. [Services (burrow.yaml multi-service)](#3-services)
4. [Tokens (UI mint + use)](#4-tokens)
5. [Users + Roles (CRUD)](#5-users--roles)
6. [Access modes (open, api_key, burrow_login; mTLS via API)](#6-access-modes)
7. [AI Gateway basic (chat-completions, metering, rate limit, cost)](#7-ai-gateway-basic)
8. [AI Gateway depth (semantic cache, guardrail, redaction)](#8-ai-gateway-depth)
9. [Custom domains (removed from the dashboard)](#9-custom-domains)
10. [Connection logs (drive TCP traffic → entry + NDJSON export)](#10-connection-logs)
11. [Audit + Webhooks + OpenAPI viewer + Retention](#11-audit--webhooks--openapi--retention)
12. [Postgres swap (compose.postgres.yml profile)](#12-postgres-swap)
13. [Reconnect (restart relay container)](#13-reconnect)

## Sign-off

After all 13 sections pass, append:

> Run-through completed on YYYY-MM-DD by &lt;NAME&gt; against commit &lt;SHA&gt;. All sections ✅ except &lt;list&gt;. Findings filed at &lt;issues&gt;.

---

## 1. Bootstrap

**Goal:** Confirm login + sidebar nav + password change flow.

### Steps
1. Open `http://localhost:8080/` → expect redirect to `/login`.
2. Sign in: `admin@e2e.local` / `e2e-pass`.
3. Land on `/tunnels`. Sidebar shows: Tunnels, Services, Tokens, Clients, Users, Roles, AI endpoints, Cache, Guardrails, Cost, Audit log, Webhooks, Settings.
4. Click avatar → Account → Change password → new password `e2e-pass-2` → save → toast "Password updated".
5. Sign out → sign in with new password → success.
6. Reset for subsequent sections: `curl -X POST http://localhost:8080/api/v1/internal/test-reset` (password reverts to seeded). ⚠ After reset, `docker compose restart` is the cleanest way to also re-seed the on-disk token files so tunnels reconnect cleanly.

### Expected ✅
- Dashboard renders without console errors (DevTools Console clean).
- All sidebar links route correctly.

### Common gotchas ⚠
- Session cookie survives test-reset (admin user is re-seeded with the same ID). If logged out, re-login.
- The exact sidebar item list is set in the SPA; the textual labels above are illustrative. Mismatches should be captured in Findings, not treated as a defect of the runbook.

### Findings
- [ ] ✅ / ⚠ / ❌ — fill in during run-through.

---

## 2. Tunnels

**Goal:** All 3 client containers + their 4 tunnels (1 from client-ai, 1 from client-tcp, 2 from client-multi) appear on `/tunnels` with status=connected; bytes counters move under traffic.

### Steps
1. Navigate `/tunnels`. Confirm 4 rows:
   - `ai` (http; random slug, e.g. `/svc/qgnh4v/` — no fixed port)
   - `tcp-echo` (tcp; remote :9002)
   - `svc-a` (tcp; remote :9003)
   - `svc-b` (tcp; remote :9004)
2. All 4 rows show `connected` badge.
3. Initial `In` and `Out` values are small (≤ a few KB from the connect handshake).
4. Drive traffic from a shell:
   ```bash
   for i in 1 2 3 4 5; do curl -fsS http://localhost:9002/healthz; done
   AI=$(docker logs burrow-e2e-full-relay-1 | grep "http tunnel registered" | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS -X POST -H "content-type: application/json" \
        -d '{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
        "https://$AI.test.local:8443/v1/chat/completions" > /dev/null
   ```
5. Within 5s the `tcp-echo` row's `In`/`Out` cells should increase (SSE event `tunnels` fires); the `ai` row should also tick up.

### Expected ✅
- 4 rows visible.
- All connected.
- Bytes counters move within 5s of driving traffic.

### Gotchas ⚠
- If `connected` badge doesn't appear, check `docker compose logs client-ai|client-tcp|client-multi` — the client may be in backoff retry loop (typically after a /test-reset wiped tokens).
- HTTP tunnels (`ai`) do NOT have a fixed `--remote` port — they are reached at `/svc/<slug>/` (and host-routed on :8443 in this harness). The "Remote" column renders `—`; the "URL" column shows the path.

### Findings
- [ ]

---

## 3. Services (burrow.yaml multi-service)

**Goal:** Confirm v0.3 burrow.yaml multi-service surface — `client-multi` exposes 2 services through one client process.

### Steps
1. Navigate `/services`. Two rows under the `client-multi` client: `svc-a` (remote :9003) and `svc-b` (remote :9004).
2. Click `svc-a` → service detail page renders (per-service tabs: Overview, Access, AI config if applicable, Connection logs).
3. Drive traffic through both:
   ```bash
   curl -fsS http://localhost:9003/healthz
   curl -fsS http://localhost:9004/healthz
   ```
4. Both services' bytes counters move independently.

### Expected ✅
- Both services visible under one client.
- Per-service bytes counters move.

### Gotchas ⚠
- The basic upstream binary (`test/harness/upstream/main.go`) only exposes `/healthz` and `/echo`. Other paths return 404 from upstream; that is upstream behavior, not a tunnel defect.

### Findings
- [ ]

---

## 4. Tokens

**Goal:** UI mint creates a `bur_*` token + appears in list; token authenticates a CLI invocation.

### Steps
1. Navigate `/tokens`. Form field: `Token name`. Existing tokens visible (from `relay-full.sh`: `e2e-ai`, `e2e-tcp`, `e2e-multi`).
2. Mint `e2e-manual-runbook` → reveal dialog → copy plaintext `bur_xxx`. ⚠ The plaintext is shown ONCE; subsequent views show only the prefix.
3. From a shell, use the token to open a new tunnel:
   ```bash
   docker run --rm -d --name burrow-test \
     --network burrow-e2e-full_e2e \
     burrow-e2e-client-tcp:dev \
     /bin/sh -c "/usr/local/bin/upstream -addr :8090 &
                 burrow connect --server relay.test.local:7000 \
                   --token bur_xxx --local 127.0.0.1:8090 \
                   --remote 9099 --name runbook --type tcp --insecure"
   ```
   (Replace `bur_xxx` with the minted plaintext.)
4. Within 5s, `runbook` appears on `/tunnels` with status=connected.
5. Cleanup: `docker rm -f burrow-test`.

### Expected ✅
- Mint succeeds + dialog reveals `bur_*`.
- New token appears in list.
- Token authenticates a real `burrow connect` from a sidecar container on the e2e network.

### Gotchas ⚠
- :9099 is NOT published to the host — it's only reachable from inside the `burrow-e2e-full_e2e` network. Confirming connectivity requires `docker exec ... curl` against another container, or just observing the `connected` badge in the UI.

### Findings
- [ ]

---

## 5. Users + Roles

**Goal:** v0.2 user CRUD + v0.4 custom roles work end-to-end.

### Steps
1. `/users` → "Invite user" → email `bob@e2e.local`, role `viewer`, save → toast "Invitation sent" (SMTP unconfigured by default — the row should still land with status `invited`; the email send itself may fail silently if SMTP isn't wired).
2. Bob's row visible with role=viewer + status=invited.
3. Suspend Bob → status flips to suspended → page reload preserves.
4. Reactivate → status=active (or invited).
5. Delete Bob → row removed.
6. `/roles` → "New role" → name `auditor`, permissions: `audit:read`, `audit:verify`, `service:read`, save.
7. Re-invite Bob with role=`auditor` → confirm role assigned.
8. Cleanup: delete Bob; delete `auditor` role.

### Expected ✅
- All user CRUD operations succeed without console errors.
- Custom role assignment persists across page reload.

### Gotchas ⚠
- The exact permission catalog is set in `internal/authz/`. If a permission name in the role-creation form doesn't match, the role will save but won't grant the implied access. Cross-check against `internal/authz/perms.go` if uncertain.
- If SMTP isn't configured, the invite email isn't sent but the user row IS created. Use `bob@e2e.local` (a sink address) and don't expect a real email.

### Findings
- [ ]

---

## 6. Access modes

**Goal:** The 3 dashboard access modes (open, api_key, burrow_login) and API-only mTLS work end-to-end against the HTTP tunnel from `client-ai` (configured on the `ai` service).

Discover the AI service's slug first (same as pre-flight):
```bash
AI=$(docker logs burrow-e2e-full-relay-1 | grep "http tunnel registered" | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
echo "AI slug: $AI"
```

### 6a. Open
1. `/services` → click the `ai` row → Access tab → mode "Open" → save.
2. From a shell:
   ```bash
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS https://$AI.test.local:8443/healthz
   ```
   Expect 200.
3. Findings ✅

### 6b. API key
1. Same Access tab → mode "API key" → reveal generated key → copy (`bua_xxx` shape).
2. From shell:
   ```bash
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS https://$AI.test.local:8443/healthz \
        -H "Authorization: Bearer <key>"
   ```
   → 200.
3. Without header: same curl minus `-H` → 401 with JSON `{"error":"unauthorized"}` (or similar).
4. Findings ✅

### 6c. Burrow login (SSO)
1. Access tab → mode "Burrow login" → save. Requires `BURROW_AUTH_DOMAIN` configured (relay container ships with `test.local`; check `docker exec burrow-e2e-full-relay-1 printenv BURROW_AUTH_DOMAIN`).
2. From browser (incognito): open `http://localhost:8080/svc/<slug>/` → expect the login gate on the same origin.
3. Sign in with admin creds → you land on `/svc/<slug>/` and the page loads.
4. Findings ✅

### 6d. mTLS (REST API only, host-routed ingress)
The dashboard has no mTLS controls. The mode is set through the REST API and only works on the opt-in host-routed ingress (`BURROW_HTTP_PROXY_LISTEN`, `:8443` in this harness); without that ingress the API refuses `mtls`.
1. Access tab of the `ai` service: only "Open", "API key" and "Burrow login" are offered.
2. Generate a client cert against the test CA:
   ```bash
   cd test/harness/certs
   MSYS_NO_PATHCONV=1 openssl req -new -newkey rsa:2048 -nodes \
     -keyout client.key -out client.csr \
     -subj "/C=US/O=Burrow Test/CN=e2e-mtls-client"
   MSYS_NO_PATHCONV=1 openssl x509 -req -in client.csr -days 365 \
     -CA ca.crt -CAkey ca.key -CAcreateserial -out client.crt
   rm client.csr ca.srl
   cd -
   ```
3. Set the mode with the CA as trust anchor (Burrow does NOT sign client certs):
   ```bash
   JAR=$(mktemp)
   curl -s -o /dev/null -c "$JAR" -X POST http://localhost:8080/api/v1/auth/login \
        -H 'Content-Type: application/json' -d '{"email":"admin@e2e.local","password":"e2e-pass"}'
   CSRF=$(grep burrow_csrf "$JAR" | awk '{print $NF}')
   SVC=$(curl -s -b "$JAR" http://localhost:8080/api/v1/services \
     | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>console.log(JSON.parse(d).find(x=>x.name==="ai").id))')
   node -e 'console.log(JSON.stringify({access_mode:"mtls",mtls_ca_pem:require("fs").readFileSync("test/harness/certs/ca.crt","utf8")}))' \
     | curl -s -o /dev/null -w '%{http_code}\n' -b "$JAR" -H "X-CSRF-Token: $CSRF" \
        -H 'Content-Type: application/json' -X PUT -d @- \
        "http://localhost:8080/api/v1/services/$SVC/access-mode"
   ```
   → 204.
4. With the client cert, over the host-routed ingress:
   ```bash
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS https://$AI.test.local:8443/healthz \
        --cert test/harness/certs/client.crt --key test/harness/certs/client.key
   ```
   → 200.
5. Without `--cert/--key`: → 401.
6. Reload the `ai` service page: the Access tab shows the note that the service is set to mTLS and preselects no mode. Choose "Open" and save to leave the mode.
7. Cleanup: `git status` the new `client.{crt,key}` and remove them — they should NOT be committed.
8. Findings ✅ / ❌

### Gotchas ⚠
- TCP tunnels reject all access modes except "Open" (409 Conflict in UI). Verify by attempting to set api_key on `tcp-echo`.
- `burrow_login` without `BURROW_AUTH_DOMAIN` configured → 409 from the API.

### Findings
- [ ]

---

## 7. AI Gateway basic (chat-completions, metering, rate limit, cost)

**Goal:** v0.4 AI gateway middleware chain works end-to-end via mockoai.

### Steps
1. **Turn the `ai` service into an AI endpoint.** An AI endpoint is simply an HTTP
   service in **api_key** access mode — there is NO separate "Register endpoint"
   button. Go to `/services` → `ai` → **Configure** → select **API key** →
   **Save changes**.
2. `/ai/endpoints` → the `ai` row now appears with status **Connected**.
   (Services left in *open* mode are intentionally NOT listed here — that empty
   list is expected until at least one service is in api_key mode.)
3. **Link check — every AI-gateway page must render (no blank page / error).**
   Click each AI-gateway nav link and confirm content renders:
   - `/ai/endpoints` (list) → click `ai` → `/ai/endpoints/<id>` (detail page:
     **Routing**, **Backends**, **Recent requests** sections all render)
   - `/cache` → both the **Exact match** AND **Semantic** tabs render (the
     Semantic tab must not blank out)
   - `/cost`, `/guardrails`, and `/inspector/<ai-service-id>` each render
4. `/tokens` → mint `ai-key-1` → copy.
5. Discover the AI slug:
   ```bash
   AI=$(docker logs burrow-e2e-full-relay-1 | grep "http tunnel registered" | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
   ```
6. From shell, hit chat-completions:
   ```bash
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS -N -X POST https://$AI.test.local:8443/v1/chat/completions \
        -H "Authorization: Bearer ai-key-1" \
        -H "Content-Type: application/json" \
        -d '{"model":"mock","stream":true,"messages":[{"role":"user","content":"hi"}]}'
   ```
7. Expect SSE stream with `data: {...}` chunks + `[DONE]` (5 chunks total: 4 content + 1 DONE).
8. `/ai/endpoints` → click `ai` → endpoint detail page renders. ⚠ The Requests /
   Tokens / Cost / Cache-hit tiles currently read **0** — see Gotchas (server-side
   usage aggregation is not wired yet). That is expected, not a failure.
9. Repeat the curl 20 times rapidly → trigger rate limit → expect 429 on later calls.
10. After rate limit fires, `/audit` should have a `ratelimit.enforced` entry.

### Expected ✅
- Every AI-gateway page in step 3 renders (no blank page).
- SSE streams without buffering (chunks arrive sequentially, not in one batch).
- Rate limit triggers 429.
- Audit chain captures `ratelimit.enforced`.
- ⚠ Per-endpoint metering tiles (requests/tokens/cost/cache) reading **0** is
  currently EXPECTED — proof of proxying is the SSE stream + the audit /
  connection-log entries, not the metric tiles.

### Gotchas ⚠
- **Per-endpoint metering is not aggregated yet.** `GET /ai/endpoints` and
  `GET /ai/endpoints/{id}/metrics` (internal/api/ai_endpoint_handlers.go) return
  hard-zeroed requests/tokens/cost/cache values — a documented TODO pending
  `usage_events` aggregation on the proxy hot-path. So the metric tiles on
  `/ai/endpoints`, the endpoint detail page, and the `$` figures on `/cost` will
  read 0 even after real traffic. Don't flag this as a regression; verify
  proxying via the SSE stream + `ratelimit.enforced` audit row instead.
- mockoai's `/v1/chat/completions` doesn't honor the bearer token (it accepts every request). The bearer is checked by Burrow's proxy access-mode gate BEFORE the request reaches mockoai. So 401 without bearer means Burrow's gate fired; 200 with bearer means Burrow accepted + proxied.
- Mock-oai cost depends on `model_aliases` + `cost_pricing` config. If pricing isn't set for `mock`/`claude-mock`, cost stays $0.0000. Either configure pricing OR accept $0 as the pass condition.
- The `mock` model name isn't mapped to a real provider — Burrow won't try to forward to OpenAI/Anthropic. mockoai is the upstream.

### Findings
- [ ]

---

## 8. AI Gateway depth (semantic cache, guardrail, redaction — v0.5 surfaces)

**Goal:** v0.5 semantic cache hits on similar prompts; guardrail refuses banned content; redaction masks PII.

### 8a. Semantic cache
1. `/cache` → "Semantic" tab → enable, similarity threshold 0.85, fallback "Return cached + Burrow-Cache: similar".
2. Send prompt A:
   ```bash
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS -X POST https://$AI.test.local:8443/v1/chat/completions \
        -H "Authorization: Bearer ai-key-1" \
        -d '{"model":"mock","stream":false,"messages":[{"role":"user","content":"What is the capital of France?"}]}'
   ```
3. Send a *similar* prompt B: `"Tell me the capital of France."` → expect `Burrow-Cache: similar` response header.
4. `/cache` → "Hits" panel shows 1 semantic hit.
5. Findings ✅

### 8b. Guardrail
1. `/guardrails` → add rule: pattern `forbidden`, action "refuse".
2. Send prompt: `"what about forbidden topics?"` → expect 400 with `{"error":"guardrail.refused"}`.
3. `/audit` shows `guardrail.refused` entry.
4. Findings ✅

### 8c. Redaction
1. `/guardrails` (Redaction tab) → add rule: pattern `email`, action "mask".
2. Send prompt: `"my email is alice@example.com"`.
3. Open `/inspector/<ai-service-id>` → latest request → body shows `my email is [REDACTED]`.
4. Findings ✅

### Gotchas ⚠
- Semantic cache only fires when v0.5 semantic backend is compiled in (`-tags=semantic_cache`). The default integration build does NOT include it — the Semantic tab will show "Not available" or zeros. To exercise this section, rebuild with:
  ```bash
  docker compose -f test/harness/compose.full.yml build \
    --build-arg GO_BUILD_TAGS=integration,semantic_cache relay
  docker compose -f test/harness/compose.full.yml up -d --wait
  ```
- mockoai's deterministic 4-dim SHA256-seeded embeddings (`/v1/embeddings`) only give meaningful similarity for prompts that share leading bytes. The "France" example may not cross the 0.85 threshold against a SHA256 seed; lower the threshold to 0.50 or use prompts that share the same first character if you need a guaranteed hit.
- mockoai always returns the same canned content (`Hello from mockoai.`), so prompt B's response body looks identical to A regardless of cache state. The cache signal is the `Burrow-Cache:` header, not the body.

### Findings
- [ ]

---

## 9. Custom domains (removed from the dashboard)

**Goal:** The dashboard offers no custom-domain UI. Custom domains need the opt-in host-routed ingress and are managed through the REST API only (`/api/v1/services/{serviceID}/domains`, see `docs/openapi.yaml`).

### Steps
1. `/services` → `ai`: the tabs are "Access", "API keys" and "Upstream key". There is no "Custom domains" tab.
2. Open `/services/<id>/domains` directly → lands on the service page.
3. Open `/settings/custom-domains` directly → lands on Settings.
4. The sidebar and the Settings tabs have no "Custom domains" entry.

### Expected ✅
- No "Custom domains" tab, nav entry or settings page.
- Both old links redirect instead of showing an empty or broken page.

### Findings
- [ ]

---

## 10. Connection logs (v0.5 surface)

**Goal:** Every TCP tunnel session writes a row to per-tunnel connection-logs; UI displays + NDJSON export works.

### Steps
1. From shell, hit the TCP echo tunnel 5x with fresh sessions:
   ```bash
   for i in 1 2 3 4 5; do
     curl --no-keepalive -fsS http://localhost:9002/healthz
   done
   ```
2. `/services` → `tcp-echo` → "Connection logs" tab.
3. Expect 5 rows (or fewer if sessions are keep-alive coalesced): each shows `start_ts`, `end_ts`, `duration_ms`, `bytes_in`, `bytes_out`, `source_ip`, `tunnel_id`.
4. Click "Export NDJSON" → file downloads.
5. Verify: `head -1 connection-logs.ndjson | jq .` parses as JSON with the expected fields.
6. Rollups: `/services/tcp-echo/connection-logs/rollups` (or via UI: rollups tab) shows daily aggregates. After running step 1 a few minutes apart you should see rows aggregating by day.
7. Top-source-IPs: if the "Top source IPs" feature is enabled (`connection_logs.rollup_include_top_ips=true`), the rollup row shows the source IPs sorted by traffic.
8. Retention: `/settings` → "Retention" → set `connection_logs.retention_days = 7` → save.

### Expected ✅
- Rows visible within 2s of driving traffic.
- NDJSON export downloads a valid file.
- Retention knob accepts integer value.

### Gotchas ⚠
- Connection logs only fire for TCP tunnels (per-session). HTTP tunnels emit per-request entries via the inspector instead.
- If sessions are keep-alive, you may see 1 row for many curls. Use `curl --no-keepalive` to force fresh sessions.
- The `connection_logs.rollup_include_top_ips` setting is opt-in (default OFF) for privacy reasons. The UI toggle was added in v0.5.1 P2.1.

### Findings
- [ ]

---

## 11. Audit + Webhooks + OpenAPI viewer + Retention

### 11a. Audit log
1. `/audit` → table populated (entries from prior sections: `user.create`, `token.mint`, `ratelimit.enforced`, etc.).
2. Search: filter by `token.mint` → only mint events visible.
3. Click row → expand → shows JSON payload + `prev_hash` + `hash`.
4. "Verify chain" → green notice "Chain valid from <first_id> to <last_id>".

### 11b. Webhooks (v0.5 expanded vocabulary)
1. `/webhooks` → "New webhook" → URL `http://mockoai:8081/healthz` (in-network catchall), events: `token.mint`, `ratelimit.enforced`, `connection.session_summary` → save.
2. Mint a token in another tab. Within 5s, webhook delivery succeeds (200 from mockoai's `/healthz`).
3. `/webhooks` → delivery log shows entry with status=200.
4. v0.5 payload templates: edit the webhook → enable "Template" → paste `{"event":"{{.Action}}","actor":"{{.ActorEmail}}"}` → save.
5. Mint another token → delivery body now matches the template.

### 11c. OpenAPI viewer (v0.5 surface)
1. `/settings` → "API" → "OpenAPI viewer" link → opens viewer page (path: `/api/v1/openapi/viewer`).
2. Navigates: list of endpoints (servers, routes, schemas) renders from the embedded `openapi.yaml`.
3. No external CDN scripts loaded (DevTools Network → filter `cdn.` → empty).

### 11d. Retention
1. `/settings` → "Retention" → set `audit.retention_days = 30`, `usage.retention_days = 7`, `inspector.retention_count = 100` → save.
2. Page reload preserves values.
3. (Enforcement is a backend compactor — manual smoke can't easily prove it without injecting old rows; defer to the Playwright spec or an integration test.)

### Gotchas ⚠
- The webhook target URL is `http://mockoai:8081/healthz` — that's the in-network DNS name, reachable from the relay container. Don't use `http://localhost:8081/...` (mockoai's :8081 is NOT published to the host).
- Audit "Verify chain" computes hashes over a range; on a fresh /test-reset the chain is empty and verification returns trivially.
- OpenAPI viewer is gated by admin OR `metrics:read` — viewer/openapi.yaml endpoints under the same gate.

### Findings
- [ ]

---

## 12. Postgres swap (v0.5 alpha)

**Goal:** Confirm the same UI works against the Postgres backend.

### Steps
1. Tear down SQLite stack: `docker compose -f test/harness/compose.full.yml down --volumes`.
2. Bring up with Postgres override:
   ```bash
   docker compose \
     -f test/harness/compose.full.yml \
     -f test/harness/compose.postgres.yml \
     up -d --build --wait
   ```
3. Verify: `docker compose -f .../compose.full.yml -f .../compose.postgres.yml ps` shows `postgres` healthy and `relay` healthy.
4. Re-run a subset of the above sections: §1 Bootstrap, §2 Tunnels, §4 Tokens, §11a Audit (Verify chain). Each should pass identically.
5. Verify the relay is actually using Postgres:
   ```bash
   docker exec -it burrow-e2e-full-postgres-1 psql -U burrow -d burrow \
     -c "SELECT count(*) FROM users;"
   ```
   Expect ≥ 1 (seeded admin).
6. Verify `/database` status surface (v0.5.0 Task 15) reports `driver: postgres`:
   ```bash
   curl -fsS http://localhost:8080/api/v1/database -H "Cookie: <session>" | jq .
   ```
   (Or check `/settings` → "Database" surface in the UI if shipped.)
7. Tear down + return to SQLite for subsequent sections:
   ```bash
   docker compose -f test/harness/compose.full.yml \
                  -f test/harness/compose.postgres.yml \
                  down --volumes
   docker compose -f test/harness/compose.full.yml up -d --wait
   ```

### Expected ✅
- Stack comes up healthy with Postgres.
- All re-tested sections pass identically.
- `psql` confirms data is in Postgres, not SQLite.
- `/api/v1/database` returns `{"driver":"postgres", "alpha":true, ...}`.

### Gotchas ⚠
- The relay image needs `-tags=integration,postgres` (handled by `GO_BUILD_TAGS` build arg in `compose.postgres.yml`).
- First boot under Postgres re-runs all migrations against a fresh DB — may take ~5s longer than SQLite warm boot.
- v0.5 Postgres is marked alpha (`Database.Alpha=true`). Some surfaces (e.g. semantic cache aggregator) may have SQLite-specific assumptions; capture any in Findings.
- Don't try to run `docker compose` with only one of the two `-f` files when bringing the Postgres stack down — pass both, otherwise compose can't find the postgres service.

### Findings
- [ ]

---

## 13. Reconnect (relay container restart)

**Goal:** All 3 clients reconnect after relay container restart; dashboard reflects recovery.

### Steps
1. With stack up, navigate `/tunnels`. Confirm all 4 tunnels connected.
2. From shell: `docker compose -f test/harness/compose.full.yml restart relay`.
3. Within 5-10s, dashboard may show "disconnected" badges (—) OR redirect to /login if session lost.
4. If logged out, re-login.
5. Within 30s, all 4 tunnels show `connected` again.
6. Drive traffic to confirm:
   ```bash
   curl -fsS http://localhost:9002/healthz
   AI=$(docker logs burrow-e2e-full-relay-1 | grep "http tunnel registered" | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
   curl --ssl-no-revoke -k --resolve "$AI.test.local:8443:127.0.0.1" \
        -fsS https://$AI.test.local:8443/healthz
   ```
   → both 200.

### Expected ✅
- All 4 tunnels reconnect within 30s.
- Driving traffic post-reconnect succeeds.

### Gotchas ⚠
- Client backoff is configurable; first reconnect attempt is usually within 1-2s. Subsequent retries exponentially.
- Don't `docker restart` on a single container — use `docker compose restart relay` so the network alias persists.
- The slug is stored with the service and survives a restart. If the service was removed (e.g. by /test-reset), re-discover it via `docker logs ... | grep "http tunnel registered" | tail -1`.

### Findings
- [ ]

---
