#!/usr/bin/env bash
# test-only — never deploy this shape.
# test/harness/entrypoints/relay-acme.sh
#
# Boots burrowd with BUILT-IN ACME (no --dev-certs: CertMagic obtains the certs
# from Pebble and serves them on every listener). burrowd's acme.New blocks on
# ManageSync, so listeners only start once certs are ready; we then poll the
# HTTPS dashboard, mint a client token, and write it to the shared volume.
set -euo pipefail
: "${BURROW_ADMIN_EMAIL:=admin@e2e.local}"
: "${BURROW_ADMIN_PASSWORD:=e2e-pass}"
export BURROW_ADMIN_EMAIL BURROW_ADMIN_PASSWORD

TOKEN_PATH="/run/burrow/token"
mkdir -p "$(dirname "$TOKEN_PATH")"

echo "[relay-acme] starting burrowd serve (ACME domain=${BURROW_ACME_DOMAIN}, ca=${BURROW_ACME_CA})"
echo "[relay-acme] SSL_CERT_FILE=${SSL_CERT_FILE:-<unset>} (CertMagic trusts Pebble via this CA)"
burrowd serve &
SERVE_PID=$!

# Poll the HTTPS dashboard on :443 (ACME promotes the :8080 default to :443).
# CertMagic only serves a cert for the managed SNI (relay.test), so we --resolve
# relay.test to loopback to present the right SNI (a plain https://127.0.0.1
# handshake gets "no certificate available for 127.0.0.1"). -k: the served cert
# is real but signed by Pebble's test root, which this in-container curl doesn't
# trust — readiness only needs "is it answering"; the real TLS-chain proof is
# done by the tester against Pebble's /roots/0.
echo "[relay-acme] polling https://relay.test/healthz via loopback (waiting on ACME issuance)"
for i in $(seq 1 120); do
  if curl -fsS -k -o /dev/null --resolve "relay.test:443:127.0.0.1" "https://relay.test/healthz"; then
    echo "[relay-acme] dashboard up after ${i}s — ACME certs are ready"
    break
  fi
  if ! kill -0 "$SERVE_PID" 2>/dev/null; then
    echo "[relay-acme] burrowd exited before /healthz came up (ACME issuance likely failed)" >&2
    exit 1
  fi
  sleep 1
done
if ! curl -fsS -k -o /dev/null --resolve "relay.test:443:127.0.0.1" "https://relay.test/healthz"; then
  echo "[relay-acme] giving up — dashboard not up after 120s" >&2
  exit 1
fi

# Mint a client token for the e2e admin user.
echo "[relay-acme] minting token via 'burrowd token'"
TOKEN="$(burrowd token --email "$BURROW_ADMIN_EMAIL" --name e2e-acme)"
if [ -z "$TOKEN" ]; then
  echo "[relay-acme] burrowd token produced empty output" >&2
  exit 1
fi
echo "$TOKEN" > "$TOKEN_PATH"
echo "[relay-acme] token written to $TOKEN_PATH ($(wc -c < "$TOKEN_PATH") bytes)"

# Hand control back to burrowd.
wait "$SERVE_PID"
