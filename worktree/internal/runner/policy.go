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
			return errors.New("policy_mismatch: provider resume requires a persisted session identity")
		}
		return nil
	}
	source, ok := r.store.(store.SessionPolicyStore)
	if !ok {
		if req.Policy.ID == "" {
			return nil
		}
		return errors.New("policy_mismatch: session policy store is required before resuming a Hub attempt")
	}
	pinned, err := source.SessionPolicy(ctx, req.ProjectID, resume.DetentSessionID)
	if err != nil {
		return fmt.Errorf("policy_mismatch: resume requires the persisted approved policy: %w", err)
	}
	if pinned.ID == "" && req.Policy.ID == "" {
		return nil
	}
	return pinned.Match(req.Policy)
}
