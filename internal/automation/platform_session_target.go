package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// minSessionIDPrefix keeps prefix lookups meaningful; MCP tools print 8 chars.
const minSessionIDPrefix = 6

// SessionIDResolver returns up to limit session ids equal to value or, when
// none is equal, starting with it.
type SessionIDResolver interface {
	MatchSessionIDs(ctx context.Context, value string, limit int) ([]string, error)
}

// canonicalizeSessionTarget normalizes every accepted spelling of a session
// target to {"type":"session","id":"<full id>"}: session_id is folded into id
// and a unique id prefix is expanded. Non-session targets pass through.
func canonicalizeSessionTarget(ctx context.Context, resolver SessionIDResolver, target json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(target))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return target, nil
	}
	kind := ""
	for _, key := range []string{"type", "kind"} {
		if value, ok := fields[key].(string); ok && strings.TrimSpace(value) != "" {
			kind = strings.ToLower(strings.TrimSpace(value))
			break
		}
	}
	rawSessionID, hasSessionID := fields["session_id"]
	if kind != "session" && !(kind == "" && hasSessionID) {
		return target, nil
	}
	id := ""
	if raw, ok := fields["id"]; ok {
		value, isString := raw.(string)
		if !isString {
			return nil, platformFailure("platform_target_invalid", "target id must be a string session id: "+sessionTargetDescription, false)
		}
		id = strings.TrimSpace(value)
	}
	if hasSessionID {
		value, isString := rawSessionID.(string)
		if !isString {
			return nil, platformFailure("platform_target_invalid", "target session_id must be a string", false)
		}
		value = strings.TrimSpace(value)
		if id != "" && value != "" && id != value {
			return nil, platformFailure("platform_target_invalid", "target id and session_id name different sessions", false)
		}
		if id == "" {
			id = value
		}
		delete(fields, "session_id")
	}
	if id == "" {
		return nil, platformFailure("platform_target_invalid", "session id is required: "+sessionTargetDescription, false)
	}
	if _, ok := fields["type"]; !ok {
		if _, ok := fields["kind"]; !ok {
			fields["type"] = "session"
		}
	}
	resolved, err := resolveSessionID(ctx, resolver, id)
	if err != nil {
		return nil, err
	}
	fields["id"] = resolved
	normalized, err := json.Marshal(fields)
	if err != nil {
		return nil, platformFailure("platform_target_invalid", "the session target could not be encoded", false)
	}
	return normalized, nil
}

func resolveSessionID(ctx context.Context, resolver SessionIDResolver, id string) (string, error) {
	if resolver == nil || len(id) > maxExecutionIDRunes {
		return id, nil
	}
	matches, err := resolver.MatchSessionIDs(ctx, id, 2)
	if err != nil {
		return "", err
	}
	for _, match := range matches {
		if match == id {
			return id, nil
		}
	}
	switch {
	case len(matches) == 0:
		// Unknown ids keep flowing so the owning service reports not-found in
		// its usual shape.
		return id, nil
	case len(id) < minSessionIDPrefix:
		return "", platformFailure("platform_target_invalid", fmt.Sprintf("session id prefix %q is too short; send at least %d characters or the full id", id, minSessionIDPrefix), false)
	case len(matches) > 1:
		return "", platformFailure("platform_target_ambiguous", fmt.Sprintf("session id prefix %q matches more than one session; send more characters or the full id", id), false)
	}
	return matches[0], nil
}
