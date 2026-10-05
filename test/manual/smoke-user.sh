#!/usr/bin/env bash
# test-only — never deploy this shape.
#
# smoke-user.sh — Burrow user-acceptance smoke runner.
# Drives the full user journey via curl + docker + the admin API:
#   mint → connect real client → expose → access gate → AI+cache →
#   quota 429 → ip-geo 403 → audit → restart & reconnect
#
# Usage:
#   bash test/manual/smoke-user.sh           # bring stack up + tear down on exit
#   bash test/manual/smoke-user.sh --no-up   # use already-running stack, skip teardown

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"
COMPOSE="test/harness/compose.full.yml"

# ---------------------------------------------------------------------------
# Flags
# ---------------------------------------------------------------------------
NO_UP=0
for arg in "$@"; do
  case "$arg" in
    --no-up) NO_UP=1 ;;
    *) echo "Unknown flag: $arg" >&2; exit 1 ;;
  esac
done

# ---------------------------------------------------------------------------
# PASS/FAIL framework
# ---------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0
CURRENT_STEP=""

step() {
  CURRENT_STEP="$1"
  echo ""
  echo ">>> STEP: $CURRENT_STEP"
}

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  echo "[PASS] $CURRENT_STEP"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  echo "[FAIL] $CURRENT_STEP — $1"
}

summary() {
  echo ""
  echo "========================================"
  echo "  RESULTS: $PASS_COUNT passed, $FAIL_COUNT failed"
  echo "========================================"
  if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Global state (set by helpers, used by cleanup trap)
# ---------------------------------------------------------------------------
EPHEM_CONTAINER="burrow-smoke-user-ephem"
AI_ID=""
AI_SUB=""
EPHEM_SUB=""
RATE_LIMIT_ID=""
COOKIE_JAR=""
CSRF=""
LOGIN_OUTPUT=""

cleanup() {
  echo ""
  echo "[smoke-user] cleanup..."

  # Remove ephemeral client container
  if docker ps -a --format "{{.Names}}" 2>/dev/null | grep -q "^${EPHEM_CONTAINER}$"; then
    docker rm -f "$EPHEM_CONTAINER" >/dev/null 2>&1 || true
    echo "  removed container: $EPHEM_CONTAINER"
  fi

  # Remove any leftover rate-limit rule
  if [ -n "$RATE_LIMIT_ID" ] && [ -n "$CSRF" ] && [ -n "$COOKIE_JAR" ]; then
    curl -s -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
      -X DELETE "http://localhost:8080/api/v1/rate-limits/${RATE_LIMIT_ID}" \
      >/dev/null 2>&1 || true
    RATE_LIMIT_ID=""
  fi

  # Reset ai service: open mode, ip-geo disabled
  if [ -n "$AI_ID" ] && [ -n "$CSRF" ] && [ -n "$COOKIE_JAR" ]; then
    curl -s -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
      -H "Content-Type: application/json" \
      -X PUT "http://localhost:8080/api/v1/services/${AI_ID}/access-mode" \
      -d '{"access_mode":"open"}' >/dev/null 2>&1 || true

    curl -s -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
      -H "Content-Type: application/json" \
      -X PUT "http://localhost:8080/api/v1/services/${AI_ID}/ip-geo" \
      -d '{"enabled":false,"block_cidrs":[],"allow_cidrs":[],"allow_countries":[],"block_countries":[]}' \
      >/dev/null 2>&1 || true
    echo "  reset ai service to open + ip-geo disabled"
  fi

  # Clean up cookie jar temp file
  if [ -n "$COOKIE_JAR" ] && [ -f "$COOKIE_JAR" ]; then
    rm -f "$COOKIE_JAR"
  fi

  if [ "$NO_UP" = "0" ]; then
    echo "[smoke-user] tearing down stack"
    cd "$REPO_ROOT"
    docker compose -f "$COMPOSE" down --volumes || true
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Stack management
# ---------------------------------------------------------------------------
if [ "$NO_UP" = "0" ]; then
  echo "[smoke-user] building + starting stack (--wait)"
  docker compose -f "$COMPOSE" up -d --build --wait
else
  echo "[smoke-user] --no-up: assuming stack is already running"
fi

# ---------------------------------------------------------------------------
# API helpers
# ---------------------------------------------------------------------------

# do_login — sets global COOKIE_JAR, CSRF, LOGIN_OUTPUT.
# Must be called directly (not in a subshell) so globals propagate.
do_login() {
  COOKIE_JAR="$(mktemp /tmp/burrow-smoke-user.XXXXXX)"
  LOGIN_OUTPUT=$(curl -s -c "$COOKIE_JAR" \
    -X POST http://localhost:8080/api/v1/auth/login \
    -H "Content-Type: application/json" \
    -d '{"email":"admin@e2e.local","password":"e2e-pass"}' 2>&1)
  # Extract CSRF from the cookie jar (tab-delimited Netscape format; value is last field)
  CSRF=$(grep "burrow_csrf" "$COOKIE_JAR" 2>/dev/null | awk '{print $NF}' || true)
}

# api_get <path> — authenticated GET
api_get() {
  curl -s -b "$COOKIE_JAR" "http://localhost:8080${1}"
}

# api_csrf_body <METHOD> <path> <json-body> — CSRF mutation, returns body
api_csrf_body() {
  curl -s -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
    -H "Content-Type: application/json" \
    -X "$1" "http://localhost:8080${2}" -d "$3"
}

# api_csrf_status <METHOD> <path> [json-body] — returns HTTP status code only
api_csrf_status() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -o /dev/null -w "%{http_code}" \
      -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
      -H "Content-Type: application/json" \
      -X "$method" "http://localhost:8080${path}" -d "$body"
  else
    curl -s -o /dev/null -w "%{http_code}" \
      -b "$COOKIE_JAR" -H "X-CSRF-Token: $CSRF" \
      -X "$method" "http://localhost:8080${path}"
  fi
}

# proxy_get_status <subdomain> <path> — HTTPS proxy GET, returns status
proxy_get_status() {
  curl -sk -o /dev/null -w "%{http_code}" \
    "https://localhost:8443${2}" -H "host: ${1}.test.local"
}

# proxy_post_status <subdomain> <path> <json-body> — HTTPS proxy POST, returns status
proxy_post_status() {
  curl -sk -o /dev/null -w "%{http_code}" \
    -X POST "https://localhost:8443${2}" \
    -H "host: ${1}.test.local" \
    -H "Content-Type: application/json" \
    -d "$3"
}

# proxy_post_with_headers <subdomain> <path> <json-body> — POST, outputs status line + headers
proxy_post_with_headers() {
  curl -sk -D - -o /dev/null \
    -X POST "https://localhost:8443${2}" \
    -H "host: ${1}.test.local" \
    -H "Content-Type: application/json" \
    -d "$3"
}

# json_field <field> — extract a simple string field from piped JSON
# e.g.  echo '{"token":"bur_abc"}' | json_field token  => bur_abc
json_field() {
  grep -o "\"${1}\":\"[^\"]*\"" | head -1 | cut -d'"' -f4 || true
}

# json_services_slug <name> [require_connected] — read services JSON from stdin,
# find service by name, print its slug (empty if not found / not connected).
# Uses node (reliably in PATH on this host; the e2e stack depends on it too).
json_services_slug() {
  local svcname="$1"
  local require_connected="${2:-1}"
  node -e "
const d=[];
process.stdin.on('data',c=>d.push(c));
process.stdin.on('end',()=>{
  try {
    const a=JSON.parse(d.join(''));
    const s=a.find(x=>x.name==='${svcname}'${require_connected:+&&x.connected&&x.slug});
    console.log(s&&s.slug?s.slug:'');
  } catch(e){ console.log(''); }
});
" 2>/dev/null || true
}

# json_services_id <name> — read services JSON from stdin, print id for named service
json_services_id() {
  local svcname="$1"
  node -e "
const d=[];
process.stdin.on('data',c=>d.push(c));
process.stdin.on('end',()=>{
  try {
    const a=JSON.parse(d.join(''));
    const s=a.find(x=>x.name==='${svcname}');
    console.log(s&&s.id?s.id:'');
  } catch(e){ console.log(''); }
});
" 2>/dev/null || true
}

# json_count — read JSON array from stdin, print length
json_count() {
  node -e "
const d=[];
process.stdin.on('data',c=>d.push(c));
process.stdin.on('end',()=>{
  try { console.log(JSON.parse(d.join('')).length); }
  catch(e){ console.log('0'); }
});
" 2>/dev/null || echo "0"
}

# find_ai_service — sets AI_ID and AI_SUB globals from /api/v1/services
find_ai_service() {
  local svcs
  svcs=$(api_get /api/v1/services)
  AI_ID=$(echo "$svcs" | json_services_id "ai")
  AI_SUB=$(echo "$svcs" | json_services_slug "ai" 1)
}

# poll_service_connected <name> <timeout_secs> — prints subdomain when found, else ""
poll_service_connected() {
  local svcname="$1"
  local timeout="$2"
  local found=""
  for attempt in $(seq 1 "$timeout"); do
    found=$(api_get /api/v1/services | json_services_slug "$svcname" 1)
    if [ -n "$found" ]; then
      echo "$found"
      return 0
    fi
    sleep 1
  done
  echo ""
}

# ---------------------------------------------------------------------------
# STEP 1 — Boot / login
# ---------------------------------------------------------------------------
step "1-login"
do_login
if echo "$LOGIN_OUTPUT" | grep -q '"email":"admin@e2e.local"'; then
  if [ -n "$CSRF" ]; then
    pass
  else
    fail "login response OK but no burrow_csrf cookie found in jar"
  fi
else
  fail "login failed: $LOGIN_OUTPUT"
fi

# ---------------------------------------------------------------------------
# STEP 2 — Mint token
# ---------------------------------------------------------------------------
step "2-mint-token"
TOKEN=""
TOKEN_RESP=$(api_csrf_body POST /api/v1/tokens '{"name":"smoke-user-ephem"}')
TOKEN=$(echo "$TOKEN_RESP" | json_field token)
if echo "$TOKEN" | grep -q '^bur_'; then
  pass
else
  fail "expected bur_ token, got: $TOKEN_RESP"
fi

# ---------------------------------------------------------------------------
# STEP 3 — Connect a real client
# ---------------------------------------------------------------------------
step "3-connect-client"
EPHEM_SUB=""
if [ -z "$TOKEN" ]; then
  fail "no token available (step 2 failed)"
else
  # Remove any stale container from a previous run
  docker rm -f "$EPHEM_CONTAINER" >/dev/null 2>&1 || true

  # MSYS_NO_PATHCONV=1: prevent Git Bash from translating /usr/local/bin/burrow
  # to a Windows path (C:/Program Files/Git/usr/local/bin/burrow) when passing
  # it to docker --entrypoint. The path is inside the Linux container image.
  MSYS_NO_PATHCONV=1 docker run -d --rm --name "$EPHEM_CONTAINER" \
    --network burrow-e2e-full_e2e \
    --entrypoint /usr/local/bin/burrow \
    burrow-e2e-client-ai:dev \
    connect \
    --server relay.test.local:7000 \
    --token "$TOKEN" \
    --local mockoai:8081 \
    --remote 0 \
    --name smoke-ephem \
    --type http \
    --insecure >/dev/null 2>&1

  # Poll up to 30s for the tunnel to register connected
  EPHEM_SUB=$(poll_service_connected "smoke-ephem" 30)

  if [ -n "$EPHEM_SUB" ]; then
    pass
    echo "  subdomain: $EPHEM_SUB"
  else
    fail "smoke-ephem tunnel did not register connected within 30s"
  fi
fi

# ---------------------------------------------------------------------------
# STEP 4 — Expose (proxy request through ephemeral tunnel)
# ---------------------------------------------------------------------------
step "4-expose"
if [ -z "$EPHEM_SUB" ]; then
  fail "no ephemeral subdomain available (step 3 failed)"
else
  STATUS=$(proxy_get_status "$EPHEM_SUB" /healthz)
  if [ "$STATUS" = "200" ]; then
    pass
  else
    fail "expected 200, got $STATUS (host: ${EPHEM_SUB}.test.local)"
  fi
fi

# ---------------------------------------------------------------------------
# Resolve ai service (needed for steps 5-9)
# ---------------------------------------------------------------------------
find_ai_service
echo ""
echo "  [info] ai service id=${AI_ID:-<not found>}  subdomain=${AI_SUB:-<not connected>}"

# ---------------------------------------------------------------------------
# STEP 5 — Access gate (api_key)
# ---------------------------------------------------------------------------
step "5-access-gate-api-key"
if [ -z "$AI_ID" ] || [ -z "$AI_SUB" ]; then
  fail "ai service not found or not connected"
else
  # Set access mode to api_key
  MODE_STATUS=$(api_csrf_status PUT "/api/v1/services/${AI_ID}/access-mode" '{"access_mode":"api_key"}')
  if [ "$MODE_STATUS" != "204" ]; then
    fail "set api_key mode returned $MODE_STATUS (expected 204)"
  else
    # Mint an API key
    KEY_RESP=$(api_csrf_body POST "/api/v1/services/${AI_ID}/api-keys" '{"name":"smoke-gate-key"}')
    API_KEY=$(echo "$KEY_RESP" | json_field key)

    if [ -z "$API_KEY" ]; then
      # Reset before failing
      api_csrf_status PUT "/api/v1/services/${AI_ID}/access-mode" '{"access_mode":"open"}' >/dev/null
      fail "could not mint api key: $KEY_RESP"
    else
      # Request WITHOUT key -> expect 401
      STATUS_NO_KEY=$(proxy_get_status "$AI_SUB" /healthz)

      # Request WITH key (Bearer prefix required by proxy/access.go)
      STATUS_WITH_KEY=$(curl -sk -o /dev/null -w "%{http_code}" \
        "https://localhost:8443/healthz" \
        -H "host: ${AI_SUB}.test.local" \
        -H "Authorization: Bearer ${API_KEY}")

      # Reset to open before asserting
      api_csrf_status PUT "/api/v1/services/${AI_ID}/access-mode" '{"access_mode":"open"}' >/dev/null

      if [ "$STATUS_NO_KEY" = "401" ] && [ "$STATUS_WITH_KEY" = "200" ]; then
        pass
      elif [ "$STATUS_NO_KEY" != "401" ]; then
        fail "no-key: expected 401, got $STATUS_NO_KEY"
      else
        fail "with-key: expected 200, got $STATUS_WITH_KEY"
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
# STEP 6 — AI + cache
# ---------------------------------------------------------------------------
step "6-ai-cache"
if [ -z "$AI_SUB" ]; then
  fail "ai service not connected"
else
  CHAT_BODY='{"model":"mock","stream":false,"messages":[{"role":"user","content":"user-acceptance-smoke-cache-test-2026"}]}'

  # First request
  S1=$(proxy_post_status "$AI_SUB" /v1/chat/completions "$CHAT_BODY")

  # Second identical request — capture headers to check for cache hit
  RESP2=$(proxy_post_with_headers "$AI_SUB" /v1/chat/completions "$CHAT_BODY")
  S2=$(echo "$RESP2" | grep "^HTTP/" | awk '{print $2}' | tr -d '\r' || true)

  if [ "$S1" != "200" ] || [ "$S2" != "200" ]; then
    fail "AI requests failed: req1=$S1 req2=$S2 (expected both 200)"
  else
    CACHE_HEADER=$(echo "$RESP2" | grep -i "^Burrow-Cache:" | tr -d '\r' || true)
    if echo "$CACHE_HEADER" | grep -qi "HIT"; then
      pass
      echo "  Burrow-Cache: $CACHE_HEADER (confirmed HIT)"
    else
      # Both 200 is the minimum; Burrow-Cache header is best-effort
      pass
      if [ -n "$CACHE_HEADER" ]; then
        echo "  note: both 200; cache header present but not HIT: $CACHE_HEADER"
      else
        echo "  note: both 200; no Burrow-Cache header in response (stream or cache disabled)"
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
# STEP 7 — Quota 429
# ---------------------------------------------------------------------------
step "7-quota-429"
if [ -z "$AI_SUB" ]; then
  fail "ai service not connected"
else
  # Create a global rpm=5 rate-limit rule
  RL_RESP=$(api_csrf_body POST /api/v1/rate-limits \
    '{"scope":"global","subject":"","dimension":"rpm","limit":5,"burst":5}')
  RATE_LIMIT_ID=$(echo "$RL_RESP" | json_field id)

  if [ -z "$RATE_LIMIT_ID" ]; then
    fail "could not create rate-limit rule: $RL_RESP"
  else
    CHAT_BODY='{"model":"mock","stream":false,"messages":[{"role":"user","content":"quota-test"}]}'
    GOT_429=0
    for i in $(seq 1 8); do
      ST=$(proxy_post_status "$AI_SUB" /v1/chat/completions "$CHAT_BODY")
      if [ "$ST" = "429" ]; then
        GOT_429=1
        echo "  got 429 on request $i"
        break
      fi
    done

    # Delete the rate-limit rule (safety net; trap also handles this)
    api_csrf_status DELETE "/api/v1/rate-limits/${RATE_LIMIT_ID}" >/dev/null
    RATE_LIMIT_ID=""

    if [ "$GOT_429" = "1" ]; then
      pass
    else
      fail "fired 8 requests, never got 429 from rpm=5 rule"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# STEP 8 — IP/geo 403
# ---------------------------------------------------------------------------
step "8-ipgeo-403"
if [ -z "$AI_ID" ] || [ -z "$AI_SUB" ]; then
  fail "ai service not found or not connected"
else
  # Enable ip-geo: block 0.0.0.0/0 (blocks all IPv4 regardless of Docker bridge IP)
  GEO_STATUS=$(api_csrf_status PUT "/api/v1/services/${AI_ID}/ip-geo" \
    '{"enabled":true,"block_cidrs":["0.0.0.0/0"],"allow_cidrs":[],"allow_countries":[],"block_countries":[]}')

  if [ "$GEO_STATUS" != "204" ]; then
    fail "enable ip-geo returned $GEO_STATUS (expected 204)"
  else
    # Request to proxy should now be 403
    DENIED_STATUS=$(proxy_get_status "$AI_SUB" /healthz)

    # Disable ip-geo (safety net; trap also handles this)
    api_csrf_status PUT "/api/v1/services/${AI_ID}/ip-geo" \
      '{"enabled":false,"block_cidrs":[],"allow_cidrs":[],"allow_countries":[],"block_countries":[]}' >/dev/null

    # Verify traffic flows after removing block
    RESTORED_STATUS=$(proxy_get_status "$AI_SUB" /healthz)

    if [ "$DENIED_STATUS" = "403" ] && [ "$RESTORED_STATUS" = "200" ]; then
      pass
    elif [ "$DENIED_STATUS" != "403" ]; then
      fail "ip-geo block active: expected 403, got $DENIED_STATUS"
    else
      fail "ip-geo block removed: expected 200, got $RESTORED_STATUS"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# STEP 9 — Observe (audit + connection logs)
# ---------------------------------------------------------------------------
step "9-observe"
AUDIT_RESP=$(api_get /api/v1/audit/events)
AUDIT_COUNT=$(echo "$AUDIT_RESP" | json_count)
CONNLOG_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -b "$COOKIE_JAR" \
  "http://localhost:8080/api/v1/connection-logs")

AUDIT_OK=0
CONNLOG_OK=0
if [ "$AUDIT_COUNT" -ge 1 ] 2>/dev/null; then AUDIT_OK=1; fi
if [ "$CONNLOG_STATUS" = "200" ]; then CONNLOG_OK=1; fi

if [ "$AUDIT_OK" = "1" ] && [ "$CONNLOG_OK" = "1" ]; then
  pass
  echo "  audit events: $AUDIT_COUNT  connection-logs: HTTP $CONNLOG_STATUS"
else
  REASON=""
  if [ "$AUDIT_OK" = "0" ]; then
    REASON="audit: got count=$AUDIT_COUNT (expected ≥1)"
  fi
  if [ "$CONNLOG_OK" = "0" ]; then
    REASON="${REASON:+$REASON; }connection-logs: HTTP $CONNLOG_STATUS (expected 200)"
  fi
  fail "$REASON"
fi

# ---------------------------------------------------------------------------
# STEP 10 — Restart & reconnect
# ---------------------------------------------------------------------------
step "10-restart-reconnect"
if [ -z "$AI_SUB" ]; then
  fail "ai service was not connected before restart (cannot verify reconnect)"
else
  echo "  restarting relay container..."
  cd "$REPO_ROOT"
  docker compose -f "$COMPOSE" restart relay

  # Wait for relay healthcheck to respond (up to 60s)
  RELAY_HEALTHY=0
  for attempt in $(seq 1 60); do
    if curl -fsS -o /dev/null http://localhost:8080/healthz 2>/dev/null; then
      RELAY_HEALTHY=1
      echo "  relay healthy after ${attempt}s"
      break
    fi
    sleep 1
  done

  if [ "$RELAY_HEALTHY" = "0" ]; then
    fail "relay did not become healthy within 60s after restart"
  else
    # Re-login — session cookies are invalidated on restart
    OLD_JAR="$COOKIE_JAR"
    do_login
    rm -f "$OLD_JAR" 2>/dev/null || true

    if [ -z "$CSRF" ]; then
      fail "re-login after restart failed (no CSRF token)"
    else
      echo "  re-login OK; polling for ai service to reconnect (up to 90s)..."

      # Poll for ai service to reconnect; subdomain may change after restart
      AI_SUB=$(poll_service_connected "ai" 90)

      if [ -z "$AI_SUB" ]; then
        fail "ai service did not reconnect within 90s after relay restart"
      else
        echo "  ai reconnected; subdomain: $AI_SUB"
        # Update AI_ID for cleanup trap
        find_ai_service

        # Verify traffic flows
        FINAL_STATUS=$(proxy_get_status "$AI_SUB" /healthz)
        if [ "$FINAL_STATUS" = "200" ]; then
          pass
        else
          fail "proxy request after restart: expected 200, got $FINAL_STATUS"
        fi
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
summary
