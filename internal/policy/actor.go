package policy

import "context"

// Actor identifies the caller for audit purposes. It is populated by a
// middleware that bridges the authenticated identity (from
// internal/auth) and request metadata into the request context, then
// retrieved by the audit-wrapping engine.
//
// Zero values are valid: an anonymous request has UserID=0 and
// TokenID=0; missing request metadata simply produces empty strings.
type Actor struct {
	UserID     int64
	TokenID    int64
	RequestID  string
	RemoteAddr string
	UserAgent  string
}

// actorContextKey is unexported so callers can't put their own Actor on
// the context with a colliding key.
type actorContextKey struct{}

// WithActor returns a new context carrying actor.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFromContext returns the actor stored on ctx, or the zero Actor
// when none has been set.
func ActorFromContext(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorContextKey{}).(Actor); ok {
		return a
	}
	return Actor{}
}
