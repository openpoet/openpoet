package coordinator

import (
	"encoding/json"
	"testing"
	"time"
)

// TestTurnCompletedCarriesReportRef: the turn event carries the report pointer
// when the session has one, and omits the field entirely when it does not
// (additive payload — SchemaVersion stays 1).
func TestTurnCompletedCarriesReportRef(t *testing.T) {
	withRef := turnCompletedEvent("sess-1", 7, []string{"a.go"}, "rep-42", time.Now())
	var payload map[string]any
	if err := json.Unmarshal([]byte(withRef.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["report_ref"] != "rep-42" {
		t.Fatalf("report_ref missing/wrong: %v", payload)
	}
	if withRef.SchemaVersion != 1 {
		t.Fatalf("additive field must not bump schema version (got %d)", withRef.SchemaVersion)
	}

	without := turnCompletedEvent("sess-1", 7, []string{"a.go"}, "", time.Now())
	payload = map[string]any{}
	if err := json.Unmarshal([]byte(without.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["report_ref"]; present {
		t.Fatalf("empty ref must omit the field: %v", payload)
	}
	if payload["files_touched"] == nil {
		t.Fatalf("files_touched contract broken: %v", payload)
	}
}
