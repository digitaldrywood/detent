package hookproc

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type ProcessSource func(context.Context, string) ([]int, error)

type processSourceKey struct{}

func WithProcessSource(ctx context.Context, source ProcessSource) context.Context {
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, processSourceKey{}, source)
}

const HookOwnerKey = "DETENT_HOOK_OWNER"

type hookOwnerKey struct{}
type lifetimeKey struct{}

func (l *Lifetime) Reap(ctx context.Context, path, owner string) (int, error) {
	if owner == "" {
		return 0, errors.New("hook owner is required")
	}
	ctx = context.WithValue(ctx, lifetimeKey{}, l)
	return ReapProcesses(context.WithValue(ctx, hookOwnerKey{}, owner), path, procgroup.DefaultTerminationGrace)
}

func hookSignalAllowed(owner, path string, env []string) bool {
	if owner != "" {
		return slices.Contains(env, HookOwnerKey+"="+owner) && slices.Contains(env, "DETENT_WORKSPACE="+path)
	}
	return !slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, HookOwnerKey+"=") })
}
