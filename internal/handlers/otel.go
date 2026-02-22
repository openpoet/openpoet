package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"openpoet/internal/database"
	"openpoet/internal/llm"
	"strconv"
	"sync"
)

// OTELHandler receives OpenTelemetry metrics from Claude Code sessions
// and accumulates token usage data per session.
type OTELHandler struct {
	db *database.DB

	mu       sync.Mutex
	sessions map[string]*sessionMetrics // sessionID -> accumulated metrics
}

// sessionMetrics holds the latest cumulative metric values for a session.
type sessionMetrics struct {
	// Per-model token counts (cumulative from OTLP)
	models map[string]*modelMetrics
	// Total cost reported by Claude Code
	costByModel map[string]float64
}

type modelMetrics struct {
	inputTokens         int64
	outputTokens        int64
	cacheReadTokens     int64
	cacheCreationTokens int64
}

func NewOTELHandler(db *database.DB) *OTELHandler {
	return &OTELHandler{
		db:       db,
		sessions: make(map[string]*sessionMetrics),
	}
}

// HandleMetrics receives OTLP HTTP JSON metrics at POST /v1/metrics.
func (h *OTELHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var payload otlpMetricsPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	for _, rm := range payload.ResourceMetrics {
		sessionID := extractResourceAttribute(rm.Resource, "openpoet.session_id")
		if sessionID == "" {
			continue
		}

		h.mu.Lock()
		sm, ok := h.sessions[sessionID]
		if !ok {
			sm = &sessionMetrics{
				models:      make(map[string]*modelMetrics),
				costByModel: make(map[string]float64),
			}
			h.sessions[sessionID] = sm
		}

		for _, scope := range rm.ScopeMetrics {
			for _, metric := range scope.Metrics {
				h.processMetric(sm, metric)
			}
		}
		h.mu.Unlock()
	}

	// OTLP expects a 200 with empty JSON response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

// HandleTraces receives OTLP traces at POST /v1/traces (accept and discard).
func (h *OTELHandler) HandleTraces(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

// HandleLogs receives OTLP logs at POST /v1/logs (accept and discard).
func (h *OTELHandler) HandleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

// SessionTokenSummary holds live token usage for an active session.
type SessionTokenSummary struct {
	TotalInputTokens  int64   `json:"total_input_tokens"`
	TotalOutputTokens int64   `json:"total_output_tokens"`
	TotalCost         float64 `json:"total_cost"`
}

// GetLiveMetrics returns the current in-memory accumulated token metrics for a session.
// Returns nil if no metrics have been received for this session.
func (h *OTELHandler) GetLiveMetrics(sessionID string) *SessionTokenSummary {
	h.mu.Lock()
	defer h.mu.Unlock()

	sm, ok := h.sessions[sessionID]
	if !ok || len(sm.models) == 0 {
		return nil
	}

	summary := &SessionTokenSummary{}
	for model, mm := range sm.models {
		summary.TotalInputTokens += mm.inputTokens
		summary.TotalOutputTokens += mm.outputTokens
		if cost, ok := sm.costByModel[model]; ok {
			summary.TotalCost += cost
		}
	}
	return summary
}

// FlushSession writes the accumulated metrics for a session to the database
// and removes the session from the in-memory map.
func (h *OTELHandler) FlushSession(sessionID string) {
	h.mu.Lock()
	sm, ok := h.sessions[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, sessionID)
	h.mu.Unlock()

	if len(sm.models) == 0 {
		return
	}

	// Look up session to get project_id
	ctx := context.Background()
	sess, err := h.db.GetSession(ctx, sessionID)
	if err != nil {
		log.Printf("[OTEL] Failed to get session %s for flushing: %v", sessionID[:8], err)
		return
	}

	// Write one token_usage record per model
	for model, mm := range sm.models {
		if mm.inputTokens == 0 && mm.outputTokens == 0 {
			continue
		}

		cost := sm.costByModel[model]
		if cost == 0 {
			cost = llm.CalculateCost(model, int(mm.inputTokens), int(mm.outputTokens))
		}

		tu := &database.TokenUsage{
			Source:              "claude_code",
			ProjectID:           sql.NullInt64{Int64: sess.ProjectID, Valid: true},
			SessionID:           sql.NullString{String: sessionID, Valid: true},
			Model:               model,
			InputTokens:         int(mm.inputTokens),
			OutputTokens:        int(mm.outputTokens),
			CacheReadTokens:     int(mm.cacheReadTokens),
			CacheCreationTokens: int(mm.cacheCreationTokens),
			CostUSD:             cost,
		}

		if err := h.db.CreateTokenUsage(ctx, tu); err != nil {
			log.Printf("[OTEL] Failed to save token usage for session %s model %s: %v", sessionID[:8], model, err)
		} else {
			log.Printf("[OTEL] Saved token usage for session %s: model=%s input=%d output=%d cost=$%.4f",
				sessionID[:8], model, mm.inputTokens, mm.outputTokens, cost)
		}
	}
}

// processMetric updates session metrics from an OTLP metric data point.
func (h *OTELHandler) processMetric(sm *sessionMetrics, metric otlpMetric) {
	switch metric.Name {
	case "claude_code.token.usage":
		h.processTokenMetric(sm, metric)
	case "claude_code.cost.usage":
		h.processCostMetric(sm, metric)
	}
}

func (h *OTELHandler) processTokenMetric(sm *sessionMetrics, metric otlpMetric) {
	var dataPoints []otlpDataPoint
	if metric.Sum != nil {
		dataPoints = metric.Sum.DataPoints
	}
	if metric.Gauge != nil {
		dataPoints = append(dataPoints, metric.Gauge.DataPoints...)
	}

	for _, dp := range dataPoints {
		model := extractDataPointAttribute(dp, "model")
		tokenType := extractDataPointAttribute(dp, "type")
		value := dp.AsInt()

		if model == "" {
			model = "unknown"
		}

		mm, ok := sm.models[model]
		if !ok {
			mm = &modelMetrics{}
			sm.models[model] = mm
		}

		switch tokenType {
		case "input":
			mm.inputTokens = value
		case "output":
			mm.outputTokens = value
		case "cacheRead":
			mm.cacheReadTokens = value
		case "cacheCreation":
			mm.cacheCreationTokens = value
		}
	}
}

func (h *OTELHandler) processCostMetric(sm *sessionMetrics, metric otlpMetric) {
	var dataPoints []otlpDataPoint
	if metric.Sum != nil {
		dataPoints = metric.Sum.DataPoints
	}

	for _, dp := range dataPoints {
		model := extractDataPointAttribute(dp, "model")
		if model == "" {
			model = "unknown"
		}
		sm.costByModel[model] = dp.AsDouble()
	}
}

// --- OTLP JSON types ---

type otlpMetricsPayload struct {
	ResourceMetrics []otlpResourceMetric `json:"resourceMetrics"`
}

type otlpResourceMetric struct {
	Resource     otlpResource      `json:"resource"`
	ScopeMetrics []otlpScopeMetric `json:"scopeMetrics"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeMetric struct {
	Metrics []otlpMetric `json:"metrics"`
}

type otlpMetric struct {
	Name  string     `json:"name"`
	Sum   *otlpSum   `json:"sum,omitempty"`
	Gauge *otlpGauge `json:"gauge,omitempty"`
}

type otlpSum struct {
	DataPoints []otlpDataPoint `json:"dataPoints"`
}

type otlpGauge struct {
	DataPoints []otlpDataPoint `json:"dataPoints"`
}

type otlpDataPoint struct {
	Attributes  []otlpAttribute `json:"attributes"`
	RawAsInt    json.RawMessage `json:"asInt,omitempty"`
	RawAsDouble *float64        `json:"asDouble,omitempty"`
}

func (dp otlpDataPoint) AsInt() int64 {
	if dp.RawAsInt != nil {
		s := string(dp.RawAsInt)
		// OTLP JSON encodes int64 as string
		s = trimQuotes(s)
		v, _ := strconv.ParseInt(s, 10, 64)
		return v
	}
	// Claude Code sends token counts as asDouble
	if dp.RawAsDouble != nil {
		return int64(*dp.RawAsDouble)
	}
	return 0
}

func (dp otlpDataPoint) AsDouble() float64 {
	if dp.RawAsDouble != nil {
		return *dp.RawAsDouble
	}
	// Try parsing from asInt
	if dp.RawAsInt != nil {
		s := trimQuotes(string(dp.RawAsInt))
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	return 0
}

type otlpAttribute struct {
	Key   string        `json:"key"`
	Value otlpAttrValue `json:"value"`
}

type otlpAttrValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
}

func extractResourceAttribute(res otlpResource, key string) string {
	for _, attr := range res.Attributes {
		if attr.Key == key {
			if attr.Value.StringValue != nil {
				return *attr.Value.StringValue
			}
		}
	}
	return ""
}

func extractDataPointAttribute(dp otlpDataPoint, key string) string {
	for _, attr := range dp.Attributes {
		if attr.Key == key {
			if attr.Value.StringValue != nil {
				return *attr.Value.StringValue
			}
			if attr.Value.IntValue != nil {
				return *attr.Value.IntValue
			}
		}
	}
	return ""
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
