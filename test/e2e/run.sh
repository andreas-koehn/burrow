#!/usr/bin/env bash
# test-only — never deploy this shape.
# Tier-2 runner: bring up the full stack, run the Playwright UI suite
# (mock project), tear down on EXIT. Supersedes the old smoke-ui.sh +
# the Playwright half of the old smoke-full.sh.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"   # test/e2e -> repo root
cd "$REPO_ROOT"
COMPOSE="test/harness/compose.full.yml"
DIR="test/e2e"
# Where the dashboard is published on this host; the compose file and
# test/e2e/fixtures/env.ts read the same variable.
export BURROW_E2E_DASHBOARD_PORT="${BURROW_E2E_DASHBOARD_PORT:-8080}"

teardown() { echo "[e2e:run] tearing down"; cd "$REPO_ROOT"; docker compose -f "$COMPOSE" down --volumes || true; }
trap teardown EXIT

echo "[e2e:run] building + starting full stack (--wait)"
docker compose -f "$COMPOSE" up -d --build --wait

echo "[e2e:run] sanity: dashboard + 3 TCP tunnels"
curl -fsS "http://localhost:${BURROW_E2E_DASHBOARD_PORT}/healthz" >/dev/null
for port in 9002 9003 9004; do
  for attempt in $(seq 1 60); do
    if curl -fsS -m 2 -o /dev/null "http://localhost:${port}/healthz"; then break; fi
    if [ "$attempt" = "60" ]; then echo "  :${port} not ready"; exit 1; fi
    sleep 1
  done
done

echo "[e2e:run] running Playwright (mock project)"
cd "$DIR"
npm ci --no-fund --no-audit
npx playwright install --with-deps chromium
npx playwright test --project=mock
echo "[e2e:run] all specs passed"
