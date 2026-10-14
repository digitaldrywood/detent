package runner

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/gate"
)

func (r *Runner) Run(ctx context.Context, req RunRequest) (result RunResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, release, err := r.buildCache.Use(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, release()) }()
	ctx = r.withValidationProcessOwner(ctx, req.Issue, req.WorkAttemptID)
	return r.runExecution(ctx, req)
}

func (r *Runner) Validate(ctx context.Context, req ValidatorRequest) (result gate.ValidatorResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, release, err := r.buildCache.Use(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, release()) }()
	ctx = r.withValidationProcessOwner(ctx, req.Issue, 0)
	return r.validate(ctx, req)
}

func (r *Runner) Audit(ctx context.Context, req SecurityAuditRequest) (result SecurityAuditExecution, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, release, err := r.buildCache.Use(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, release()) }()
	ctx = r.withValidationProcessOwner(ctx, req.Issue, 0)
	return r.audit(ctx, req)
}
