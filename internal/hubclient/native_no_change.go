package hubclient

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) noChangeValidation(ctx context.Context) (runner.NativeValidation, error) {
	d := e.preparedDisposition
	if d.NoChange.Validate() != nil || e.lastDiff == nil || e.lastDiff.BaseSHA != d.NoChange.VerifiedSHA || e.lastDiff.HeadSHA != d.NoChange.VerifiedSHA || len(e.lastDiff.Files) != 0 {
		return runner.NativeValidation{}, errors.New("no-change validation source is unavailable")
	}
	runtime, err := e.claim.source.client.RuntimeEvidence(ctx, e.claim.lease.WorkItemID, e.data.AttemptID)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	if runtime.Attempt == nil || runtime.Attempt.LeaseID != e.claim.lease.ID || runtime.Attempt.FencingToken != e.claim.lease.FencingToken || runtime.Attempt.PolicyID != e.data.PolicyID {
		return runner.NativeValidation{}, errors.New("no-change evidence owner differs from the execution")
	}
	encoded, err := json.Marshal(d.NoChange)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	raw := string(encoded)
	e.scheduler.mu.Lock()
	pinned := e.scheduler.claimPolicies[string(e.claim.lease.WorkItemID)]
	e.scheduler.mu.Unlock()
	if pinned.descriptor.ID != e.data.PolicyID {
		return runner.NativeValidation{}, errors.New("no-change policy differs from the execution")
	}
	version := &tracker.ChangeVersion{ID: e.data.AttemptID, Policy: pinned.descriptor, ChangeVersionInput: tracker.ChangeVersionInput{
		BaseSHA: d.NoChange.VerifiedSHA, HeadSHA: d.NoChange.VerifiedSHA, Repository: e.repository, PolicyID: e.data.PolicyID, Validation: e.validation,
	}}
	diff := &connector.ValidationDiff{Repository: e.repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest([]byte(raw))}
	discussion := runner.NativeReviewDiscussion{Issue: append([]tracker.NativeComment(nil), e.claim.recovery.Discussion...)}
	discussion.Issue = append(discussion.Issue, tracker.NativeComment{Body: raw})
	return runner.NativeValidation{Version: version, Diff: diff, Discussion: discussion, Evidence: runtime.Attempt.Evidence}, nil
}

func (e *nativeExecution) NoChangeResult(ctx context.Context) (*runner.NativeNoChange, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.preparedDisposition == nil || e.preparedDisposition.Status != "no_change_needed" || e.preparedDisposition.NoChange.Validate() != nil {
		return &runner.NativeNoChange{}, nil
	}
	result := &runner.NativeNoChange{VerifiedSHA: e.preparedDisposition.NoChange.VerifiedSHA, Validation: e.validation}
	for _, receipt := range e.data.Steps {
		step, err := e.restoreStep(ctx, receipt)
		if err != nil {
			return nil, err
		}
		switch step.Step {
		case "review":
			result.Review = step.Validator
		case "validator":
			result.Validator = step.Validator
		case "security_audit":
			result.Audit = step.SecurityAudit
		}
	}
	return result, nil
}
