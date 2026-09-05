package orchestrator

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/policy"
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

func (o *Orchestrator) checkAttemptPolicy(attempt store.WorkAttempt) error {
	if o.cfg.Policy.ID == "" {
		return nil
	}
	var metadata struct {
		Policy policy.Descriptor `json:"policy"`
	}
	if err := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata); err != nil {
		return errors.New("policy_mismatch: recovery requires the attempt's persisted policy identity")
	}
	return metadata.Policy.Match(o.cfg.Policy)
}
