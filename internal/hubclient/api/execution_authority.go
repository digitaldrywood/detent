package api

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func WithMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, cancellation func() error) context.Context {
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: scope, lease: lease, cancellation: cancellation,
		guard: requestGuard{done: ctx.Done(), cause: func() error { return context.Cause(ctx) }}})
}

func WithPublicationRetry(ctx context.Context, retry func(context.Context, func() error) error) context.Context {
	authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	if !ok {
		return ctx
	}
	authority.retry = retry
	authority.publications = true
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, authority)
}

func MutationEffectError(ctx context.Context) error { return nativeEffectError(ctx) }
