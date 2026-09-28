package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// PlatformPayloadField describes one accepted payload field. Required mirrors
// the json tag: fields without omitempty must be present.
type PlatformPayloadField struct {
	Name        string                 `json:"name"`
	Type        string                 `json:"type"`
	Required    bool                   `json:"required"`
	Description string                 `json:"description,omitempty"`
	Fields      []PlatformPayloadField `json:"fields,omitempty"`
}

// PlatformPayloadSchema is the discoverable request contract of a capability:
// what the target must identify and which payload fields are accepted. Unknown
// payload fields are always rejected.
type PlatformPayloadSchema struct {
	Target  string                 `json:"target,omitempty"`
	Fields  []PlatformPayloadField `json:"fields"`
	Example json.RawMessage        `json:"example,omitempty"`
	Notes   string                 `json:"notes,omitempty"`
}

const projectTargetDescription = `{"type":"project","id":<project id>} or {"project_id":<project id>}`

const sessionTargetDescription = `{"type":"session","id":"<session id or unique prefix, e.g. the 8-char id>"}`

// withPayloadSchema attaches a schema derived from the payload struct the
// executor decodes into. A nil prototype declares an empty payload.
func withPayloadSchema(definition PlatformCapabilityDefinition, target string, prototype any, example, notes string) PlatformCapabilityDefinition {
	schema := &PlatformPayloadSchema{Target: target, Fields: []PlatformPayloadField{}, Notes: notes}
	if prototype != nil {
		schema.Fields = payloadFields(reflect.TypeOf(prototype))
	}
	if example != "" {
		schema.Example = json.RawMessage(example)
	}
	definition.Payload = schema
	return definition
}

func payloadFields(t reflect.Type) []PlatformPayloadField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return []PlatformPayloadField{}
	}
	fields := make([]PlatformPayloadField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, omitempty, ok := jsonFieldName(field)
		if !ok {
			continue
		}
		described := PlatformPayloadField{
			Name: name, Type: payloadTypeName(field.Type), Required: !omitempty && field.Type.Kind() != reflect.Pointer,
			Description: field.Tag.Get("doc"),
		}
		if element := structElement(field.Type); element != nil {
			described.Fields = payloadFields(element)
		}
		fields = append(fields, described)
	}
	return fields
}

func jsonFieldName(field reflect.StructField) (string, bool, bool) {
	if !field.IsExported() {
		return "", false, false
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	name, options, _ := strings.Cut(tag, ",")
	if name == "" {
		name = field.Name
	}
	return name, strings.Contains(","+options+",", ",omitempty,"), true
}

func structElement(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct && t.PkgPath() != "time" {
		return t
	}
	return nil
}

func payloadTypeName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "string"
		}
		return "array<" + payloadTypeName(t.Elem()) + ">"
	case reflect.Map:
		return "object<string," + payloadTypeName(t.Elem()) + ">"
	case reflect.Struct:
		if t.PkgPath() == "time" && t.Name() == "Time" {
			return "string(RFC3339)"
		}
		return "object"
	case reflect.Interface:
		return "any"
	}
	if t == reflect.TypeOf(json.RawMessage(nil)) {
		return "any"
	}
	return t.Kind().String()
}

// payloadFieldHints are capability-independent corrections for fields callers
// commonly put in the payload instead of the target.
var payloadFieldHints = map[string]string{
	"session_id": `identify the session in the target instead: ` + sessionTargetDescription,
}

// payloadDecodeFailure turns a JSON decode error into a message that names the
// failing field and lists what the payload accepts.
func payloadDecodeFailure(err error, output any, kind string, hints map[string]string) *PlatformDispatchError {
	accepted := acceptedFieldList(output)
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	var message string
	switch {
	case errors.As(err, &typeErr) && typeErr.Field != "":
		message = fmt.Sprintf("the %s payload field %q must be %s (got JSON %s)", kind, typeErr.Field, payloadTypeName(typeErr.Type), typeErr.Value)
	case errors.As(err, &typeErr):
		message = fmt.Sprintf("the %s payload must be a JSON %s (got JSON %s)", kind, payloadTypeName(typeErr.Type), typeErr.Value)
	case errors.As(err, &syntaxErr):
		message = fmt.Sprintf("the %s payload is not valid JSON", kind)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		message = fmt.Sprintf("the %s payload field %q is not accepted", kind, field)
		if hint := hints[field]; hint != "" {
			message += "; " + hint
		} else if hint := payloadFieldHints[field]; hint != "" {
			message += "; " + hint
		}
	default:
		message = fmt.Sprintf("the %s payload is invalid", kind)
	}
	if accepted == "" {
		message += "; this capability accepts no payload fields"
	} else {
		message += "; accepted fields: " + accepted
	}
	return platformFailure("platform_payload_invalid", message, false)
}

func acceptedFieldList(output any) string {
	if output == nil {
		return ""
	}
	fields := payloadFields(reflect.TypeOf(output))
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		label := field.Name + " (" + field.Type
		if field.Required {
			label += ", required"
		}
		parts = append(parts, label+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// missingPayloadField reports a required field the caller did not send.
func missingPayloadField(field, expected string) *PlatformDispatchError {
	return platformFailure("platform_payload_invalid", fmt.Sprintf("the payload field %q is required: %s", field, expected), false)
}

// unexpectedPayloadFields rejects a payload sent to a capability that takes
// none, naming the offending fields.
func unexpectedPayloadFields(payload map[string]json.RawMessage) *PlatformDispatchError {
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, fmt.Sprintf("%q", name))
	}
	sort.Strings(names)
	message := "this capability accepts no payload fields (send {}); unexpected: " + strings.Join(names, ", ")
	for _, name := range names {
		if hint := payloadFieldHints[strings.Trim(name, `"`)]; hint != "" {
			message += "; " + hint
		}
	}
	return platformFailure("platform_payload_invalid", message, false)
}
