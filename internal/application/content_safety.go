package application

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxApplicationTitleRunes   = 200
	maxApplicationSummaryRunes = 4 << 10
	maxApplicationPromptRunes  = 16 << 10
	maxApplicationContentRunes = 256 << 10
	maxApplicationOutputRunes  = 32 << 10
	maxApplicationJSONBytes    = 64 << 10
)

func boundedRedactedInput(value string, limit int, field string, required bool) (string, error) {
	value = strings.TrimSpace(redactReportSecrets(value))
	if required && value == "" {
		return "", validationError("content_required", field+" is required")
	}
	if utf8.RuneCountInString(value) > limit {
		return "", validationError("content_too_large", field+" exceeds its bounded limit")
	}
	return value, nil
}

func boundedRedactedOutput(value string, limit int) (text string, truncated bool) {
	value = strings.TrimSpace(redactReportSecrets(value))
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	return string(runes[:limit]) + "\n...[TRUNCATED]", true
}

func boundedRedactedStrings(values []string, maxItems, perItemLimit int) ([]string, bool, error) {
	truncated := false
	if len(values) > maxItems {
		values = values[:maxItems]
		truncated = true
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized, itemTruncated := boundedRedactedOutput(value, perItemLimit)
		if normalized == "" {
			continue
		}
		truncated = truncated || itemTruncated
		result = append(result, normalized)
	}
	return result, truncated, nil
}

func sanitizeBoundedJSONMap(input map[string]any) (map[string]any, error) {
	if input == nil {
		return map[string]any{}, nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, validationError("invalid_json_payload", "Payload must be JSON serializable")
	}
	if len(encoded) > maxApplicationJSONBytes {
		return nil, validationError("json_payload_too_large", "JSON payload exceeds its bounded limit")
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, validationError("invalid_json_payload", "Payload must use JSON-compatible values")
	}
	return sanitizeJSONMap(normalized, 0)
}

func sanitizeJSONMap(input map[string]any, depth int) (map[string]any, error) {
	if depth > 12 {
		return nil, validationError("json_payload_too_deep", "JSON payload exceeds its nesting limit")
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		if isSensitivePayloadKey(key) {
			result[key] = "[REDACTED]"
			continue
		}
		sanitized, err := sanitizeJSONValue(value, depth+1)
		if err != nil {
			return nil, err
		}
		result[key] = sanitized
	}
	return result, nil
}

func sanitizeJSONValue(value any, depth int) (any, error) {
	if depth > 12 {
		return nil, validationError("json_payload_too_deep", "JSON payload exceeds its nesting limit")
	}
	switch typed := value.(type) {
	case string:
		text, _ := boundedRedactedOutput(typed, maxApplicationPromptRunes)
		return text, nil
	case map[string]any:
		return sanitizeJSONMap(typed, depth)
	case []any:
		if len(typed) > 1000 {
			return nil, validationError("json_payload_too_large", "JSON array exceeds its item limit")
		}
		result := make([]any, len(typed))
		for i, item := range typed {
			sanitized, err := sanitizeJSONValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = sanitized
		}
		return result, nil
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return typed, nil
	default:
		return nil, validationError("invalid_json_payload", "Payload contains a non-JSON value")
	}
}

func isSensitivePayloadKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"), " ", "_"))
	for _, marker := range []string{"password", "passwd", "secret", "api_key", "apikey", "access_token", "refresh_token", "authorization", "credential", "private_key"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

type safeOperationError struct {
	message string
	cause   error
}

func (e *safeOperationError) Error() string { return e.message }
func (e *safeOperationError) Unwrap() error { return e.cause }

func safeBackendError(message string, cause error) error {
	if cause == nil {
		return errors.New(message)
	}
	return &safeOperationError{message: message, cause: cause}
}

func validateBoundedID(id int64, name string) error {
	if id <= 0 {
		return validationError("invalid_id", fmt.Sprintf("%s ID must be positive", name))
	}
	return nil
}
