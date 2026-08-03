#!/usr/bin/env bash
# E2E validation of automatic worktree isolation, against a REAL server, REAL git
# repositories and a FRESH database.
#
# Guardrails: never port 8080/8081/8090 (dev + production + the user's apps), never
# the production DB, never a real registered project — a fresh DB is born with zero
# projects, which is the structural isolation.
#
# Cost: $0. Sessions are minted through the OPENPOET_TEST_MODE-only endpoint (no
# PTY, no agent, no LLM) and driven with synthetic hook events.
set -uo pipefail

PORT="${PORT:-8793}"
SANDBOX="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.sandbox"
BASE="http://127.0.0.1:${PORT}"
BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/build/openpoet"
JAR="${SANDBOX}/cookies.txt"
PASS=0; FAIL=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want=$3 got=$2)"; fi; }

for p in 8080 8081 8090; do
  if [ "$PORT" = "$p" ]; then echo "REFUSING to use protected port $p"; exit 1; fi
done

cleanup() { [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null; }
trap cleanup EXIT

say "Sandbox setup (port ${PORT})"
rm -rf "$SANDBOX"; mkdir -p "$SANDBOX"
mkrepo() {
  local dir="$SANDBOX/$1"; mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.email t@t; git -C "$dir" config user.name t
  printf 'package a\n' > "$dir/shared.go"
  printf 'package b\n' > "$dir/other.go"
  git -C "$dir" add .; git -C "$dir" commit -qm init
  echo "$dir"
}
REPO_A="$(mkrepo proj-a)"
REPO_B="$(mkrepo proj-b)"
ok "two real git repos created"

OPENPOET_TEST_MODE=1 "$BIN" -port "$PORT" -bind 127.0.0.1 -db "$SANDBOX/op.db" \
  >"$SANDBOX/server.log" 2>&1 &
SRV_PID=$!
for _ in $(seq 1 50); do
  curl -sf -o /dev/null "$BASE/" && break
  sleep 0.2
done
if ! curl -sf -o /dev/null "$BASE/"; then bad "server did not start"; tail -20 "$SANDBOX/server.log"; exit 1; fi
ok "server up on ${PORT} with a fresh DB"

# API mutations need a credential; the UI cookie is the cheapest one.
curl -s -c "$JAR" -H "Accept: text/html" -o /dev/null "$BASE/"
api() { # api METHOD PATH [BODY]
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -s -b "$JAR" -X "$m" -H 'Content-Type: application/json' -d "$b" "$BASE$p"
  else
    curl -s -b "$JAR" -X "$m" "$BASE$p"
  fi
}
api_code() { # like api but prints the HTTP status
  local m="$1" p="$2" b="${3:-}"
  curl -s -o "$SANDBOX/body.json" -w '%{http_code}' -b "$JAR" -X "$m" \
    -H 'Content-Type: application/json' ${b:+-d "$b"} "$BASE$p"
}

say "Register test projects (conflict gate ON)"
PA=$(api POST /api/projects "{\"name\":\"proj-a\",\"path\":\"$REPO_A\",\"type\":\"local\",\"backend\":\"claude_code\",\"conflict_policy\":\"gate\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')
PB=$(api POST /api/projects "{\"name\":\"proj-b\",\"path\":\"$REPO_B\",\"type\":\"local\",\"backend\":\"claude_code\",\"conflict_policy\":\"gate\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')
if [ -n "$PA" ] && [ -n "$PB" ]; then ok "projects registered (A=$PA B=$PB)"; else bad "project registration failed"; exit 1; fi

worktrees() { git -C "$1" worktree list --porcelain | grep -c '^worktree '; }

say "S1  isolation is really plumbed through REST (not silently dropped)"
CODE=$(api_code POST /api/sessions "{\"project_id\":$PA,\"isolation\":\"bogus\"}")
BODY=$(cat "$SANDBOX/body.json")
check "unknown isolation mode is rejected" "$CODE" "400"
if grep -q invalid_isolation <<<"$BODY"; then ok "typed invalid_isolation reached the caller"; else bad "expected invalid_isolation, got: $BODY"; fi

say 'S2  isolation:"always" provisions a lane on demand'
BEFORE=$(worktrees "$REPO_A")
api POST /api/sessions "{\"project_id\":$PA,\"isolation\":\"always\"}" >"$SANDBOX/always.json" 2>&1
AFTER=$(worktrees "$REPO_A")
check "a new git worktree appeared" "$AFTER" "$((BEFORE+1))"
LANE_DIR=$(git -C "$REPO_A" worktree list --porcelain | awk '/^worktree /{print $2}' | grep worktrees | head -1)
if [ -n "$LANE_DIR" ] && [ -d "$LANE_DIR" ]; then ok "lane exists on disk: ${LANE_DIR##*/}"; else bad "lane directory missing"; fi
LANE_BRANCH=$(git -C "$LANE_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null)
case "$LANE_BRANCH" in openpoet/auto-*) ok "lane has its own auto-named branch ($LANE_BRANCH)";; *) bad "unexpected lane branch: $LANE_BRANCH";; esac
if grep -q '/.openpoet/' "$REPO_A/.git/info/exclude" 2>/dev/null; then ok "main checkout stays clean (.git/info/exclude)"; else bad "exclude line missing"; fi
if [ -z "$(git -C "$REPO_A" status --porcelain)" ]; then ok "git status of the main checkout is clean"; else bad "lane dirtied the main checkout"; fi

say 'S3  isolation:"auto" uses the main checkout while it is free'
BEFORE=$(worktrees "$REPO_B")
api POST /api/sessions "{\"project_id\":$PB,\"isolation\":\"auto\"}" >/dev/null 2>&1
check "no lane provisioned on a free project" "$(worktrees "$REPO_B")" "$BEFORE"

# --- synthetic sessions: $0, no PTY, no agent -------------------------------
# The real lane row, so a synthetic lane session carries a valid workspace_id
# (the column is a foreign key) exactly like a real one.
LANE_ID=$(sqlite3 "$SANDBOX/op.db" "SELECT id FROM workspaces WHERE project_id=$PA ORDER BY created_at LIMIT 1")

mint() { # mint PROJECT_ID [WORKSPACE_DIR]
  local pid="$1" wd="${2:-}" body
  if [ -n "$wd" ]; then body="{\"project_id\":$pid,\"workspace_id\":\"$LANE_ID\",\"work_dir\":\"$wd\"}"
  else body="{\"project_id\":$pid}"; fi
  api POST /api/test/sessions "$body"
}
field() { python3 -c "import json,sys; print(json.load(sys.stdin).get('$1',''))"; }

gate() { # gate SESSION_ID HOOK_TOKEN FILE_PATH  → prints permissionDecision
  curl -s -X POST "$BASE/api/hooks/pretooluse" \
    -H 'Content-Type: application/json' -H "X-Session-ID: $1" -H "X-Hook-Token: $2" \
    -d "{\"session_id\":\"$1\",\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Write\",\"tool_input\":{\"file_path\":\"$3\"}}" \
    | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("PARSE_ERROR"); raise SystemExit
h=d.get("hookSpecificOutput") or {}
print(h.get("permissionDecision") or d.get("decision") or "allow")'
}
event() { # event SESSION_ID HOOK_TOKEN FILE_PATH — feeds the async indexer
  curl -s -o /dev/null -X POST "$BASE/api/hooks/event" \
    -H 'Content-Type: application/json' -H "X-Session-ID: $1" -H "X-Hook-Token: $2" \
    -d "{\"session_id\":\"$1\",\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Write\",\"tool_input\":{\"file_path\":\"$3\"}}"
}

say "S4  same working tree still collides (the hazard that must stay blocked)"
S1J=$(mint "$PA"); S1=$(field session_id <<<"$S1J"); T1=$(field hook_token <<<"$S1J")
S2J=$(mint "$PA"); S2=$(field session_id <<<"$S2J"); T2=$(field hook_token <<<"$S2J")
if [ -n "$S1" ] && [ -n "$S2" ]; then ok "two synthetic main-tree sessions minted"; else bad "mint failed: $S1J"; exit 1; fi
check "first writer allowed" "$(gate "$S1" "$T1" "$REPO_A/shared.go")" "allow"
check "second writer of the SAME file in the SAME tree denied" "$(gate "$S2" "$T2" "$REPO_A/shared.go")" "deny"
check "a different file is not blocked" "$(gate "$S2" "$T2" "$REPO_A/other.go")" "allow"

say "S5  a lane diverges instead of colliding (the fix that makes isolation work)"
S3J=$(mint "$PA" "$LANE_DIR"); S3=$(field session_id <<<"$S3J"); T3=$(field hook_token <<<"$S3J")
if [ -n "$S3" ]; then ok "synthetic LANE session minted (work_dir=${LANE_DIR##*/})"; else bad "lane mint failed: $S3J"; exit 1; fi
check "same logical file in ANOTHER tree is allowed" "$(gate "$S3" "$T3" "$LANE_DIR/shared.go")" "allow"
check "…also when the tool reports a relative path" "$(gate "$S3" "$T3" "shared.go")" "allow"

say "S6  divergence is reported as merge risk, not filed as a conflict"
event "$S1" "$T1" "$REPO_A/shared.go"
event "$S3" "$T3" "$LANE_DIR/shared.go"
sleep 3   # the flusher batches every 2s
DIV=$(sqlite3 "$SANDBOX/op.db" "SELECT COUNT(*) FROM event_outbox WHERE event_type='conflict.divergence'")
check "conflict.divergence event persisted" "$([ "$DIV" -ge 1 ] && echo yes || echo no)" "yes"
PAYLOAD=$(sqlite3 "$SANDBOX/op.db" "SELECT payload_json FROM event_outbox WHERE event_type='conflict.divergence' LIMIT 1")
if grep -q '"path":"shared.go"' <<<"$PAYLOAD" && grep -q 'main' <<<"$PAYLOAD"; then
  ok "payload names the logical path and both trees"
else bad "unexpected divergence payload: $PAYLOAD"; fi
INC=$(sqlite3 "$SANDBOX/op.db" "SELECT COUNT(*) FROM coordinator_incidents WHERE rule='file_overlap' AND details_json LIKE '%ws-synth%'")
check "cross-tree overlap opened NO incident" "$INC" "0"

say "S7  same-tree collision IS filed as a critical incident"
event "$S2" "$T2" "$REPO_A/shared.go"
sleep 3
SAME=$(sqlite3 "$SANDBOX/op.db" "SELECT COUNT(*) FROM coordinator_incidents WHERE rule='file_overlap' AND severity='critical'")
check "critical file_overlap incident recorded" "$([ "$SAME" -ge 1 ] && echo yes || echo no)" "yes"

say "S8  substrate protection survived the refactor"
check ".openpoet metadata write denied" "$(gate "$S1" "$T1" "$REPO_A/.openpoet/environment.yaml")" "deny"

say "S9  the gate stays opt-in per project"
api PUT "/api/projects/$PB" "{\"name\":\"proj-b\",\"path\":\"$REPO_B\",\"type\":\"local\",\"backend\":\"claude_code\",\"conflict_policy\":\"observe\"}" >/dev/null
B1J=$(mint "$PB"); B1=$(field session_id <<<"$B1J"); BT1=$(field hook_token <<<"$B1J")
B2J=$(mint "$PB"); B2=$(field session_id <<<"$B2J"); BT2=$(field hook_token <<<"$B2J")
gate "$B1" "$BT1" "$REPO_B/shared.go" >/dev/null
check "observe-mode project never blocks" "$(gate "$B2" "$BT2" "$REPO_B/shared.go")" "allow"

say "S10 an unauthenticated gate call is refused"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/hooks/pretooluse" \
  -H 'Content-Type: application/json' -H "X-Session-ID: $S1" \
  -d '{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"x"}}')
check "missing hook token rejected" "$CODE" "401"

say "Result"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"
if grep -qiE 'panic|data race' "$SANDBOX/server.log"; then bad "server log contains a panic or race"; FAIL=$((FAIL+1)); fi
[ "$FAIL" -eq 0 ] || { echo; echo "--- server log tail ---"; tail -30 "$SANDBOX/server.log"; }
exit $((FAIL > 0))
