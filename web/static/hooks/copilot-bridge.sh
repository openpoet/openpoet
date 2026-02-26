#!/bin/bash
# OpenPoet Hook Bridge Script for GitHub Copilot CLI
# Receives Copilot hook event JSON on stdin, POSTs to OpenPoet API,
# and translates the response back to Copilot's expected format.
# Env: OPENPOET_HOOK_URL (e.g., http://localhost:8080)
#      OPENPOET_SESSION_ID (set by OpenPoet when starting session)

HOOK_URL="${OPENPOET_HOOK_URL:-http://localhost:8080}"
SESSION_ID="${OPENPOET_SESSION_ID}"
INPUT=$(cat)

# Extract event name from hook JSON
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

# translate_response converts OpenPoet's Claude Code format response to Copilot's format.
# OpenPoet returns: {"hookSpecificOutput":{"decision":{"behavior":"allow|deny","message":"..."}}}
# Copilot expects: {"hookSpecificOutput":{"hookEventName":"preToolUse","decision":{"behavior":"approve|deny"}}}
translate_response() {
    local RESPONSE="$1"
    if [ -z "$RESPONSE" ]; then
        return
    fi
    if command -v jq &>/dev/null; then
        # Translate behavior: "allow" -> "approve" for Copilot
        echo "$RESPONSE" | jq -c '.hookSpecificOutput.hookEventName = "preToolUse" |
            if .hookSpecificOutput.decision.behavior == "allow" then .hookSpecificOutput.decision.behavior = "approve" else . end' 2>/dev/null
    else
        # Fallback: simple string replacement
        echo "$RESPONSE" | sed 's/"behavior":"allow"/"behavior":"approve"/g' | sed 's/"hookEventName":"PermissionRequest"/"hookEventName":"preToolUse"/g'
    fi
}

case "$EVENT" in
    preToolUse)
        # Blocking: POST and wait for user decision in the browser UI
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
    postToolUse)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
    userPromptSubmitted)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
    sessionStart|sessionEnd)
        curl -s -X POST "${HOOK_URL}/api/hooks/event" \
            -H "Content-Type: application/json" \
            -H "X-Session-ID: ${SESSION_ID}" \
            -H "X-Backend: copilot" \
            -d "$INPUT" > /dev/null 2>&1 &
        ;;
esac

exit 0
