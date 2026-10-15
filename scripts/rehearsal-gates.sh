#!/usr/bin/env bash
# Rehearsal gate check — docs/pilot-onboarding.md §0 made mechanical.
#
# Verifies every prerequisite row is verifiably landed BEFORE the first
# external user is onboarded ("re-check this table at rehearsal time").
# Fails closed: any FAIL exits 1 and onboarding must not proceed.
#
# Usage: scripts/rehearsal-gates.sh [path-to-runtime-.env]
#        (default: ../.env relative to this script's repo checkout of
#         the runtime checkout /home/barceloneta/code/maquinista/.env)
#
# Gates:
#   P1  per-user repo binding   USER_REPOS present + parseable (userID:repoRoot pairs)
#   P2  per-user session cap    MAQUINISTA_MAX_AGENTS_PER_USER unset (default 2) or int >= 1
#   P3  cost ledger             `maquinista ledger --by user` runs clean
#   P4  onboarding runbook      docs/pilot-onboarding.md present in checkout

set -u

fail=0
note() { printf '  %s\n' "$*"; }
pass() { printf '[PASS] %s\n' "$1"; }
fail_gate() { printf '[FAIL] %s\n' "$1"; fail=1; }

ENV_FILE="${1:-/home/barceloneta/code/maquinista/.env}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Binary discovery: this checkout, then the deployed runtime checkout (the
# daemon runs from the same dir as the env file), then $PATH.
MAQ_BIN="${MAQUINISTA_BIN:-}"
[ -x "$MAQ_BIN" ] || MAQ_BIN="$REPO_ROOT/maquinista"
[ -x "$MAQ_BIN" ] || MAQ_BIN="$(dirname "$ENV_FILE")/maquinista"
[ -x "$MAQ_BIN" ] || MAQ_BIN="$(command -v maquinista || true)"

env_get() { # env_get VAR — last non-comment assignment from the env file
    grep -E "^${1}=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2-
}

echo "== F0 rehearsal gate check ($(date -u +%FT%TZ)) =="

# ── P4: runbook present (everything else references it) ──────────────
if [ -f "$REPO_ROOT/docs/pilot-onboarding.md" ]; then
    pass "P4 onboarding runbook present (docs/pilot-onboarding.md)"
else
    fail_gate "P4 runbook docs/pilot-onboarding.md missing from $REPO_ROOT"
fi

# ── P1: per-user repo binding configured (MAQ-45) ────────────────────
# Unset USER_REPOS is a FAIL here even though the code tolerates it:
# config.MayAccessRepo treats users without an entry as UNRESTRICTED,
# i.e. the guard is inert exactly when it is needed most. Runbook §0:
# "do not onboard around a missing guard".
user_repos="$(env_get USER_REPOS)"
if [ -z "$user_repos" ]; then
    fail_gate "P1 USER_REPOS unset in $ENV_FILE — repo binding inert; add '<tg-id>:<repo-root>' pairs before any invite"
else
    bad=""
    IFS=',' read -ra pairs <<< "$user_repos"
    for pair in "${pairs[@]}"; do
        pair="$(echo "$pair" | xargs)"
        [ -z "$pair" ] && continue
        if ! echo "$pair" | grep -qE '^[0-9]+:.+$'; then
            bad="$bad '$pair'"
        fi
    done
    if [ -n "$bad" ]; then
        fail_gate "P1 USER_REPOS malformed pair(s):$bad (expected <userID>:<repoRoot>)"
    else
        pass "P1 USER_REPOS configured: $user_repos"
    fi
fi

# ── P2: per-user session cap (MAQ-46) ────────────────────────────────
# Default N=2 when unset (internal/config/config.go); invalid values are
# a load-time error — check them here so the operator sees it pre-restart.
cap="$(env_get MAQUINISTA_MAX_AGENTS_PER_USER)"
if [ -z "$cap" ]; then
    pass "P2 session cap at default N=2 (MAQUINISTA_MAX_AGENTS_PER_USER unset)"
elif echo "$cap" | grep -qE '^[1-9][0-9]*$'; then
    pass "P2 session cap N=$cap"
else
    fail_gate "P2 MAQUINISTA_MAX_AGENTS_PER_USER='$cap' invalid — must be integer >= 1 (daemon will refuse to load)"
fi

# ── P3: cost ledger rollup runs (MAQ-47) ─────────────────────────────
if [ -z "$MAQ_BIN" ] || [ ! -x "$MAQ_BIN" ]; then
    fail_gate "P3 maquinista binary not found (set MAQUINISTA_BIN or build with make build-go)"
elif "$MAQ_BIN" ledger --by user --limit 1 >/dev/null 2>&1; then
    pass "P3 ledger rollup verb runs (maquinista ledger --by user)"
else
    fail_gate "P3 maquinista ledger --by user failed — MAQ-47 rollup not deployed or DB unreachable"
fi

echo
if [ "$fail" -eq 0 ]; then
    echo "All gates green — proceed with runbook §2 per-user onboarding."
else
    echo "GATES RED — fix the [FAIL] rows before onboarding anyone (runbook §0)."
fi
exit "$fail"
