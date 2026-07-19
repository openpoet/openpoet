package automation

import "context"

type Actor struct {
	Type     string   `json:"type"`
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Scopes   ScopeSet `json:"-"`
	ClientID string   `json:"client_id"`
	// ProjectFilter, when non-empty, scopes this actor to a set of projects. It
	// is the raw JSON {"project_ids":[...],"tag_ids":[...]} from the client row
	// (empty = unrestricted). Enforced centrally in DispatchPlatformCapability and
	// on read/push surfaces (sessions.list, the SSE stream).
	ProjectFilter string `json:"-"`
}

type actorContextKey struct{}

func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}
