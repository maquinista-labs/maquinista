#!/usr/bin/env bash
# deploy-barceloneta.sh — ship playa changes to the barceloneta runtime.
#
# Dev happens on playa (~/code/maquinista); maquinista RUNS on barceloneta
# (/root/git/maquinista, systemd unit `maquinista`). This script is the whole
# deployment procedure in one command:
#
#   ./scripts/deploy-barceloneta.sh          # from playa — ssh-trampolines itself
#   MAQ_DEPLOY_LOCAL=1 ./scripts/...         # force local run (e.g. ON barceloneta)
#
# Local (barceloneta) steps:
#   1. git pull --rebase origin main      (deploy = fast-forward to what playa pushed)
#   2. SKIP_DASHBOARD=1 make build-go     (binary only — dashboard ships as standalone.tgz)
#   3. ./maquinista migrate               (migrations only ever ADD files — idempotent)
#   4. systemctl restart maquinista
#   5. health check: unit active + no error/panic/fatal in the first 30s of logs
set -euo pipefail

REPO_DIR="/root/git/maquinista"
UNIT="maquinista"

# ---- trampoline: not on barceloneta? ssh there and run this same script ----
if [ "$(hostname)" != "barceloneta" ] && [ "${MAQ_DEPLOY_LOCAL:-0}" != "1" ]; then
  # playa-side preflight: deploy pulls what was PUSHED — refuse to ship stale code
  if [ -n "$(git -C "${BASH_SOURCE[0]%/*}/.." rev-list origin/main..main 2>/dev/null)" ]; then
    echo "ERROR: local main has unpushed commits. Push first: git -C ~/code/maquinista push origin main" >&2
    exit 1
  fi
  echo "==> trampolining to barceloneta..."
  exec ssh barceloneta "$REPO_DIR/scripts/deploy-barceloneta.sh"
fi

cd "$REPO_DIR"
export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"  # non-interactive ssh has a bare PATH

echo "==> git pull --rebase (was $(git rev-parse --short HEAD))"
git pull --rebase origin main
echo "==> now at $(git rev-parse --short HEAD) $(git log -1 --format=%s)"

echo "==> building (SKIP_DASHBOARD=1 make build-go)"
SKIP_DASHBOARD=1 make build-go

echo "==> migrating"
./maquinista migrate

echo "==> restarting $UNIT"
systemctl restart "$UNIT"
sleep 4

if ! systemctl is-active --quiet "$UNIT"; then
  echo "ERROR: $UNIT is not active after restart. Recent logs:" >&2
  journalctl -u "$UNIT" --since "-2 min" --no-pager | tail -30 >&2
  exit 1
fi

# 30s observation window for panics/startup errors, then report
sleep 1
if journalctl -u "$UNIT" --since "-30 sec" --no-pager | grep -qiE "panic|fatal|ERROR"; then
  echo "WARN: error lines in startup logs:"
  journalctl -u "$UNIT" --since "-30 sec" --no-pager | grep -iE "panic|fatal|ERROR" | tail -10
fi

echo "==> OK: $UNIT active, deployed $(git rev-parse --short HEAD). Agents are reconciled from the DB on start."
