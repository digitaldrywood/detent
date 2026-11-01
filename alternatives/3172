package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/store"
)

func (r *Runner) checkResumePolicy(ctx context.Context, req RunRequest, resume store.AgentResumeState) error {
	if resume.DetentSessionID == 0 {
		if req.Policy.ID != "" && (resume.ProviderThreadID != "" || resume.ProviderSessionID != "") {
			return errors.New("provider resume requires a persisted session identity")
		}
		return nil
	}
	source, ok := r.store.(store.SessionPolicyStore)
	if !ok {
		if req.Policy.ID == "" {
			return nil
		}
		return errors.New("session configuration store is required before resuming a Hub attempt")
	}
	_, err := source.SessionPolicy(ctx, req.ProjectID, resume.DetentSessionID)
	if err != nil {
		return fmt.Errorf("resume requires the persisted session configuration: %w", err)
	}
	return nil
}
