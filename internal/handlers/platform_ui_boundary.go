package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"openpoet/internal/application"
)

const platformUIActorID = "local-ui"

var platformUICorrelationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

// platformUIContext carries the same bounded actor/correlation metadata used
// by Automation commands. Browser-provided identity is intentionally ignored:
// the current UI is local and has no authenticated multi-user principal.
func platformUIContext(r *http.Request) context.Context {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	correlationID := ""
	if r != nil {
		candidate := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if platformUICorrelationPattern.MatchString(candidate) {
			correlationID = candidate
		}
	}
	if correlationID == "" {
		correlationID = "ui:" + uuid.NewString()
	}
	return application.WithEventMetadata(ctx, application.EventMetadata{
		Actor:         platformUIActor(),
		CorrelationID: correlationID,
	})
}

func platformUIActor() application.Actor {
	return application.Actor{Type: "user", ID: platformUIActorID}
}

// platformUIAuthorization represents the user's direct interaction with a
// local OpenPoet control. It satisfies Application Service boundaries without
// accepting an approver or reason supplied by the browser payload.
func platformUIAuthorization(r *http.Request) application.ActionAuthorization {
	reason := "OpenPoet UI request"
	if r != nil {
		method := strings.TrimSpace(r.Method)
		path := ""
		if r.URL != nil {
			path = strings.TrimSpace(r.URL.Path)
		}
		if method != "" && path != "" {
			reason = method + " " + path
		}
	}
	return application.ActionAuthorization{
		Actor:      platformUIActor(),
		Reason:     reason,
		Approved:   true,
		ApprovedBy: "user:" + platformUIActorID,
	}
}

func requirePlatformApplicationServices(a *API, w http.ResponseWriter) (*PlatformApplicationServices, bool) {
	if a != nil {
		if services, ok := a.platformApplicationServices(); ok && services != nil {
			return services, true
		}
	}
	respondError(w, http.StatusServiceUnavailable, "OpenPoet platform services are unavailable")
	return nil, false
}
