#!/usr/bin/env bash
# test-only — never deploy this shape.
# test/harness/acme-assert.sh
#
# The ACME + path-routing assertions, run from INSIDE the relay container by
# test/integration/acme.sh (the relay image has curl + openssl, sits on the
# acme network, resolves 'pebble' and 'relay.test', and mounts Pebble's CA).
# Kept as a file (mounted via compose.acme.yml) rather than piped over
# `docker exec` stdin: a non-interactive `docker exec` does NOT attach stdin,
# so a here-doc'd `bash -s` would silently run an EMPTY script and report a
# false green. A mounted file + `docker exec bash <file>` is deterministic.
#
# Inputs (env): PEBBLE_CA, DOMAIN, RELAY_IP, ID (the http tunnel slug).
# Emits one PASS/FAIL line per check and a final sentinel:
#   ACME_ASSERT_RESULT pass=<n> fail=<m>
# Exits non-zero if any assertion failed (and the caller ALSO greps the
# sentinel, so a crash before the sentinel can never masquerade as success).
set -uo pipefail

: "${PEBBLE_CA:?}"; : "${DOMAIN:?}"; : "${RELAY_IP:?}"; : "${ID:?}"

pass=0; fail=0; fails=()
ok() { echo "PASS  $1"; pass=$((pass + 1)); }
no() { echo "FAIL  $1"; fail=$((fail + 1)); fails+=("$1"); }

CA_OPT=(--cacert "$PEBBLE_CA")
RES=(--resolve "$DOMAIN:443:$RELAY_IP")

# Pebble's ISSUING root (generated fresh each Pebble run). Pebble's own :15000
# TLS is signed by the bundled minica we trust via $PEBBLE_CA; the body is the
# root that signed the relay's leaf certificate.
ROOT=/tmp/pebble-root.pem
if curl -fsS "${CA_OPT[@]}" "https://pebble:15000/roots/0" -o "$ROOT" && [ -s "$ROOT" ]; then
  ok "fetched Pebble issuing root from https://pebble:15000/roots/0"
else
  no "fetch Pebble issuing root (/roots/0)"
  echo "ACME_ASSERT_RESULT pass=$pass fail=$fail"
  exit 1
fi

# 1) :443 dashboard serves a cert that chains to Pebble's issuing root — i.e. a
#    REAL Pebble-issued cert, validated (not -k). ssl_verify_result=0 == chain ok.
read -r code vr < <(curl -s -o /dev/null -w '%{http_code} %{ssl_verify_result}' \
  --cacert "$ROOT" "${RES[@]}" "https://$DOMAIN/healthz")
if [ "$code" = "200" ] && [ "$vr" = "0" ]; then
  ok "GET https://$DOMAIN/healthz = 200, TLS chain valid vs Pebble root (http=$code verify=$vr)"
else
  no "dashboard cert/health (http=$code ssl_verify=$vr; want 200/0)"
fi

# 2) http tunnel reachable at /svc/<slug>/healthz over the ACME cert (path routing).
body="$(curl -s --cacert "$ROOT" "${RES[@]}" "https://$DOMAIN/svc/$ID/healthz")"
if echo "$body" | grep -q '"status":"ok"'; then
  ok "GET https://$DOMAIN/svc/$ID/healthz routed to upstream (body=$body)"
else
  no "path route /svc/$ID/healthz (body=$body)"
fi

# 3) POST /svc/<slug>/echo round-trips method + custom header + body.
er="$(curl -s --cacert "$ROOT" "${RES[@]}" -X POST -H 'X-Acme-T: y' -d 'hi-acme' "https://$DOMAIN/svc/$ID/echo")"
if echo "$er" | grep -q '"body":"hi-acme"' \
  && echo "$er" | grep -q '"method":"POST"' \
  && echo "$er" | grep -q '"X-Acme-T":\["y"\]'; then
  ok "POST https://$DOMAIN/svc/$ID/echo echoes method+header+body"
else
  no "path route POST /svc/$ID/echo (resp=$er)"
fi

# 4) the proxy strips the /svc/<slug> prefix before the upstream (the echo upstream
#    reports the path it actually received — must be /echo, not /svc/<slug>/echo).
if echo "$er" | grep -q '"path":"/echo"'; then
  ok "proxy stripped the /svc/$ID prefix (upstream saw /echo)"
else
  no "prefix strip (upstream path in resp=$er)"
fi

# 5) Location rewrite: the upstream's 302 -> /redirected must come back rewritten
#    under /svc/<slug>/redirected (proxy uses X-Burrow-Path-Prefix; only path-
#    absolute Locations are rewritten, which is what the upstream emits).
loc="$(curl -s -o /dev/null -D - --cacert "$ROOT" "${RES[@]}" "https://$DOMAIN/svc/$ID/redirect" \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')"
if echo "$loc" | grep -q "/svc/$ID/redirected"; then
  ok "Location header rewritten under /svc/$ID (got: $loc)"
else
  no "Location rewrite (got: ${loc:-<none>})"
fi

# 6) control channel :7000 presents a cert chaining to Pebble's root.
#    -verify_return_error makes the handshake exit non-zero on a bad chain.
if echo | openssl s_client -connect "$DOMAIN:7000" -servername "$DOMAIN" \
  -CAfile "$ROOT" -verify_return_error >/tmp/sc.out 2>&1 \
  && grep -q 'Verify return code: 0 (ok)' /tmp/sc.out; then
  ok ":7000 control channel presents a cert chaining to Pebble's root"
else
  no ":7000 control-channel cert chain (s_client output below)"
  sed -n '1,12p' /tmp/sc.out
fi

echo "ACME_ASSERT_RESULT pass=$pass fail=$fail"
if [ "$fail" -gt 0 ]; then
  printf '  - %s\n' "${fails[@]}"
  exit 1
fi
exit 0
