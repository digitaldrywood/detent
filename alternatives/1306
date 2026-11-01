package nativechange

import (
	"context"
	"log/slog"
	"slices"

	"github.com/digitaldrywood/detent/internal/runner"
)

func (e *nativeExecution) SetDependencyReasonGenerator(generate runner.DependencyReasonGenerator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reasonGenerator = generate
}

func (e *nativeExecution) refreshReasons(ctx context.Context, finished bool) {
	e.mu.Lock()
	generate := e.reasonGenerator
	before := slices.Clone(e.data.Usage)
	e.mu.Unlock()
	if generate == nil {
		return
	}
	if err := e.claim.source.NativeClient().RefreshDependencyReasons(ctx, e.claim.lease.WorkItemID, generate); err != nil {
		slog.Warn("Dependency reason refresh unavailable", "error", err)
	}
	if finished {
		if err := e.flushReasonUsage(ctx, before); err != nil {
			slog.Warn("Dependency reason usage unavailable", "error", err)
		}
	}
}
