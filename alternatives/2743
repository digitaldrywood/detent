package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) Run(ctx context.Context, req RunRequest) (result RunResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = gobudget.WithContext(ctx, r.goBudget)
	ctx = r.buildCache.WithContext(ctx)
	ctx = r.withCacheWaitObserver(ctx, req)
	ctx = r.withValidationProcessOwner(ctx, req.Issue, req.WorkAttemptID)
	ctx = gotestreceipt.WithContext(ctx, r.goTestReceipts)
	return r.runExecution(ctx, req)
}

func (r *Runner) ReapWorkspace(ctx context.Context, issue connector.Issue) (result WorkspaceReapResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = r.buildCache.WithContext(ctx)
	ctx = r.withCacheWaitObserver(ctx, RunRequest{Issue: issue})
	ctx = r.withValidationProcessOwner(ctx, issue, 0)
	return r.reapWorkspace(ctx, issue)
}

func (r *Runner) ReconcileWorkspaces(ctx context.Context, activeIssues []connector.Issue) (result WorkspaceReconcileResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = r.buildCache.WithContext(ctx)
	ctx = r.withValidationProcessOwner(ctx, connector.Issue{}, 0)
	return r.reconcileWorkspaces(ctx, activeIssues)
}

func (r *Runner) WorkspaceCleanupIssues(ctx context.Context, cursor string, limit int) ([]connector.Issue, string, error) {
	inventory, ok := r.workspaceSnapshot().(interface {
		CleanupIssues(context.Context, string, int) ([]workspace.Issue, string, error)
	})
	if !ok {
		return nil, cursor, errors.ErrUnsupported
	}
	issues, next, err := inventory.CleanupIssues(ctx, cursor, limit)
	var result []connector.Issue
	for _, issue := range issues {
		result = append(result, connector.Issue{ID: issue.ID, Identifier: issue.Identifier})
	}
	return result, next, err
}

func (r *Runner) Validate(ctx context.Context, req ValidatorRequest) (result gate.ValidatorResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = gobudget.WithContext(ctx, r.goBudget)
	ctx = r.buildCache.WithContext(ctx)
	ctx = r.withCacheWaitObserver(ctx, RunRequest{Issue: req.Issue})
	ctx = r.withValidationProcessOwner(ctx, req.Issue, 0)
	ctx = gotestreceipt.WithContext(ctx, r.goTestReceipts)
	return r.validate(ctx, req)
}

func (r *Runner) Audit(ctx context.Context, req SecurityAuditRequest) (result SecurityAuditExecution, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = gobudget.WithContext(ctx, r.goBudget)
	ctx = r.buildCache.WithContext(ctx)
	ctx = r.withCacheWaitObserver(ctx, RunRequest{Issue: req.Issue})
	ctx = r.withValidationProcessOwner(ctx, req.Issue, 0)
	return r.audit(ctx, req)
}

func (r *Runner) withCacheWaitObserver(ctx context.Context, req RunRequest) context.Context {
	return toolcache.WithWaitObserver(ctx, func(lock string, waiting bool) {
		if profile := req.activityProfile; profile != nil {
			profile.runtimeMu.Lock()
			defer profile.runtimeMu.Unlock()
			profile.cacheWait = ""
			if waiting {
				profile.cacheWait = lock
			}
		}
		phase := nativeRuntimePhase(req)
		if waiting {
			phase = "waiting_cache_" + lock
		}
		r.logWorkerEventLevel(slog.LevelInfo, req.Issue, "worker_cache_wait", "work_attempt_id", req.WorkAttemptID, "cache_lock", lock, "waiting", waiting)
		if runtime, ok := req.Execution.(RuntimeExecution); ok {
			observeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if err := runtime.ObserveRuntime(observeCtx, tracker.NativeRuntimeObservation{LocalAttemptID: req.WorkAttemptID, Generation: req.Generation, Phase: phase, HeartbeatAt: r.now()}); err != nil {
				r.logger.Warn("cache wait observation unavailable", "work_attempt_id", req.WorkAttemptID, "error", err)
			}
		}
	})
}
