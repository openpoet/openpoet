#!/usr/bin/env bash
#
# gate.sh - mandatory pre-deploy gate for OpenPoet.
#
# Every path that deploys, releases or restarts OpenPoet in production runs
# this BEFORE acting (.scripts/deploy.sh, ops/safe-rollout, the release
# workflow). It passes only when:
#
#   1. the working tree is clean: `git status --porcelain` is empty
#      (modified, staged, deleted AND untracked files all count; .gitignore'd
#      files do not);
#   2. the commit being deployed is contained in the local `main` branch;
#   3. the same commit is contained in `origin/main`, freshly fetched (it has
#      been pushed). A failed fetch fails the gate: we never assume.
#
# On failure it prints every offending file / reason and exits 1.
#
# Emergency bypass (the ONLY one; there is no environment variable):
#   --emergency-bypass "<reason>"   still runs and prints every check, then
#   exits 0 with a loud warning and appends one JSON line (time, user,
#   session, commit, reason, failures) to the bypass log. A reason of fewer
#   than 10 characters is refused. See docs/deploy-gate.md.
#
# Usage:
#   ops/deploy-gate/gate.sh [--repo DIR] [--commit REV] [--context LABEL]
#                           [--log FILE] [--emergency-bypass REASON]
#
# Exit codes: 0 passed (or bypassed), 1 gate failed, 2 usage error.
#

set -uo pipefail

REPO=""
COMMIT="HEAD"
CONTEXT="deploy"
BYPASS_LOG=""
BYPASS_REASON=""
BYPASS_SET=0
REMOTE="origin"
BRANCH="main"

usage() { sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --repo) REPO="${2:-}"; shift 2 ;;
        --repo=*) REPO="${1#*=}"; shift ;;
        --commit) COMMIT="${2:-}"; shift 2 ;;
        --commit=*) COMMIT="${1#*=}"; shift ;;
        --context) CONTEXT="${2:-}"; shift 2 ;;
        --context=*) CONTEXT="${1#*=}"; shift ;;
        --log) BYPASS_LOG="${2:-}"; shift 2 ;;
        --log=*) BYPASS_LOG="${1#*=}"; shift ;;
        --emergency-bypass) BYPASS_SET=1; BYPASS_REASON="${2:-}"; shift 2 ;;
        --emergency-bypass=*) BYPASS_SET=1; BYPASS_REASON="${1#*=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "deploy-gate: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [ "$BYPASS_SET" = "1" ]; then
    trimmed="$(printf '%s' "$BYPASS_REASON" | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//')"
    if [ "${#trimmed}" -lt 10 ]; then
        echo "deploy-gate: --emergency-bypass needs a real reason (at least 10 characters)." >&2
        exit 2
    fi
    BYPASS_REASON="$trimmed"
fi

if [ -z "$REPO" ]; then
    REPO="$(git rev-parse --show-toplevel 2>/dev/null || true)"
fi
if [ -z "$REPO" ] || ! git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
    echo "DEPLOY GATE FAILED ($CONTEXT): not a git repository: ${REPO:-$(pwd)}" >&2
    exit 1
fi

g() { git -C "$REPO" "$@"; }

FAILURES=()
fail() { FAILURES+=("$1"); }

SHA="$(g rev-parse --verify --quiet "${COMMIT}^{commit}" 2>/dev/null || true)"
SHORT="${SHA:0:12}"
[ -n "$SHA" ] || fail "commit '$COMMIT' does not exist in $REPO"

# 1. clean working tree
DIRTY="$(g status --porcelain 2>&1)" || fail "git status failed: $DIRTY"
if [ -n "$DIRTY" ]; then
    fail "working tree has uncommitted changes (git status --porcelain is not empty)"
fi

# 3a. refresh origin/main; a failed fetch is a failure, never a pass
FETCH_ERR=""
if ! FETCH_ERR="$(g fetch --quiet "$REMOTE" "+refs/heads/$BRANCH:refs/remotes/$REMOTE/$BRANCH" 2>&1)"; then
    fail "could not fetch $REMOTE/$BRANCH to verify the commit is pushed: $(printf '%s' "$FETCH_ERR" | tr '\n' ' ')"
fi

if [ -n "$SHA" ]; then
    # 2. contained in local main
    if ! g rev-parse --verify --quiet "refs/heads/$BRANCH" >/dev/null; then
        fail "local branch '$BRANCH' does not exist"
    elif ! g merge-base --is-ancestor "$SHA" "refs/heads/$BRANCH"; then
        fail "commit $SHORT is not on local '$BRANCH' (merge it into $BRANCH first)"
    fi
    # 3b. contained in origin/main
    if ! g rev-parse --verify --quiet "refs/remotes/$REMOTE/$BRANCH" >/dev/null; then
        fail "remote branch '$REMOTE/$BRANCH' is unknown"
    elif ! g merge-base --is-ancestor "$SHA" "refs/remotes/$REMOTE/$BRANCH"; then
        fail "commit $SHORT is not on '$REMOTE/$BRANCH' (git push $REMOTE $BRANCH first)"
    fi
fi

if [ "${#FAILURES[@]}" -eq 0 ]; then
    echo "deploy-gate ($CONTEXT): OK — tree clean, $SHORT is on $BRANCH and $REMOTE/$BRANCH."
    exit 0
fi

{
    echo "=================================================================="
    echo "DEPLOY GATE FAILED ($CONTEXT) — nothing was deployed or restarted."
    echo "Rule: no deploy without everything committed on main and pushed."
    echo "Repository: $REPO"
    echo "Commit:     ${SHORT:-$COMMIT}"
    for f in "${FAILURES[@]}"; do echo "  ✗ $f"; done
    if [ -n "$DIRTY" ]; then
        echo "Uncommitted files (git status --porcelain):"
        printf '%s\n' "$DIRTY" | sed 's/^/    /'
    fi
    echo "Fix: commit (and push) everything, or discard what is not meant to ship,"
    echo "then run the deploy again. See docs/deploy-gate.md."
    echo "=================================================================="
} >&2

if [ "$BYPASS_SET" != "1" ]; then
    exit 1
fi

# ── explicit, logged emergency bypass ─────────────────────────────────
json_str() {
    local v="$1"
    v="${v//\\/\\\\}"; v="${v//\"/\\\"}"; v="${v//$'\n'/\\n}"; v="${v//$'\t'/ }"; v="${v//$'\r'/ }"
    printf '"%s"' "$v"
}
if [ -z "$BYPASS_LOG" ]; then
    BYPASS_LOG="$(g rev-parse --absolute-git-dir)/openpoet-deploy-gate-bypass.log"
fi
mkdir -p "$(dirname "$BYPASS_LOG")"
failures_joined="$(printf '%s; ' "${FAILURES[@]}")"
dirty_joined="$(printf '%s' "$DIRTY" | tr '\n' ',')"
printf '{"time":%s,"context":%s,"user":%s,"session":%s,"repo":%s,"commit":%s,"reason":%s,"failures":%s,"dirty":%s}\n' \
    "$(json_str "$(date -u '+%Y-%m-%dT%H:%M:%SZ')")" "$(json_str "$CONTEXT")" \
    "$(json_str "$(id -un 2>/dev/null || echo unknown)")" "$(json_str "${OPENPOET_SESSION_ID:-}")" \
    "$(json_str "$REPO")" "$(json_str "${SHA:-$COMMIT}")" "$(json_str "$BYPASS_REASON")" \
    "$(json_str "${failures_joined%; }")" "$(json_str "${dirty_joined%,}")" >> "$BYPASS_LOG" || {
    echo "deploy-gate: could not write the bypass log $BYPASS_LOG — bypass refused." >&2
    exit 1
}
{
    echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
    echo "!! EMERGENCY BYPASS of the deploy gate ($CONTEXT)"
    echo "!! Reason: $BYPASS_REASON"
    echo "!! Logged to: $BYPASS_LOG"
    echo "!! Commit what was deployed and record the incident afterwards."
    echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
} >&2
echo "deploy-gate ($CONTEXT): BYPASSED"
exit 0
