#!/usr/bin/env bash
# test-only — never deploy this shape.
# Tier-1 gate for the client CLI: the three lines of the Connect page, run on
# "a machine with nothing installed".
#
#   machine (debian slim: curl, ca-certificates)  --->  relay (burrowd, this branch)
#     curl -fsSL https://relay.test.local/install.sh | sh
#     burrow login relay.test.local
#     burrow http 3000
#
# The relay serves the dashboard on :443 and the control endpoint on :7000
# with the harness's test certificate; the machine trusts the harness CA the
# way a real machine trusts a public one, so no command needs a TLS flag. The
# client the installer downloads is this branch's, packed like the rolling
# develop build and served from BURROW_CLIENT_DOWNLOAD_DIR. Nothing is
# published on the host: every request is made inside the two containers.
#
# Also: an OLD client (built from OLD_REF) against this relay, and the new
# client's `burrow connect` next to it, log line for log line.
#
# Usage:
#   bash test/integration/client-cli.sh           # up / assert / down
#   bash test/integration/client-cli.sh --keep    # leave the two containers running
#   OLD_REF=refs/heads/develop   the commit the old client is built from
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1
OLD_REF="${OLD_REF:-refs/heads/develop}"
NET="burrow-client-cli-it"
RELAY="$NET-relay"
MACHINE="$NET-machine"
IMG_RELAY="burrow-client-cli-it:relay"
IMG_MACHINE="burrow-client-cli-it:machine"
IMG_OLD="burrow-client-cli-it:old-client"
HOST="relay.test.local"
ADMIN_EMAIL="admin@e2e.local"
ADMIN_PASS="e2e-pass-12345"
WORK="$(mktemp -d)"

PASS=0; FAIL=0; SKIP=0
check() { if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS  $1"; else FAIL=$((FAIL+1)); echo "FAIL  $1 (want '$3', got '$2')"; fi; }
skip()  { SKIP=$((SKIP+1)); echo "SKIP  $1 ($2)"; }
# has <text> <fixed string>: "yes" or "no"
has()   { if printf '%s' "$1" | grep -qF -- "$2"; then echo yes; else echo no; fi; }

teardown() {
  rm -rf "$WORK"
  if [ "$KEEP" = "1" ]; then echo "[client-cli] --keep: $RELAY and $MACHINE left running ($ADMIN_EMAIL / $ADMIN_PASS)"; return; fi
  echo "[client-cli] tearing down"
  docker rm -f -v "$MACHINE" "$RELAY" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap teardown EXIT

# ---------------------------------------------------------------- build ----
HAVE_OLD=0
if git rev-parse --verify --quiet "$OLD_REF^{commit}" >/dev/null; then
  echo "[client-cli] building the old client from $OLD_REF ($(git rev-parse --short "$OLD_REF^{commit}"))"
  git archive "$OLD_REF" | docker build -q -f test/harness/Dockerfile.client -t "$IMG_OLD" - >/dev/null
  HAVE_OLD=1
else
  echo "[client-cli] $OLD_REF is not a commit here: the old-client lines are skipped"
  # The machine image copies from this tag; give it a stand-in.
  docker build -q -t "$IMG_OLD" - >/dev/null <<'EOF'
FROM alpine:3.20
RUN mkdir -p /usr/local/bin && printf '#!/bin/sh\nexit 127\n' >/usr/local/bin/burrow && chmod +x /usr/local/bin/burrow
EOF
fi

echo "[client-cli] building the relay and the machine from this tree"
build() { # <target> <tag>
  docker build -q --target "$1" -t "$2" --build-arg OLD_IMAGE="$IMG_OLD" -f - . >/dev/null <<'EOF'
ARG OLD_IMAGE
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/burrowd ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/burrow ./cmd/client \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/upstream ./test/harness/upstream
# Packed like the rolling develop build, which is what an untagged relay hands out.
RUN mkdir /dl && tar -czf "/dl/burrow_linux_$(go env GOARCH).tar.gz" -C /out burrow \
 && cd /dl && sha256sum -- * >checksums.txt

FROM alpine:3.20 AS relay
RUN apk add --no-cache curl ca-certificates \
 && adduser -D -u 65532 burrow && mkdir -p /data /certs && chown burrow:burrow /data
COPY --from=build /out/burrowd /usr/local/bin/burrowd
COPY --from=build /dl /dl
COPY test/harness/certs/ca.crt test/harness/certs/wildcard.test.local.crt test/harness/certs/wildcard.test.local.key /certs/
USER 65532
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/burrowd"]

FROM ${OLD_IMAGE} AS old

# The machine with nothing installed: curl and ca-certificates. The harness CA
# is in its trust store, as a public CA is in a real one. /opt/test holds the
# test's own tools, off the PATH: a tiny HTTP server and the old client.
FROM debian:stable-slim AS machine
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash tester
COPY test/harness/certs/ca.crt /usr/local/share/ca-certificates/burrow-test-ca.crt
RUN update-ca-certificates >/dev/null
COPY --from=build /out/upstream /opt/test/upstream
COPY --from=old /usr/local/bin/burrow /opt/test/old-burrow
USER tester
WORKDIR /home/tester
CMD ["sleep", "infinity"]
EOF
}
build relay "$IMG_RELAY"
build machine "$IMG_MACHINE"

# ------------------------------------------------------------------- up ----
docker rm -f -v "$MACHINE" "$RELAY" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null 2>&1 || true

echo "[client-cli] starting the relay"
docker run -d --name "$RELAY" --network "$NET" --network-alias "$HOST" \
  --tmpfs /data:rw,uid=65532,gid=65532 \
  -e BURROW_ADMIN_EMAIL="$ADMIN_EMAIL" -e BURROW_ADMIN_PASSWORD="$ADMIN_PASS" \
  -e BURROW_DATABASE_PATH=/data/burrow.db -e BURROW_AUTH_DOMAIN="$HOST" \
  -e BURROW_HTTP_LISTEN=:443 \
  -e BURROW_HTTP_TLS_CERT=/certs/wildcard.test.local.crt -e BURROW_HTTP_TLS_KEY=/certs/wildcard.test.local.key \
  -e BURROW_TLS_CERT=/certs/wildcard.test.local.crt -e BURROW_TLS_KEY=/certs/wildcard.test.local.key \
  -e BURROW_CLIENT_DOWNLOAD_DIR=/dl \
  "$IMG_RELAY" serve >/dev/null
# The dashboard API, called inside the relay's container with a cookie jar there.
B="https://$HOST"
api()   { docker exec "$RELAY" curl -sS --cacert /certs/ca.crt -b /tmp/jar -c /tmp/jar "$@"; }
for i in $(seq 1 60); do
  api -o /dev/null -f "$B/healthz" 2>/dev/null && break
  [ "$i" = "60" ] && { echo "relay not healthy"; docker logs --tail 40 "$RELAY"; exit 1; }
  sleep 1
done

echo "[client-cli] starting the machine"
docker run -d --name "$MACHINE" --network "$NET" "$IMG_MACHINE" >/dev/null
docker exec -d "$MACHINE" /opt/test/upstream --addr 127.0.0.1:3000
# m <script>: a shell command on the machine, as its user, with the installer's
# directory on the PATH as the installer's hint says. No BURROW_* variable is set.
m()  { docker exec "$MACHINE" sh -c "PATH=\"\$HOME/.local/bin:\$PATH\"; $1"; }
# mrc <script>: the same, printing only the exit status.
mrc() { set +e; docker exec "$MACHINE" sh -c "PATH=\"\$HOME/.local/bin:\$PATH\"; $1" >/dev/null 2>&1; echo $?; set -e; }
# mbg <script>: the same, left running in the background of the container.
mbg() { docker exec -d "$MACHINE" sh -c "PATH=\"\$HOME/.local/bin:\$PATH\"; $1"; }
# wait_for <file on the machine> <fixed text> [seconds]: "yes" once the file holds the text.
wait_for() {
  local i
  for i in $(seq 1 $(( ${3:-30} * 5 ))); do
    if docker exec "$MACHINE" grep -qF -- "$2" "$1" 2>/dev/null; then echo yes; return; fi
    sleep 0.2
  done
  echo no
}
stop_clients() { docker exec "$MACHINE" sh -c 'for p in /proc/[0-9]*; do case "$(tr "\0" " " <$p/cmdline 2>/dev/null)" in *burrow\ http*|*burrow\ connect*) kill "${p#/proc/}" 2>/dev/null;; esac; done; true'; sleep 1; }
admin_login() {
  api -o /dev/null -X POST "$B/api/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}"
  CSRF=$(docker exec "$RELAY" sh -c "grep burrow_csrf /tmp/jar | awk '{print \$NF}'")
}
amut() { api -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -X "$1" "$B$2" ${3:+-d "$3"}; }

check "0. nothing is installed: burrow is not a command on the machine" "$(mrc 'burrow version')" "127"
check "0. the tiny server answers on 127.0.0.1:3000" "$(m 'curl -fsS http://127.0.0.1:3000/healthz')" '{"status":"ok"}'

# ------------------------------------------------------- 1. the install ----
# As typed into a new shell: the PATH is the image's, without ~/.local/bin.
OUT=$(docker exec "$MACHINE" sh -c "curl -fsSL $B/install.sh | sh" 2>&1) || true
check "1. install line exits 0 and names the next step" "$(has "$OUT" "Next: burrow login $HOST")" "yes"
HINT_DIR=$(printf '%s\n' "$OUT" | sed -n 's/^ *export PATH="\([^:]*\):.*/\1/p' | head -n 1)
check "1. the PATH hint names ~/.local/bin" "$HINT_DIR" "/home/tester/.local/bin"
check "1. burrow is in the PATH hint's directory and runs" "$(mrc "$HINT_DIR/burrow version")" "0"

# ------------------------------------------- 2.-4. sign in, in the browser ----
mbg "burrow login $HOST --no-browser >/tmp/login.out 2>/tmp/login.err; echo \$? >/tmp/login.rc"
check "2. login prints a page of the relay with a code" "$(wait_for /tmp/login.out "$B/link?code=")" "yes"
CODE=$(m "sed -n 's|.*/link?code=\([A-Z0-9-]*\).*|\1|p' /tmp/login.out" | head -n 1)
check "2. the code has the shape XXXX-XXXX" "$(printf '%s' "$CODE" | grep -cE '^[A-Z2-9]{4}-[A-Z2-9]{4}$')" "1"
check "2. the login waits: it has not ended" "$(mrc 'test -e /tmp/login.rc')" "1"

admin_login
REQ=$(api "$B/api/v1/client/login/requests/$CODE")
check "3. the dashboard API finds the request by its code, pending" "$(printf '%s' "$REQ" | jq -r .status)" "pending"
check "3. the request names the machine" "$(printf '%s' "$REQ" | jq -r .hostname)" "$(docker exec "$MACHINE" cat /etc/hostname)"
check "3. approve answers approved" "$(amut POST "/api/v1/client/login/requests/$CODE/approve" '{"token_name":"three-lines"}' | jq -r .status)" "approved"

check "4. the login ends" "$(wait_for /tmp/login.rc "" 20)" "yes"
check "4. login exits 0" "$(m 'cat /tmp/login.rc')" "0"
check "4. login prints \"signed in as\" with the approver" "$(has "$(m 'cat /tmp/login.out')" "signed in as $ADMIN_EMAIL")" "yes"
CFG=/home/tester/.config/burrow/config.yaml
check "4. the user config exists with mode 600" "$(m "stat -c %a $CFG")" "600"
TOKEN1=$(m "sed -n 's/^ *token: *//p' $CFG" | tr -d '"'"'"' \r')
check "4. it holds a token" "$([ "${#TOKEN1}" -gt 20 ] && echo yes || echo no)" "yes"
check "4. the token is in no output of the login" "$(has "$(m 'cat /tmp/login.out /tmp/login.err')" "$TOKEN1")" "no"

# ------------------------------------------------------------ 5. status ----
OUT=$(m 'burrow status' 2>&1) && RC=0 || RC=$?
check "5. status exits 0" "$RC" "0"
check "5. status shows the relay" "$(has "$OUT" "$B")" "yes"
check "5. status shows the token's last four characters" "$(has "$OUT" "${TOKEN1: -4}")" "yes"
check "5. status does not show the token" "$(has "$OUT" "$TOKEN1")" "no"
check "5. status shows no more of the token than that" "$(has "$OUT" "${TOKEN1: -5}")" "no"

# ------------------------------------------------------------ 6. doctor ----
OUT=$(m 'burrow doctor' 2>&1) && RC=0 || RC=$?
check "6. doctor exits 0" "$RC" "0"
check "6. doctor: no check fails or warns" "$(printf '%s\n' "$OUT" | grep -cE '^ *(fail|warn|✗|!)')" "0"
check "6. doctor: at least five checks pass" "$([ "$(printf '%s\n' "$OUT" | grep -cE '^ *(ok|✓)')" -ge 5 ] && echo yes || echo no)" "yes"
check "6. doctor does not show the token" "$(has "$OUT" "$TOKEN1")" "no"

# -------------------------------------------------- 7.-9. burrow http 3000 ----
url_in() { m "sed -n 's|.*\(https://$HOST/svc/[a-z0-9-]*/\).*|\1|p' $1" | head -n 1; }
mbg "burrow http 3000 --log text >/tmp/http1.out 2>&1"
check "7. burrow http 3000 prints a line with the service URL" "$(wait_for /tmp/http1.out "$B/svc/")" "yes"
URL1=$(url_in /tmp/http1.out)
check "8. the service URL answers through the relay with the tiny server's answer" "$(m "curl -fsS ${URL1}healthz" 2>&1)" '{"status":"ok"}'
check "8. a POST with a body arrives" "$(m "curl -fsS -X POST -d hi ${URL1}echo" 2>&1 | jq -r .body)" "hi"
stop_clients
check "9. stopped: the URL answers no more with the server's answer" "$(mrc "curl -fsS -m 5 ${URL1}healthz")" "22"
mbg "burrow http 3000 --log text >/tmp/http2.out 2>&1"
check "9. second run registers" "$(wait_for /tmp/http2.out "$B/svc/")" "yes"
check "9. second run: the SAME URL" "$(url_in /tmp/http2.out)" "$URL1"
stop_clients

# ------------------------------------- 10. options for a service that exists ----
mbg "burrow http 3000 --access login --slug other --log text >/tmp/http3.out 2>&1"
check "10. third run registers" "$(wait_for /tmp/http3.out "$B/svc/")" "yes"
sleep 1
OUT=$(m 'cat /tmp/http3.out')
check "10. the note says the service exists and its settings are kept" "$(has "$OUT" "already exists")" "yes"
check "10. the note links to the dashboard" "$(has "$OUT" "$B/services")" "yes"
check "10. the URL is unchanged" "$(url_in /tmp/http3.out)" "$URL1"
check "10. the service is still open: it answers without a login" "$(m "curl -fsS ${URL1}healthz" 2>&1)" '{"status":"ok"}'
stop_clients

# ------------------------------------------------------------ 11. logout ----
check "11. logout exits 0" "$(mrc 'burrow logout')" "0"
check "11. the user config is gone" "$(mrc "test -e $CFG")" "1"
check "11. status exits 3" "$(mrc 'burrow status')" "3"

# ------------------------------------------- 12. sign in with --token - ----
TOKEN2=$(amut POST /api/v1/tokens '{"name":"from-stdin"}' | jq -r .token)
check "12. the API mints a token" "$([ "${#TOKEN2}" -gt 20 ] && echo yes || echo no)" "yes"
# The token reaches the machine in a file, as from a password manager; the
# command is typed into an interactive shell that keeps a history.
printf '%s\n' "$TOKEN2" | docker exec -i "$MACHINE" sh -c 'umask 077; cat >/home/tester/token.txt'
OUT=$(docker exec -i -e HISTFILE=/home/tester/.bash_history "$MACHINE" bash --norc -i 2>&1 <<EOF || true
export PATH="\$HOME/.local/bin:\$PATH"
burrow login $HOST --token - <\$HOME/token.txt
exit
EOF
)
check "12. login --token - signs in" "$(has "$OUT" "Signed in to $B")" "yes"
check "12. the user config is back with mode 600" "$(m "stat -c %a $CFG")" "600"
check "12. the shell history holds the command" "$(has "$(m 'cat ~/.bash_history')" "burrow login $HOST --token -")" "yes"
check "12. the shell history does not hold the token" "$(has "$(m 'cat ~/.bash_history')" "$TOKEN2")" "no"
check "12. the login printed no token" "$(has "$OUT" "$TOKEN2")" "no"
mbg "burrow http 3000 --log text >/tmp/http4.out 2>&1"
check "12. the client runs with the stored sign-in" "$(wait_for /tmp/http4.out "$B/svc/")" "yes"
PSOUT=$(m 'for p in /proc/[0-9]*; do tr "\0" " " <$p/cmdline 2>/dev/null; echo; done')
check "12. the running client is in the process list" "$(has "$PSOUT" "burrow http 3000")" "yes"
check "12. no process's command line holds the token" "$(has "$PSOUT" "$TOKEN2")" "no"
check "12. the client's log does not hold the token" "$(has "$(m 'cat /tmp/http4.out')" "$TOKEN2")" "no"
stop_clients

# --------------------------------------- 13.-14. burrow connect, old and new ----
# The same flags for both. What differs between two runs is cut out: the
# time, the ids the relay draws, and the client's own version.
norm() { sed -E 's/^time=[^ ]+ //; s/(session_id|tunnel_id|client_version|version)=[^ ]+/\1=…/g'; }
run_connect() { # <binary> <output file>: registers, serves one request, is stopped
  mbg "$1 connect --server $HOST:7000 --token \"\$(cat \$HOME/token.txt)\" --local 127.0.0.1:3000 --type http >$2 2>&1"
  REG=$(wait_for "$2" "tunnel registered")
  CURL=$(m "curl -fsS $B/svc/\$(sed -n 's|.*/svc/\([a-z0-9-]*\)/.*|\1|p' $2 | head -n 1)/healthz" 2>&1 || true)
  stop_clients
}
if [ "$HAVE_OLD" = "1" ]; then
  run_connect /opt/test/old-burrow /tmp/connect-old.out
  check "13. old client: burrow connect registers on this relay" "$REG" "yes"
  check "13. old client: serves a request" "$CURL" '{"status":"ok"}'
else
  skip "13. old client: burrow connect registers on this relay" "$OLD_REF not found"
  skip "13. old client: serves a request" "$OLD_REF not found"
fi
run_connect burrow /tmp/connect-new.out
check "14. new client: burrow connect with the same flags registers" "$REG" "yes"
check "14. new client: serves a request" "$CURL" '{"status":"ok"}'
if [ "$HAVE_OLD" = "1" ]; then
  m 'cat /tmp/connect-old.out' | norm >"$WORK/old.log"
  m 'cat /tmp/connect-new.out' | norm >"$WORK/new.log"
  if diff -u "$WORK/old.log" "$WORK/new.log" >"$WORK/diff"; then D=same; else D=different; cat "$WORK/diff"; fi
  check "14. the log lines match the old client's, line for line" "$D" "same"
  check "14. and there are lines to compare" "$([ "$(wc -l <"$WORK/new.log")" -ge 2 ] && echo yes || echo no)" "yes"
else
  skip "14. the log lines match the old client's, line for line" "$OLD_REF not found"
fi

# ------------------------------------------------------------ 15. burrow ai ----
if m 'burrow --help' | grep -qE '^ +ai +'; then
  FAIL=$((FAIL+1)); echo "FAIL  15. burrow ai is on the branch but this script has no case for it yet"
else
  skip "15. burrow ai publishes a model server" "burrow ai is not on this branch"
fi

echo "--- result ---"
echo "passed: $PASS   failed: $FAIL   skipped: $SKIP"
[ "$FAIL" -eq 0 ] || exit 1
echo "ALL GREEN"
