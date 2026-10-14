package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ValidatorVersion(ctx context.Context) (runner.NativeValidation, error) {
	return e.validationVersion(ctx, false)
}

func (e *nativeExecution) ReviewVersion(ctx context.Context) (runner.NativeValidation, error) {
	return e.validationVersion(ctx, true)
}

func (e *nativeExecution) validationVersion(ctx context.Context, implementationReview bool) (runner.NativeValidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.Landing != nil || e.change.VersionID == "" || e.lastDiff == nil {
		return runner.NativeValidation{}, nil
	}
	detail, err := e.claim.source.client.runtimeChange(ctx, e.claim.lease.WorkItemID, e.change.ChangeID)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	for _, version := range detail.Versions {
		if version.ID != e.change.VersionID || version.ID != detail.Change.CurrentVersion {
			continue
		}
		if version.HeadSHA != e.lastDiff.HeadSHA || version.BaseSHA != e.lastDiff.BaseSHA || version.PolicyID != e.data.PolicyID {
			return runner.NativeValidation{}, errors.New("native validator version differs from the published source")
		}
		var discussion runner.NativeReviewDiscussion
		if implementationReview {
			if version.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired {
				return runner.NativeValidation{}, nil
			}
			recovery, err := e.claim.source.client.Recovery(ctx, e.claim.lease.WorkItemID)
			if err != nil {
				return runner.NativeValidation{}, err
			}
			discussion = runner.NativeReviewDiscussionForVersion(recovery.Discussion, detail.Discussion, version.ID)
			var latest *tracker.ChangeReview
			for i := range detail.Reviews {
				review := &detail.Reviews[i]
				if review.VersionID == version.ID && review.ImplementationReview != nil && review.ImplementationReview.HeadSHA == version.HeadSHA && (latest == nil || !review.CreatedAt.Before(latest.CreatedAt)) {
					latest = review
				}
			}
			if latest != nil && latest.Decision == "approved" && !discussion.HumanFeedbackSince(latest.CreatedAt) {
				return runner.NativeValidation{}, nil
			}
		} else if !version.Policy.Gates.Validator || detail.Summary.Status == "reviewed" {
			return runner.NativeValidation{}, nil
		}
		raw, err := json.Marshal(e.lastDiff.Files)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		diff := &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest(raw)}
		var patch strings.Builder
		complete := true
		for _, file := range e.lastDiff.Files {
			diff.Files = append(diff.Files, file.Path)
			patch.WriteString(file.Patch)
			complete = complete && !file.Truncated && !file.Denied
		}
		if complete {
			diff.Patch = patch.String()
		}
		runtime, err := e.claim.source.client.RuntimeEvidence(ctx, e.claim.lease.WorkItemID, e.data.AttemptID)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		if runtime.Issue.OrganizationID != e.claim.source.client.organization || runtime.Issue.ProjectID != e.claim.source.client.project || runtime.Issue.WorkItemID != e.claim.lease.WorkItemID || runtime.Attempt == nil || runtime.Attempt.AttemptID != e.data.AttemptID || runtime.Attempt.LeaseID != e.claim.lease.ID || runtime.Attempt.FencingToken != e.claim.lease.FencingToken || runtime.Attempt.PolicyID != e.data.PolicyID {
			return runner.NativeValidation{}, errors.New("native validator evidence differs from the current attempt")
		}
		var commands []gate.CommandResult
		for _, review := range detail.Reviews {
			if review.VersionID != version.ID || review.Actor.Kind != "runner" {
				continue
			}
			for _, result := range []*gate.ValidatorResult{review.ImplementationReview, review.Validator} {
				if result != nil && result.VersionID == version.ID && result.HeadSHA == version.HeadSHA && result.BaseSHA == version.BaseSHA && result.Repository == version.Repository {
					commands = append(commands, result.Commands...)
				}
			}
		}
		return runner.NativeValidation{Discussion: discussion, Version: &version, Diff: diff, Evidence: runtime.Attempt.Evidence, Commands: commands}, nil
	}
	return runner.NativeValidation{}, errors.New("native validator version is no longer current")
}

func (e *nativeExecution) RecordReview(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.lastDiff == nil || e.change.VersionID != result.VersionID || e.lastDiff.HeadSHA != result.HeadSHA || e.lastDiff.BaseSHA != result.BaseSHA {
		return errors.New("native implementation review differs from the published source")
	}
	decision := "commented"
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Independent automated implementation review of immutable head %s in fresh session %d. This is a source review, separate from acceptance validation.\n\n%s", result.HeadSHA, result.SessionID, result.Summary)
	for _, finding := range result.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	for _, command := range result.Commands {
		fmt.Fprintf(&body, "\n\nHost command evidence: %s; head=%s tree=%s exit=%d", command.Command, command.HeadSHA, command.TreeSHA, command.ExitCode)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":implementation-review:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), ImplementationReview: &result,
	})
	if err != nil {
		return err
	}
	e.change.ImplementationReview = &result
	e.change.Reviewed = false
	detail, err := client.runtimeChange(ctx, item, e.change.ChangeID)
	if err != nil {
		if result.Verdict == gate.ValidatorVerdictRework {
			return nil
		}
		return err
	}
	e.change.Reviewed = detail.Change.CurrentVersion == result.VersionID && detail.Summary.Status == "reviewed"
	return nil
}

func (e *nativeExecution) RecordValidator(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.VersionID != result.VersionID {
		return errors.New("native validator result differs from the published version")
	}
	decision := "commented"
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	}
	var body strings.Builder
	body.WriteString(result.Summary)
	if len(result.Commands) > 0 {
		fmt.Fprintf(&body, "\n\nHost-observed validation command evidence: %d receipts in this review's structured validator.commands field, including command, head/tree, exit status and captured output.", len(result.Commands))
	}

	for _, finding := range result.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":validator:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), Validator: &result,
	})
	if err != nil {
		return err
	}
	detail, err := client.runtimeChange(ctx, item, e.change.ChangeID)
	if err != nil {
		return err
	}
	e.change.Validator = &result
	e.change.Reviewed = detail.Change.CurrentVersion == result.VersionID && detail.Summary.Status == "reviewed"
	return nil
}
