#!/usr/bin/env bash
# test-only — never deploy this shape.
# test/harness/entrypoints/client-http.sh
# Waits for the relay-minted token, starts the tiny upstream HTTP service, then
# runs `burrow connect --type http` so the relay exposes it as an http tunnel
# reachable at https://relay.test/svc/<slug>/ (single-origin path routing).
set -euo pipefail

TOKEN_PATH="/run/burrow/token"

echo "[client-http] waiting for $TOKEN_PATH (relay bootstraps this)"
for i in $(seq 1 120); do
  if [ -s "$TOKEN_PATH" ]; then
    echo "[client-http] token present (after ${i}s)"
    break
  fi
  sleep 1
done
if [ ! -s "$TOKEN_PATH" ]; then
  echo "[client-http] giving up — $TOKEN_PATH not present after 120s" >&2
  exit 1
fi
TOKEN="$(cat "$TOKEN_PATH")"

echo "[client-http] starting upstream on $UPSTREAM_ADDR (background)"
/usr/local/bin/upstream --addr "$UPSTREAM_ADDR" &
UPSTREAM_PID=$!

for i in $(seq 1 20); do
  if curl -fsS -o /dev/null "http://$UPSTREAM_ADDR/healthz"; then
    echo "[client-http] upstream /healthz is up"
    break
  fi
  if ! kill -0 "$UPSTREAM_PID" 2>/dev/null; then
    echo "[client-http] upstream exited before /healthz came up" >&2
    exit 1
  fi
  sleep 0.5
done

echo "[client-http] running burrow connect --type http -> $BURROW_RELAY (insecure: relay serves an ACME/Pebble cert the client doesn't trust)"
exec /usr/local/bin/burrow connect \
  --server "$BURROW_RELAY" \
  --token  "$TOKEN" \
  --local  "$UPSTREAM_ADDR" \
  --remote "$BURROW_REMOTE_PORT" \
  --name   "$BURROW_TUNNEL_NAME" \
  --type   http \
  --insecure
