#!/usr/bin/env bash
#
# gate_test.sh - tests for ops/deploy-gate/gate.sh against throwaway git
# repositories with a local bare "origin". Run: make test-deploy-gate
#
set -uo pipefail

GATE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gate.sh"
SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
PASS=0; FAIL=0

ok()  { PASS=$((PASS+1)); printf '  PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }

# expect NAME WANT_EXIT [GREP_PATTERN] -- gate args...
OUT=""
expect() {
    local name="$1" want="$2" pattern="$3"; shift 4
    local rc
    OUT="$(bash "$GATE" "$@" 2>&1)"; rc=$?
    if [ "$rc" != "$want" ]; then bad "$name (exit want=$want got=$rc)"; printf '%s\n' "$OUT" | sed 's/^/      /'; return; fi
    if [ -n "$pattern" ] && ! printf '%s' "$OUT" | grep -qE -- "$pattern"; then
        bad "$name (output lacks /$pattern/)"; printf '%s\n' "$OUT" | sed 's/^/      /'; return
    fi
    ok "$name"
}

# fresh REPO with a pushed main; prints the work-tree path
fresh() {
    local dir="$SANDBOX/$1"
    git init -q --bare -b main "$dir.origin.git"
    git init -q -b main "$dir"
    git -C "$dir" config user.email t@t; git -C "$dir" config user.name t
    git -C "$dir" config commit.gpgsign false
    printf 'build/\n' > "$dir/.gitignore"
    printf 'a\n' > "$dir/a.txt"
    git -C "$dir" add . && git -C "$dir" commit -qm init
    git -C "$dir" remote add origin "$dir.origin.git"
    git -C "$dir" push -q origin main
    printf '%s' "$dir"
}

echo "== deploy gate"

R="$(fresh clean)"
expect "clean, pushed main passes" 0 "OK" -- --repo "$R"

mkdir -p "$R/build"; printf 'x' > "$R/build/openpoet"
expect "ignored files do not count" 0 "OK" -- --repo "$R"

R="$(fresh modified)"
printf 'changed\n' >> "$R/a.txt"
expect "modified tracked file fails and is listed" 1 "M a.txt" -- --repo "$R"
printf '%s' "$OUT" | grep -q "nothing was deployed" && ok "failure message is explicit" || bad "failure message is explicit"

R="$(fresh untracked)"
printf 'new\n' > "$R/new.txt"
expect "untracked file fails and is listed" 1 '\?\? new.txt' -- --repo "$R"

R="$(fresh staged)"
printf 'staged\n' > "$R/s.txt"; git -C "$R" add s.txt
expect "staged file fails and is listed" 1 "A  s.txt" -- --repo "$R"

R="$(fresh unpushed)"
printf 'b\n' > "$R/b.txt"; git -C "$R" add b.txt; git -C "$R" commit -qm local
expect "commit not pushed to origin/main fails" 1 "not on 'origin/main'" -- --repo "$R"
git -C "$R" push -q origin main
expect "same commit passes once pushed" 0 "OK" -- --repo "$R"

R="$(fresh feature)"
git -C "$R" checkout -q -b feature
printf 'f\n' > "$R/f.txt"; git -C "$R" add f.txt; git -C "$R" commit -qm feature
git -C "$R" push -q origin feature
expect "commit only on a feature branch fails" 1 "not on local 'main'" -- --repo "$R"

R="$(fresh behind)"
git -C "$R" checkout -q -b old HEAD
git -C "$R" checkout -q main
printf 'c\n' > "$R/c.txt"; git -C "$R" add c.txt; git -C "$R" commit -qm newer; git -C "$R" push -q origin main
git -C "$R" checkout -q old
expect "older commit already in main and origin passes" 0 "OK" -- --repo "$R"
expect "--commit checks the given revision" 1 "does not exist" -- --repo "$R" --commit deadbeefdeadbeef

R="$(fresh nofetch)"
git -C "$R" remote set-url origin "$SANDBOX/does-not-exist.git"
expect "unreachable origin fails closed" 1 "could not fetch" -- --repo "$R"

R="$(fresh bypass)"
printf 'dirty\n' >> "$R/a.txt"
expect "bypass without a reason is refused" 2 "real reason" -- --repo "$R" --emergency-bypass ""
expect "bypass with a short reason is refused" 2 "real reason" -- --repo "$R" --emergency-bypass "fix"
LOG="$SANDBOX/bypass.log"
expect "explicit bypass passes loudly" 0 "EMERGENCY BYPASS" -- --repo "$R" --context test --log "$LOG" --emergency-bypass "production down, hotfix INC-1"
if [ -s "$LOG" ] && grep -q '"reason":"production down, hotfix INC-1"' "$LOG" && grep -q 'M a.txt' "$LOG"; then
    ok "bypass is logged with reason and dirty files"
else
    bad "bypass is logged with reason and dirty files"; cat "$LOG" 2>/dev/null
fi
printf '%s' "$OUT" | grep -q "M a.txt" && ok "bypass still lists the dirty files" || bad "bypass still lists the dirty files"

R="$(fresh noenv)"
printf 'dirty\n' >> "$R/a.txt"
OUT="$(OPENPOET_DEPLOY_GATE_BYPASS=1 DEPLOY_GATE_BYPASS=1 SKIP_DEPLOY_GATE=1 bash "$GATE" --repo "$R" 2>&1)"
[ $? -eq 1 ] && ok "no environment variable bypasses the gate" || bad "no environment variable bypasses the gate"

expect "unknown argument is a usage error" 2 "unknown argument" -- --repo "$R" --force
expect "not a repository fails" 1 "not a git repository" -- --repo "$SANDBOX"

echo
echo "deploy gate: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
