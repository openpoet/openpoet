#!/usr/bin/env bash
#
# guard_test.sh - tests for ops/publish-guard/guard.sh and the .githooks
# pre-commit/pre-push wiring, against throwaway repositories with a local bare
# remote. Needs gitleaks (make tools); skips (exit 0) without it.
# Run: make test-publish-guard
#
# Offending values are assembled at runtime so this file passes the guard.
#
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
GUARD="$HERE/guard.sh"
# The suite controls every denylist itself (CI may export a real one).
unset OPENPOET_PUBLISH_DENYLIST_FILE
if ! command -v gitleaks >/dev/null 2>&1; then
    echo "publish guard: SKIP (gitleaks not installed; make tools)"
    exit 0
fi

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }

FAKEHOME="$SANDBOX/home-guardtester"
mkdir -p "$FAKEHOME"
IP="10.255.255.""1"
IP_C="192.168.255.""1"
IP_B="172.31.255.""1"
CLIENT="acme""corp"
OTHER_HOME="/home/""jdoe"
MAC_HOME="/Users/""jdoe"
WIN_HOME="C:\\""Users\\jdoe"
HOST_INTERNAL="build01.acmecorp.inter""nal"
OAUTH="https://accounts.example.com/o/oauth2/auth?client_id=1&state=""Y6fe4jbxUf3TZ0Vm&code_challenge=AElhTi9XsS6wSY5Y"

# fresh NAME: repo with the guard + hooks installed, pushed to a bare origin
fresh() {
    local dir="$SANDBOX/$1"
    git init -q --bare -b main "$dir.origin.git"
    git init -q -b main "$dir"
    git -C "$dir" config user.email t@t; git -C "$dir" config user.name t
    git -C "$dir" config commit.gpgsign false
    mkdir -p "$dir/ops/publish-guard" "$dir/.githooks" "$dir/docs/images"
    cp "$GUARD" "$dir/ops/publish-guard/guard.sh"
    cp "$ROOT/.githooks/pre-push" "$dir/.githooks/pre-push"
    printf '#!/bin/sh\nexec ./ops/publish-guard/guard.sh staged\n' > "$dir/.githooks/pre-commit"
    chmod +x "$dir/.githooks/"*
    printf 'hello\n' > "$dir/a.txt"
    git -C "$dir" add . && git -C "$dir" commit -q --no-verify -m init
    git -C "$dir" remote add origin "$dir.origin.git"
    git -C "$dir" push -q --no-verify origin main
    git -C "$dir" config core.hooksPath .githooks
    printf '%s\n' "$CLIENT" > "$dir/.git/info/publish-denylist"
    printf '%s' "$dir"
}

G() { (cd "$R" && HOME="$FAKEHOME" bash ops/publish-guard/guard.sh "$@" 2>&1); }
# expect NAME WANT PATTERN -- guard args
expect() {
    local name="$1" want="$2" pattern="$3"; shift 4
    local out rc
    out="$(G "$@")"; rc=$?
    if [ "$rc" != "$want" ]; then bad "$name (exit want=$want got=$rc)"; printf '%s\n' "$out" | sed 's/^/      /'; return; fi
    if [ -n "$pattern" ] && ! printf '%s' "$out" | grep -qE -- "$pattern"; then bad "$name (output lacks /$pattern/)"; printf '%s\n' "$out" | sed 's/^/      /'; return; fi
    ok "$name"
}
stage() { printf '%s\n' "$2" > "$R/$1"; git -C "$R" add "$1"; }
reset_index() { git -C "$R" reset -q --hard HEAD; git -C "$R" clean -qfd; }

echo "== publish guard"
R="$(fresh staged)"

stage ok.go 'package ok // nothing to see'
expect "clean staged change passes" 0 "OK" -- staged; reset_index

stage net.go "var host = \"$IP\""
expect "private IP is refused" 1 "private IP" -- staged; reset_index

stage net_b.go "var hosts = []string{\"$IP_B\", \"$IP_C\"}"
expect "private IPs in 172.16/12 and 192.168/16 are refused" 1 "private IP" -- staged; reset_index

stage doc_ip.go 'var host = "192.0.2.10" // RFC 5737 documentation address'
expect "documentation IP is fine" 0 "OK" -- staged; reset_index

stage loop.go 'var host = "127.0.0.1"'
expect "loopback is fine" 0 "OK" -- staged; reset_index

stage path.go "var p = \"$FAKEHOME/projects\""
expect "this machine's home path is refused" 1 "user/home path" -- staged; reset_index

stage cli.md "deployed for ${CLIENT^^} last week"
expect "local denylist match (case-insensitive) is refused" 1 "local denylist" -- staged; reset_index

EXTRA_DENY="$SANDBOX/ci-denylist"
printf '# comment\n%s\n' "wid""getco" > "$EXTRA_DENY"
stage ci.md "built for wid""getco"
expect "a pattern outside every denylist passes" 0 "OK" -- staged
out="$(cd "$R" && HOME="$FAKEHOME" OPENPOET_PUBLISH_DENYLIST_FILE="$EXTRA_DENY" bash ops/publish-guard/guard.sh staged 2>&1)"; rc=$?
[ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "local denylist" && ok "OPENPOET_PUBLISH_DENYLIST_FILE adds denylist patterns" || bad "OPENPOET_PUBLISH_DENYLIST_FILE adds denylist patterns (rc=$rc)"
out="$(cd "$R" && HOME="$FAKEHOME" OPENPOET_PUBLISH_DENYLIST_FILE="$SANDBOX/missing" bash ops/publish-guard/guard.sh staged 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && ok "a missing OPENPOET_PUBLISH_DENYLIST_FILE is ignored" || bad "a missing OPENPOET_PUBLISH_DENYLIST_FILE is ignored (rc=$rc)"
reset_index

stage home.go "var p = \"$OTHER_HOME/.claude/projects\""
expect "another user's home path is refused" 1 "user home path" -- staged; reset_index
stage home.md "see $MAC_HOME/Library and $WIN_HOME\\AppData"
expect "macOS and Windows home paths are refused" 1 "user home path" -- staged; reset_index
stage fixture_home.go 'var p = "/home/dev/project" // and /Users/dev, C:\Users\dev'
expect "neutral fixture users are fine" 0 "OK" -- staged; reset_index

stage hosts.go "var h = \"$HOST_INTERNAL\""
expect "internal host name is refused" 1 "internal host name" -- staged; reset_index
stage local_url.md "open http://nas.""local:5000/"
expect "URL to a .local host is refused" 1 "internal host name" -- staged; reset_index
stage docker.go 'var h = "host.docker.internal" // branches.local is a JS property'
expect "docker host names and .local properties are fine" 0 "OK" -- staged; reset_index

stage login.md "$OAUTH"
expect "OAuth link with state/PKCE is refused" 1 "OAuth link" -- staged; reset_index
stage links.md 'https://example.com/docs?page=2&state=open and https://example.com/cb?code=abc'
expect "ordinary query strings are fine" 0 "OK" -- staged; reset_index

stage AGENTS.md '# agents'
expect "configsync AGENTS.md is refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.codex"
stage .codex/config.toml '[mcp_servers]'
expect "configsync .codex/config.toml is refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.agents/skills/x"; stage .agents/skills/x/SKILL.md 'skill'
expect "configsync .agents skills are refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.github/hooks" "$R/.acp"
stage .github/hooks/hooks.json '{}'
expect "configsync .github/hooks is refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.github" "$R/.acp"
stage .github/copilot-instructions.md 'x'
expect "configsync copilot instructions are refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.acp"; stage .acp/instructions.md 'x'
expect "configsync .acp instructions are refused" 1 "generated by configsync" -- staged; reset_index
mkdir -p "$R/.github/workflows"; stage .github/workflows/ci.yml 'on: push'
expect "other .github files are fine" 0 "OK" -- staged; reset_index

stage fixture.go "var host = \"$IP\" // publish-guard:allow"
expect "explicit allow marker skips the line" 0 "OK" -- staged; reset_index

stage .env 'FOO=bar'
expect ".env is refused" 1 "\\.env file" -- staged; reset_index
stage .env.example 'FOO='
expect ".env.example is allowed" 0 "OK" -- staged; reset_index

stage debug.log 'x'
expect "log file is refused" 1 "log file" -- staged; reset_index
stage prod.db 'x'
expect "database file is refused" 1 "database file" -- staged; reset_index

printf 'PNG' > "$R/shot.png"; git -C "$R" add shot.png
expect "screenshot outside docs/images is refused" 1 "image/video/PDF" -- staged; reset_index
mkdir -p "$R/docs/images"; printf 'PNG' > "$R/docs/images/ui.png"; git -C "$R" add docs/images/ui.png
expect "image under docs/images is allowed" 0 "OK" -- staged; reset_index

head -c 600000 /dev/zero > "$R/blob.bin"; git -C "$R" add blob.bin
expect "binary over 512 KiB is refused" 1 "binary file larger" -- staged; reset_index

TOKEN="ghp_""$(printf 'aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5')"
stage cfg.go "const githubToken = \"$TOKEN\""
expect "secret is caught by gitleaks" 1 "gitleaks found secrets" -- staged; reset_index

out="$(cd "$R" && HOME="$FAKEHOME" PATH="/usr/bin:/bin" bash ops/publish-guard/guard.sh staged 2>&1)"; rc=$?
[ "$rc" -eq 2 ] && printf '%s' "$out" | grep -q "fail closed" && ok "missing gitleaks fails closed" || bad "missing gitleaks fails closed (rc=$rc)"

echo "== hooks"
R="$(fresh hooks)"
printf 'var host = "%s"\n' "$IP" > "$R/net.go"; git -C "$R" add net.go
if (cd "$R" && HOME="$FAKEHOME" git commit -q -m "add host" >/dev/null 2>&1); then bad "pre-commit hook blocks a private IP"; else ok "pre-commit hook blocks a private IP"; fi
reset_index

printf '%s\n' "$OAUTH" > "$R/login.md"; git -C "$R" add login.md
if (cd "$R" && HOME="$FAKEHOME" git commit -q -m "login link" >/dev/null 2>&1); then bad "pre-commit hook blocks an OAuth link"; else ok "pre-commit hook blocks an OAuth link"; fi
[ "$(git -C "$R" rev-list --count HEAD)" = 1 ] && ok "the blocked commit was not created" || bad "the blocked commit was not created"
reset_index
printf 'usar /home/dev nos exemplos\n' > "$R/notes.md"; git -C "$R" add notes.md
if (cd "$R" && HOME="$FAKEHOME" git commit -q -m "notes" >/dev/null 2>&1); then ok "pre-commit hook lets a clean commit through"; else bad "pre-commit hook lets a clean commit through"; fi
git -C "$R" rm -q --cached a.txt && printf 'a.txt\n' > "$R/.gitignore" && git -C "$R" add .gitignore
if (cd "$R" && HOME="$FAKEHOME" git commit -q -m "untrack" >/dev/null 2>&1); then ok "untracking a file passes"; else bad "untracking a file passes"; fi
git -C "$R" push -q --no-verify origin main

M="$SANDBOX/make-install"; git init -q "$M"
make -s -C "$M" -f "$ROOT/Makefile" help >/dev/null 2>&1
[ "$(git -C "$M" config --get core.hooksPath)" = ".githooks" ] && ok "any make run installs the hooks" || bad "any make run installs the hooks"
git -C "$M" config core.hooksPath custom-hooks
out="$(make -s -C "$M" -f "$ROOT/Makefile" help 2>&1 >/dev/null)"
[ "$(git -C "$M" config --get core.hooksPath)" = "custom-hooks" ] && printf '%s' "$out" | grep -q "not active" && ok "a custom hooksPath is kept, with a warning" || bad "a custom hooksPath is kept, with a warning"

printf 'fine\n' > "$R/b.txt"; git -C "$R" add b.txt
(cd "$R" && HOME="$FAKEHOME" git commit -q -m "deploy notes for $CLIENT" >/dev/null 2>&1)
expect "commit message is checked in ranges" 1 "message: matches the local denylist" -- unpushed
if (cd "$R" && HOME="$FAKEHOME" git push -q origin main >/dev/null 2>&1); then bad "pre-push hook blocks the bad commit"; else ok "pre-push hook blocks the bad commit"; fi
[ "$(git -C "$R" rev-parse origin/main)" != "$(git -C "$R" rev-parse main)" ] && ok "nothing reached the remote" || bad "nothing reached the remote"

git -C "$R" commit -q --amend --no-verify -m "add notes"
if (cd "$R" && HOME="$FAKEHOME" git push -q origin main >/dev/null 2>&1); then ok "pre-push passes after rewriting the local commit"; else bad "pre-push passes after rewriting the local commit"; fi

git -C "$R" checkout -q -b feature
printf 'var host = "%s"\n' "$IP" > "$R/n2.go"; git -C "$R" add n2.go; git -C "$R" commit -q --no-verify -m "feature"
if (cd "$R" && HOME="$FAKEHOME" git push -q origin feature >/dev/null 2>&1); then bad "pre-push checks a brand-new branch"; else ok "pre-push checks a brand-new branch"; fi

echo
echo "publish guard: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
