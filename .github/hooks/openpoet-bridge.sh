#!/bin/bash
# OpenPoet Hook Bridge Script for GitHub Copilot CLI

HOOK_URL="${OPENPOET_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${OPENPOET_SESSION_ID}"
INPUT=$(cat)

EVENT=""
if command -v jq &>/dev/null; then
    EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null)
fi
if [ -z "$EVENT" ]; then
    EVENT=$(echo "$INPUT" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
fi

if [ -z "$EVENT" ] || [ -z "$SESSION_ID" ]; then
    exit 0
fi

translate_response() {
    local RESPONSE="$1"
    if [ -z "$RESPONSE" ]; then return; fi
    if command -v jq &>/dev/null; then
        echo "$RESPONSE" | jq -c '.hookSpecificOutput.hookEventName = "preToolUse" |
            if .hookSpecificOutput.decision.behavior == "allow" then .hookSpecificOutput.decision.behavior = "approve" else . end' 2>/dev/null
    else
        echo "$RESPONSE" | sed 's/"behavior":"allow"/"behavior":"approve"/g' | sed 's/"hookEventName":"PermissionRequest"/"hookEventName":"preToolUse"/g'
    fi
}

case "$EVENT" in
    preToolUse)
        RESPONSE=$(curl -s --max-time 590 -X POST \
            "${HOOK_URL}/api/hooks/permission" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESPONSE" ]; then
            translate_response "$RESPONSE"
        fi
        ;;
    postToolUse|userPromptSubmitted|sessionStart|sessionEnd)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
