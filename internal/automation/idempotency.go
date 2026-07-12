package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openpoet/internal/database"
)

const (
	defaultIdempotencyTTL    = 24 * time.Hour
	defaultMaxCachedResponse = 1 << 20
	maxIdempotencyKeyLength  = 200
)

type IdempotencyStore interface {
	ClaimAutomationCommand(ctx context.Context, command *database.AutomationCommand) (*database.AutomationCommand, bool, error)
	CompleteAutomationCommandWithEvent(ctx context.Context, id, status string, responseStatus int, contentType string, body []byte, errorCode string, event *database.EventOutboxAppend) error
}

type IdempotencyOptions struct {
	TTL               time.Duration
	MaxCachedResponse int
	Random            io.Reader
	Now               func() time.Time
	IsMutation        func(string) bool
}

type Idempotency struct {
	store             IdempotencyStore
	ttl               time.Duration
	maxCachedResponse int
	random            io.Reader
	now               func() time.Time
	isMutation        func(string) bool
}

type idempotencyKeyContextKey struct{}

const automationCommandSucceededEventType = "automation.command_succeeded"

var automationAuditValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

type automationCommandAuditEnvelope struct {
	CommandID     string        `json:"command_id"`
	Capability    string        `json:"capability"`
	Target        commandTarget `json:"target"`
	DryRun        bool          `json:"dry_run,omitempty"`
	CorrelationID string        `json:"correlation_id,omitempty"`
}

type automationCommandAuditTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type automationCommandSuccessPayload struct {
	CommandID  string                       `json:"command_id"`
	Capability string                       `json:"capability"`
	Target     automationCommandAuditTarget `json:"target"`
	Status     string                       `json:"status"`
}

func idempotencyKeyFromContext(ctx context.Context) string {
	value, _ := ctx.Value(idempotencyKeyContextKey{}).(string)
	return value
}

func NewIdempotency(store IdempotencyStore, options IdempotencyOptions) *Idempotency {
	if options.TTL <= 0 {
		options.TTL = defaultIdempotencyTTL
	}
	if options.MaxCachedResponse <= 0 {
		options.MaxCachedResponse = defaultMaxCachedResponse
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.IsMutation == nil {
		options.IsMutation = func(string) bool { return false }
	}
	return &Idempotency{
		store:             store,
		ttl:               options.TTL,
		maxCachedResponse: options.MaxCachedResponse,
		random:            options.Random,
		now:               options.Now,
		isMutation:        options.IsMutation,
	}
}

func (i *Idempotency) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReadMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		// Event ACK is already idempotent and monotonic in the V50 cursor
		// transaction. It deliberately does not consume the command ledger,
		// whose keys and replay payloads are reserved for command envelopes.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events/ack") {
			next.ServeHTTP(w, r)
			return
		}
		actor, ok := ActorFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "automation actor is missing", false)
			return
		}
		if i == nil || i.store == nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency storage is unavailable", true)
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		bodyKey, bodyKeyErr := requestBodyIdempotencyKey(r)
		if bodyKeyErr != nil && key == "" {
			var tooLarge *http.MaxBytesError
			if errors.As(bodyKeyErr, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "automation request body is too large", false)
				return
			}
			writeError(w, http.StatusBadRequest, "request_body_invalid", "automation request body could not be decoded", false)
			return
		}
		if key == "" {
			key = strings.TrimSpace(bodyKey)
		} else if bodyKey != "" && bodyKey != key {
			writeError(w, http.StatusBadRequest, "idempotency_key_conflict", "Idempotency-Key does not match idempotency_key in the command envelope", false)
			return
		}
		if key == "" {
			writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required for automation writes", false)
			return
		}
		if len(key) > maxIdempotencyKeyLength {
			writeError(w, http.StatusBadRequest, "idempotency_key_invalid", "Idempotency-Key is too long", false)
			return
		}

		fingerprint, err := requestFingerprint(r)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "automation request body is too large", false)
				return
			}
			writeError(w, http.StatusBadRequest, "request_body_invalid", "automation request body could not be read", false)
			return
		}
		id, err := randomHex(i.random, 16)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency command ID could not be generated", true)
			return
		}
		expiresAt := i.now().Add(i.ttl)
		claimed, created, err := i.store.ClaimAutomationCommand(r.Context(), &database.AutomationCommand{
			ID:                 id,
			ClientID:           actor.ClientID,
			IdempotencyKey:     key,
			RequestFingerprint: fingerprint,
			Operation:          r.Method + " " + r.URL.Path,
			ExpiresAt:          sql.NullTime{Time: expiresAt, Valid: true},
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency claim could not be persisted", true)
			return
		}
		if claimed.RequestFingerprint != fingerprint {
			writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for a different request", false)
			return
		}
		if !created {
			i.replay(w, claimed)
			return
		}
		successEvent := requestAutomationCommandSuccessEvent(r, actor, claimed.ID, i.now().UTC(), i.isMutation)

		capture := newBufferedResponse(i.maxCachedResponse)
		r = r.WithContext(context.WithValue(r.Context(), idempotencyKeyContextKey{}, key))
		next.ServeHTTP(capture, r)
		completionContext := context.WithoutCancel(r.Context())
		if capture.overflow {
			if err := i.store.CompleteAutomationCommandWithEvent(completionContext, claimed.ID, "failed",
				http.StatusInternalServerError, "application/json", nil, "response_too_large", nil); err != nil {
				writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency result could not be persisted", true)
				return
			}
			writeError(w, http.StatusInternalServerError, "response_too_large", "automation response exceeded the idempotency cache limit", false)
			return
		}

		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}
		commandStatus := "succeeded"
		if status >= 400 {
			commandStatus = "failed"
		}
		if commandStatus != "succeeded" {
			successEvent = nil
		}
		contentType := capture.header.Get("Content-Type")
		if err := i.store.CompleteAutomationCommandWithEvent(completionContext, claimed.ID, commandStatus,
			status, contentType, capture.body.Bytes(), "", successEvent); err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency result could not be persisted", true)
			return
		}
		copyHeaders(w.Header(), capture.header)
		w.WriteHeader(status)
		_, _ = w.Write(capture.body.Bytes())
	})
}

func requestAutomationCommandSuccessEvent(r *http.Request, actor Actor, ledgerID string, occurredAt time.Time, isMutation func(string) bool) *database.EventOutboxAppend {
	if r == nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/commands") || r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var envelope automationCommandAuditEnvelope
	if json.Unmarshal(body, &envelope) != nil || envelope.DryRun || isMutation == nil || !isMutation(strings.TrimSpace(envelope.Capability)) {
		return nil
	}
	commandID := automationAuditValue(envelope.CommandID, "command", true)
	capability := automationAuditValue(envelope.Capability, "capability", false)
	if commandID == "" || capability == "" {
		return nil
	}
	target := automationAuditTarget(envelope.Target, commandID)
	payload, err := json.Marshal(automationCommandSuccessPayload{
		CommandID: commandID, Capability: capability, Target: target, Status: "succeeded",
	})
	if err != nil || len(payload) > 2048 {
		return nil
	}
	actorType := automationAuditValue(actor.Type, "actor", true)
	actorID := actor.ClientID
	if strings.TrimSpace(actorID) == "" {
		actorID = actor.ID
	}
	actorID = automationAuditValue(actorID, "actor", true)
	if actorType == "" || actorID == "" {
		return nil
	}
	correlationID := strings.TrimSpace(envelope.CorrelationID)
	if !automationAuditValuePattern.MatchString(correlationID) {
		correlationID = ""
	}
	return &database.EventOutboxAppend{
		EventID:       "automation-command-" + automationAuditDigest(ledgerID),
		EventType:     automationCommandSucceededEventType,
		AggregateType: target.Type,
		AggregateID:   target.ID,
		Actor:         actorType + ":" + actorID,
		CorrelationID: correlationID,
		SchemaVersion: 1,
		PayloadJSON:   string(payload),
		OccurredAt:    occurredAt,
	}
}

func automationAuditTarget(target commandTarget, commandID string) automationCommandAuditTarget {
	targetType := strings.TrimSpace(target.Type)
	if targetType == "" {
		targetType = strings.TrimSpace(target.Kind)
	}
	if !platformIdentifierPattern.MatchString(targetType) {
		targetType = ""
	}
	targetID := automationAuditRawID(target.ID)
	if targetType != "" && targetID == "" {
		switch targetType {
		case "project":
			if target.ProjectID > 0 {
				targetID = fmt.Sprintf("%d", target.ProjectID)
			}
		case "task", "project_task":
			if target.TaskID > 0 {
				targetID = fmt.Sprintf("%d", target.TaskID)
			}
		case "session":
			if automationAuditValuePattern.MatchString(strings.TrimSpace(target.SessionID)) {
				targetID = strings.TrimSpace(target.SessionID)
			}
		}
	}
	if targetType == "" || targetID == "" {
		return automationCommandAuditTarget{Type: "automation_command", ID: commandID}
	}
	return automationCommandAuditTarget{Type: targetType, ID: targetID}
}

func automationAuditRawID(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	var normalized string
	switch typed := value.(type) {
	case string:
		normalized = strings.TrimSpace(typed)
	case json.Number:
		normalized = typed.String()
	default:
		return ""
	}
	if !automationAuditValuePattern.MatchString(normalized) {
		return ""
	}
	return normalized
}

func automationAuditValue(value, prefix string, digestUnsafe bool) string {
	value = strings.TrimSpace(value)
	if automationAuditValuePattern.MatchString(value) {
		return value
	}
	if !digestUnsafe || value == "" || utf8.RuneCountInString(value) > maxCommandFieldLength || hasAutomationAuditControl(value) {
		return ""
	}
	return prefix + "-" + automationAuditDigest(value)
}

func automationAuditDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func hasAutomationAuditControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func requestBodyIdempotencyKey(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(bytes.TrimSpace(body)) == 0 {
		return "", io.EOF
	}
	var envelope struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", err
	}
	return strings.TrimSpace(envelope.IdempotencyKey), nil
}

func (i *Idempotency) replay(w http.ResponseWriter, command *database.AutomationCommand) {
	switch command.Status {
	case "succeeded", "failed":
		if command.ResponseContentType != "" {
			w.Header().Set("Content-Type", command.ResponseContentType)
		}
		w.Header().Set("Idempotency-Replayed", "true")
		status := command.ResponseStatus
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_, _ = w.Write(command.ResponseBody)
	case "processing":
		writeError(w, http.StatusConflict, "idempotency_in_progress", "the idempotent command is still processing", true)
	case "indeterminate":
		writeError(w, http.StatusConflict, "idempotency_indeterminate", "the previous command outcome is indeterminate and will not be repeated", false)
	default:
		writeError(w, http.StatusServiceUnavailable, "idempotency_invalid", "the stored idempotency state is invalid", false)
	}
}

func requestFingerprint(r *http.Request) (string, error) {
	var body []byte
	var err error
	if r.Body != nil {
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return "", err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	bodyHash := sha256.Sum256(body)
	value := fmt.Sprintf("%s\n%s\n%s", r.Method, r.URL.RequestURI(), hex.EncodeToString(bodyHash[:]))
	fingerprint := sha256.Sum256([]byte(value))
	return hex.EncodeToString(fingerprint[:]), nil
}

func randomHex(reader io.Reader, size int) (string, error) {
	value, err := randomBytes(reader, size)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

type bufferedResponse struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	maxBytes int
	overflow bool
}

func newBufferedResponse(maxBytes int) *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), maxBytes: maxBytes}
}

func (w *bufferedResponse) Header() http.Header {
	return w.header
}

func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponse) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(value) > w.maxBytes {
		w.overflow = true
		return len(value), nil
	}
	return w.body.Write(value)
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
