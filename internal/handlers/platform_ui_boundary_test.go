package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"openpoet/internal/application"
)

func TestPlatformUIBoundaryUsesFixedActorAndBoundedCorrelation(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/config/settings", strings.NewReader(`{}`))
	request.Header.Set("X-Request-ID", "ain:ui-request")
	request.Header.Set("X-OpenPoet-Actor", "attacker")

	metadata := application.EventMetadataFromContext(platformUIContext(request))
	if metadata.Actor != platformUIActor() || metadata.CorrelationID != "ain:ui-request" {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	authorization := platformUIAuthorization(request)
	if authorization.Actor != platformUIActor() || !authorization.Approved || authorization.ApprovedBy != "user:local-ui" {
		t.Fatalf("unexpected authorization: %+v", authorization)
	}
	if authorization.Reason != "POST /api/config/settings" {
		t.Fatalf("unexpected reason: %q", authorization.Reason)
	}
}

func TestPlatformUIBoundaryRejectsUntrustedCorrelation(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/projects", nil)
	request.Header.Set("X-Request-ID", "bad\ncorrelation")
	metadata := application.EventMetadataFromContext(platformUIContext(request))
	if !strings.HasPrefix(metadata.CorrelationID, "ui:") || strings.ContainsAny(metadata.CorrelationID, "\r\n") {
		t.Fatalf("unsafe correlation: %q", metadata.CorrelationID)
	}
}
