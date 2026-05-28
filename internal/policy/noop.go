package policy

import "context"

// NoopEngine is an Engine that always returns Allow. Used as the default
// when no controls are configured, and during incremental rollout so
// hook sites can be added before the real engine is wired in.
type NoopEngine struct{}

// Evaluate always returns Allow.
func (NoopEngine) Evaluate(_ context.Context, _ Subject, _ Action) Result {
	return Result{Decision: Allow}
}

// Compile-time interface check.
var _ Engine = NoopEngine{}
