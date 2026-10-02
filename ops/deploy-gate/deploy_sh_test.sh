#!/usr/bin/env bash
#
# deploy_sh_test.sh - checks that .scripts/deploy.sh (host-specific, not
# versioned) wires the deploy gate in. It runs a COPY of deploy.sh inside a
# throwaway git repository with systemctl, systemd-run, make, lsof, curl and
# journalctl stubbed on PATH: it never touches the real production unit.
#
# Skips (exit 0) when .scripts/deploy.sh is absent (e.g. a fresh clone or CI).
# Run: make test-deploy-gate
#
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
DEPLOY="$ROOT/.scripts/deploy.sh"
if [ ! -f "$DEPLOY" ]; then
    echo "deploy.sh wiring: SKIP (.scripts/deploy.sh not present)"
    exit 0
fi

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }

# stubs: every call is appended to $CALLS; systemctl answers "installed"
STUBS="$SANDBOX/stubs"; CALLS="$SANDBOX/calls"; mkdir -p "$STUBS"; : > "$CALLS"
for cmd in systemctl systemd-run make lsof curl journalctl; do
    printf '#!/bin/sh\necho "%s $*" >> "%s"\nexit 0\n' "$cmd" "$CALLS" > "$STUBS/$cmd"
    chmod +x "$STUBS/$cmd"
done

P="$SANDBOX/project"
git init -q --bare -b main "$SANDBOX/origin.git"
git init -q -b main "$P"
git -C "$P" config user.email t@t; git -C "$P" config user.name t; git -C "$P" config commit.gpgsign false
mkdir -p "$P/.scripts" "$P/ops/deploy-gate" "$P/.run"
cp "$DEPLOY" "$P/.scripts/deploy.sh"
cp "$HERE/gate.sh" "$P/ops/deploy-gate/gate.sh"
printf '.run/\n.scripts/\n' > "$P/.gitignore"
printf 'x\n' > "$P/app.txt"
: > "$P/.run/openpoet.env"
git -C "$P" add . && git -C "$P" commit -qm init
git -C "$P" remote add origin "$SANDBOX/origin.git" && git -C "$P" push -q origin main

run() { (cd "$P" && env -u _DEPLOY_DAEMONIZED -u OPENPOET_SESSION_ID PATH="$STUBS:$PATH" bash .scripts/deploy.sh "$@" 2>&1); }
launched() { grep -q '^systemd-run' "$CALLS"; }
stopped()  { grep -qE '^systemctl --user (stop|start) openpoet-prod' "$CALLS"; }

echo "== deploy.sh wiring"

printf 'dirty\n' >> "$P/app.txt"
: > "$CALLS"; OUT="$(run)"; RC=$?
[ "$RC" -eq 1 ] && ok "dirty tree: deploy exits 1" || bad "dirty tree: deploy exits 1 (got $RC)"
printf '%s' "$OUT" | grep -q 'M app.txt' && ok "dirty tree: offending file is listed" || bad "dirty tree: offending file is listed"
launched && bad "dirty tree: nothing launched" || ok "dirty tree: nothing launched"

: > "$CALLS"; OUT="$(run --pull)"; RC=$?
[ "$RC" -eq 1 ] && ! launched && ok "dirty tree: --pull refused" || bad "dirty tree: --pull refused"

: > "$CALLS"; OUT="$(run --rollback)"; RC=$?
[ "$RC" -eq 1 ] && ! stopped && ok "dirty tree: --rollback refused, production untouched" || bad "dirty tree: --rollback refused"

: > "$CALLS"; OUT="$(_DEPLOY_DAEMONIZED=1 PATH="$STUBS:$PATH" bash -c "cd '$P' && bash .scripts/deploy.sh" 2>&1)"; RC=$?
if [ "$RC" -eq 1 ] && ! stopped && ! grep -q '^make' "$CALLS"; then
    ok "direct daemon start still runs the gate (no build, no stop)"
else
    bad "direct daemon start still runs the gate (rc=$RC)"; cat "$CALLS"
fi

: > "$CALLS"; OUT="$(run --emergency-bypass "prod down, hotfix INC-42")"; RC=$?
if [ "$RC" -eq 0 ] && launched && grep -q 'INC-42' "$P/.run/deploy-gate-bypass.log"; then
    ok "explicit bypass launches and is logged in .run/deploy-gate-bypass.log"
else
    bad "explicit bypass launches and is logged (rc=$RC)"; printf '%s\n' "$OUT"
fi
grep -q '_DEPLOY_GATE_BYPASS=prod down, hotfix INC-42' "$CALLS" && ok "bypass reason is handed to the daemon" || bad "bypass reason is handed to the daemon"

git -C "$P" checkout -q -- app.txt
: > "$CALLS"; OUT="$(run)"; RC=$?
[ "$RC" -eq 0 ] && launched && ok "clean pushed main: deploy launches" || { bad "clean pushed main: deploy launches (rc=$RC)"; printf '%s\n' "$OUT"; }

printf 'y\n' > "$P/app.txt"; git -C "$P" commit -qam unpushed
: > "$CALLS"; OUT="$(run)"; RC=$?
[ "$RC" -eq 1 ] && ! launched && printf '%s' "$OUT" | grep -q "not on 'origin/main'" \
    && ok "unpushed commit: deploy refused" || bad "unpushed commit: deploy refused"

mv "$P/ops/deploy-gate/gate.sh" "$P/ops/deploy-gate/gate.sh.off"
git -C "$P" push -q origin main
: > "$CALLS"; OUT="$(run)"; RC=$?
[ "$RC" -eq 1 ] && ! launched && ok "missing gate script: deploy fails closed" || bad "missing gate script: deploy fails closed (rc=$RC)"

echo
echo "deploy.sh wiring: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
