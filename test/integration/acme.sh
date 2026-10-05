#!/usr/bin/env bash
# test-only — never deploy this shape.
# test/integration/acme.sh
#
# End-to-end proof of burrowd's BUILT-IN ACME (CertMagic) + single-origin
# /svc/{slug} path routing against Pebble (Let's Encrypt's test ACME server).
#
# Brings up test/harness/compose.acme.yml, waits for the relay to obtain a
# Pebble-issued cert, then asserts (from INSIDE the relay container — it has
# curl + openssl, sits on the e2e network, and already mounts Pebble's CA, so
# there is no host-side TLS/path-mangling to fight):
#
#   1. burrowd logged "acme: certificates ready" and the HTTPS dashboard answers.
#   2. The cert served on :443 chains to Pebble's issuing root (/roots/0) for
#      relay.test — i.e. a REAL Pebble-issued cert (curl --cacert, verify ok).
#   3. The http tunnel is reachable at https://relay.test/svc/<slug>/healthz and
#      returns the upstream body (path routing over the ACME cert works e2e).
#   4. POST https://relay.test/svc/<slug>/echo round-trips method+header+body.
#   5. A redirect emitted by the upstream comes back rewritten under /svc/<slug>
#      (Location header rewrite).
#   6. The control channel on :7000 presents a cert chaining to Pebble's root.
#
# Tears the stack down (down -v) on success OR failure.
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'   # keep Git-Bash from mangling docker args on Windows

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"
COMPOSE="test/harness/compose.acme.yml"
RELAY="burrow-e2e-acme-relay-1"
RELAY_IP="10.30.50.10"
PEBBLE_CA="/certs/pebble.minica.pem"   # path INSIDE the relay container
DOMAIN="relay.test"

teardown() { echo "[acme] tearing down"; cd "$REPO_ROOT"; docker compose -f "$COMPOSE" down -v || true; }
trap teardown EXIT

# Bring up the infra (DNS + ACME + relay) FIRST, WITHOUT the client. The client
# has `depends_on: relay {condition: service_healthy}`; if we `up` everything at
# once and the relay needs a moment to issue its cert, compose's fail-fast aborts
# the whole `up` and the EXIT trap tears it all down before we can read why. By
# starting the relay on its own and polling its readiness here (capturing logs on
# failure), a transient relay hiccup is diagnosable instead of a silent abort.
echo "[acme] building + starting infra (challtestsrv + pebble + relay)"
docker compose -f "$COMPOSE" up -d --build challtestsrv pebble relay

# --- wait for ACME issuance + the HTTPS dashboard -------------------------
echo "[acme] waiting for burrowd to obtain its Pebble cert (log: 'acme: certificates ready')"
ready=""
for i in $(seq 1 120); do
  if docker logs "$RELAY" 2>&1 | grep -q "acme: certificates ready"; then
    echo "[acme] relay logged certificates ready (after ${i}s)"
    ready="y"; break
  fi
  st="$(docker inspect -f '{{.State.Status}}' "$RELAY" 2>/dev/null || echo gone)"
  if [ "$st" = "exited" ] || [ "$st" = "gone" ]; then
    echo "[acme] relay container is not running (state=$st) — dumping relay + pebble logs:"
    docker logs "$RELAY" 2>&1 | tail -60
    echo "--- pebble ---"; docker compose -f "$COMPOSE" logs pebble 2>&1 | tail -25
    exit 1
  fi
  sleep 1
done
if [ -z "$ready" ]; then
  echo "[acme] relay never reported certificates ready after 120s — dumping logs:"
  docker logs "$RELAY" 2>&1 | tail -80; exit 1
fi

# Belt-and-braces: the relay healthcheck also polls https://relay.test/healthz.
echo "[acme] waiting for relay container health=healthy"
for i in $(seq 1 60); do
  status="$(docker inspect -f '{{.State.Health.Status}}' "$RELAY" 2>/dev/null || echo unknown)"
  [ "$status" = "healthy" ] && { echo "[acme] relay healthy (after ${i}s)"; break; }
  sleep 1
done

# Now the relay is up with a cert; start the client (its service_healthy
# dependency is already satisfied, so this is immediate and race-free).
echo "[acme] starting client (upstream-http)"
docker compose -f "$COMPOSE" up -d upstream-http

# --- discover the http tunnel slug (the /svc/<slug> segment) ---------------
echo "[acme] discovering http tunnel slug from relay logs"
ID=""
for i in $(seq 1 60); do
  ID="$(docker logs "$RELAY" 2>&1 | grep 'http tunnel registered' | tail -1 | grep -oE 'slug=[a-z0-9-]+' | cut -d= -f2 || true)"
  [ -n "$ID" ] && break
  sleep 1
done
if [ -z "$ID" ]; then
  echo "[acme] could not discover http tunnel slug — dumping relay + client logs:"
  docker logs "$RELAY" 2>&1 | tail -40
  echo "--- client (upstream-http) ---"; docker compose -f "$COMPOSE" logs upstream-http 2>&1 | tail -30
  exit 1
fi
echo "[acme] http tunnel slug = $ID"

# --- run all TLS/path assertions INSIDE the relay container ----------------
# The relay image has curl + openssl, is on the acme network, resolves 'pebble'
# and (via alias) 'relay.test', and mounts both Pebble's CA at $PEBBLE_CA and
# the assertion script at /usr/local/bin/acme-assert.sh (see compose.acme.yml).
# We run the MOUNTED file (not a here-doc over exec stdin: a non-interactive
# `docker exec` attaches no stdin, so a piped `bash -s` would silently run an
# EMPTY script and report a FALSE green).
echo "[acme] running in-container assertions"
out="$(docker exec \
  -e PEBBLE_CA="$PEBBLE_CA" -e DOMAIN="$DOMAIN" -e RELAY_IP="$RELAY_IP" -e ID="$ID" \
  "$RELAY" bash /usr/local/bin/acme-assert.sh 2>&1)"
rc=$?
echo "$out"

# Belt-and-braces against a false green: require BOTH a zero exit AND the
# sentinel line with fail=0. If the script crashed before emitting the
# sentinel, this catches it even if rc were somehow 0.
if [ "$rc" -ne 0 ] || ! echo "$out" | grep -q 'ACME_ASSERT_RESULT pass=[1-9][0-9]* fail=0'; then
  echo "[acme] assertions FAILED (rc=$rc, sentinel check failed) — relay logs tail:"
  docker logs "$RELAY" 2>&1 | tail -40
  exit 1
fi
echo "[acme] ALL GREEN — built-in ACME + single-origin path routing verified against Pebble"
