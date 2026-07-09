package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openpoet/internal/database"
)

const (
	defaultIdempotencyTTL    = 24 * time.Hour
	defaultMaxCachedResponse = 1 << 20
	maxIdempotencyKeyLength  = 200
)

type IdempotencyStore interface {
	ClaimAutomationCommand(ctx context.Context, command *database.AutomationCommand) (*database.AutomationCommand, bool, error)
	CompleteAutomationCommand(ctx context.Context, id, status string, responseStatus int, contentType string, body []byte, errorCode string) error
}

type IdempotencyOptions struct {
	TTL               time.Duration
	MaxCachedResponse int
	Random            io.Reader
	Now               func() time.Time
}

type Idempotency struct {
	store             IdempotencyStore
	ttl               time.Duration
	maxCachedResponse int
	random            io.Reader
	now               func() time.Time
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
	return &Idempotency{
		store:             store,
		ttl:               options.TTL,
		maxCachedResponse: options.MaxCachedResponse,
		random:            options.Random,
		now:               options.Now,
	}
}

func (i *Idempotency) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReadMethod(r.Method) {
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

		capture := newBufferedResponse(i.maxCachedResponse)
		next.ServeHTTP(capture, r)
		completionContext := context.WithoutCancel(r.Context())
		if capture.overflow {
			if err := i.store.CompleteAutomationCommand(completionContext, claimed.ID, "failed",
				http.StatusInternalServerError, "application/json", nil, "response_too_large"); err != nil {
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
		contentType := capture.header.Get("Content-Type")
		if err := i.store.CompleteAutomationCommand(completionContext, claimed.ID, commandStatus,
			status, contentType, capture.body.Bytes(), ""); err != nil {
			writeError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "idempotency result could not be persisted", true)
			return
		}
		copyHeaders(w.Header(), capture.header)
		w.WriteHeader(status)
		_, _ = w.Write(capture.body.Bytes())
	})
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
