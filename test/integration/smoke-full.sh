#!/usr/bin/env bash
# test-only — never deploy this shape.
# Tier-1 data-plane gate against the FULL stack: dashboard + 3 TCP tunnels +
# AI SSE via the host-routed proxy. No browser, no /test-reset (that wipes
# client_tokens and breaks the seeded on-disk tokens). Tears down on EXIT.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"
COMPOSE="test/harness/compose.full.yml"

teardown() { echo "[smoke-full] tearing down"; cd "$REPO_ROOT"; docker compose -f "$COMPOSE" down --volumes || true; }
trap teardown EXIT

echo "[smoke-full] building + starting full stack (--wait)"
docker compose -f "$COMPOSE" up -d --build --wait

echo "[smoke-full] dashboard /healthz"
curl -fsS http://localhost:8080/healthz >/dev/null

echo "[smoke-full] wait for 3 TCP tunnels (9002 tcp-echo, 9003 svc-a, 9004 svc-b)"
for port in 9002 9003 9004; do
  for attempt in $(seq 1 60); do
    if curl -fsS -m 2 -o /dev/null "http://localhost:${port}/healthz"; then echo "  :${port} ready (${attempt}s)"; break; fi
    if [ "$attempt" = "60" ]; then echo "  :${port} did NOT register within 60s"; exit 1; fi
    sleep 1
  done
done

echo "[smoke-full] AI SSE via host-routed proxy on :8443"
SUB=$(docker logs burrow-e2e-full-relay-1 2>&1 | grep "http tunnel registered" | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2)
if [ -z "$SUB" ]; then echo "  could not discover AI slug from relay logs"; exit 1; fi
curl -fsS --ssl-no-revoke -k --resolve "$SUB.test.local:8443:127.0.0.1" \
  -X POST -H "content-type: application/json" \
  -d '{"model":"mock","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  "https://$SUB.test.local:8443/v1/chat/completions" | grep -q "data: "

echo "[smoke-full] all data-plane surfaces green"
