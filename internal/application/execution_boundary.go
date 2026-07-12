package application

import "strings"

// ActionAuthorization keeps actor identity and explicit approval at the
// application boundary. HTTP authentication alone must not silently authorize
// environment injection, unsafe session flags, destructive stops, file writes,
// permission responses, or commits.
type ActionAuthorization struct {
	Actor                  Actor
	Reason                 string
	Approved               bool
	ApprovedBy             string
	AllowEnvironment       bool
	AllowUnsafePermissions bool
}

func requireActionActor(boundary ActionAuthorization) error {
	if strings.TrimSpace(boundary.Actor.Type) == "" || strings.TrimSpace(boundary.Actor.ID) == "" {
		return validationError("action_actor_required", "An authenticated actor type and ID are required")
	}
	return nil
}

func requireExplicitActionApproval(boundary ActionAuthorization) error {
	if err := requireActionActor(boundary); err != nil {
		return err
	}
	if !boundary.Approved || strings.TrimSpace(boundary.ApprovedBy) == "" || strings.TrimSpace(boundary.Reason) == "" {
		return validationError("action_approval_required", "Explicit approval, approver, and reason are required")
	}
	return nil
}
