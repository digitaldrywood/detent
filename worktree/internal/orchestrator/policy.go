package orchestrator

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/policy"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) checkDispatchPolicy(ctx context.Context) error {
	checker, ok := o.scheduling.(interface {
		CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
	})
	if !ok {
		return nil
	}
	return checker.CheckProjectPolicy(ctx, o.cfg.Project.ID, o.cfg.SchedulingRepository, o.cfg.Policy)
}

func (o *Orchestrator) prepareDispatchProject(ctx context.Context) error {
	preparer, ok := o.scheduling.(interface {
		PrepareProject(context.Context, string) error
	})
	if !ok {
		return nil
	}
	return preparer.PrepareProject(ctx, o.cfg.Project.ID)
}

func persistedAttemptPolicy(attempt store.WorkAttempt) (policy.Descriptor, bool) {
	var metadata struct {
		Policy *policy.Descriptor `json:"policy"`
	}
	if err := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata); err != nil || metadata.Policy == nil {
		return policy.Descriptor{}, false
	}
	return *metadata.Policy, metadata.Policy.Validate() == nil
}

func (o *Orchestrator) dispatchAttemptPolicy(retry Retry, workerHost string) policy.Descriptor {
	if retry.RetryMode == runpkg.RetryModeResume && retry.ResumeState.Orphaned && retry.Policy.ID != "" && (retry.WorkerHost == workerHost || o.nativeWorkflow()) {
		return retry.Policy
	}
	return o.cfg.Policy
}
