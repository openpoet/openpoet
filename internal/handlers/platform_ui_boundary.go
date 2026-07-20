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
		Actor:         requestActor(r),
		CorrelationID: correlationID,
	})
}

func platformUIActor() application.Actor {
	return application.Actor{Type: "user", ID: platformUIActorID}
}

// requestActor returns the verified actor resolved by ResolveActorMiddleware
// for this request, falling back to the local-UI actor when no credential was
// resolved (loopback reads, tests calling handlers directly without the
// middleware). This is what makes the audit trail attribute a mutation to the
// session/automation/device that truly performed it.
func requestActor(r *http.Request) application.Actor {
	if r != nil {
		if info, ok := resolvedActorFromContext(r.Context()); ok {
			return info.actor
		}
	}
	return platformUIActor()
}

// platformUIAuthorization represents the verified caller's direct interaction
// with an OpenPoet control. Owner-tier actors (UI cookie, paired device,
// automation bearer) are Approved so their click authorizes the action;
// session-tier actors are NOT approved, so destructive/env-gated verbs fall
// through to the approval broker with a structured approval_required error.
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
	actor := platformUIActor()
	approved := true
	if r != nil {
		if info, ok := resolvedActorFromContext(r.Context()); ok {
			actor = info.actor
			approved = info.approved
		}
	}
	authorization := application.ActionAuthorization{
		Actor:  actor,
		Reason: reason,
	}
	if approved {
		authorization.Approved = true
		authorization.ApprovedBy = application.EventActorValue(actor)
	}
	return authorization
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
