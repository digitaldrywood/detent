package api

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func WithMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, cancellation func() error) context.Context {
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: scope, lease: lease, cancellation: cancellation,
		guard: requestGuard{done: ctx.Done(), check: func() error { return context.Cause(ctx) }}})
}

type mutationAuthorityOwner interface {
	ExecutionAuthorityError() error
	RejectMutationAuthority(context.Context, error) error
}

func WithExecutionMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, execution runner.Execution) context.Context {
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, executionMutationAuthority(ctx, scope, lease, execution))
}

func executionMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, execution runner.Execution) nativeMutationAuthority {
	cause := func() error { return context.Cause(ctx) }
	authority := nativeMutationAuthority{scope: scope, lease: lease, cancellation: cause, guard: requestGuard{done: ctx.Done(), check: cause}}
	if owner, ok := execution.(mutationAuthorityOwner); ok {
		authority.owner = owner
	}
	return authority
}

type requestFenceKey struct{}

func WithRequestFence(ctx context.Context, done <-chan struct{}, check func() error) context.Context {
	return context.WithValue(ctx, requestFenceKey{}, requestGuard{done: done, check: check})
}

func requestFence(ctx context.Context) requestGuard {
	if guard, ok := ctx.Value(requestFenceKey{}).(requestGuard); ok {
		return guard
	}
	return requestGuard{}
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

func (authority nativeMutationAuthority) fenceRequest(ctx context.Context) context.Context {
	explicit := requestFence(ctx)
	done := authority.guard.done
	if done == nil {
		done = explicit.done
	}
	return WithRequestFence(ctx, done, func() error {
		if err := nativeEffectError(ctx); err != nil {
			return err
		}
		if cause := authority.guard.err(); errors.Is(cause, runner.ErrExecutionAuthorityUnavailable) || errors.Is(cause, runner.ErrOperatorStopped) {
			return cause
		}
		return explicit.err()
	})
}
