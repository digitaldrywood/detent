package api

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func WithMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, cancellation func() error) context.Context {
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: scope, lease: lease, cancellation: cancellation})
}

func MutationEffectError(ctx context.Context) error { return nativeEffectError(ctx) }
