#!/bin/bash
# DevManager Hook Bridge Script
# Receives hook event JSON on stdin, POSTs to DevManager API, returns response on stdout.
# Env: DEVMANAGER_HOOK_URL (e.g., http://localhost:8080)
#      DEVMANAGER_SESSION_ID (set by DevManager when starting session)

HOOK_URL="${DEVMANAGER_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${DEVMANAGER_SESSION_ID}"
INPUT=$(cat)

# Extract event name using basic string parsing (no jq dependency)
EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    # Fallback: extract hook_event_name without jq
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

case "$EVENT" in
    PermissionRequest)
        # Blocking: POST and wait for user decision in the browser UI
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            echo "$RESPONSE"
        fi
        ;;
    PreToolUse)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
    PostToolUse|PostToolUseFailure)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
    Notification)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
    Stop|UserPromptSubmit)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
