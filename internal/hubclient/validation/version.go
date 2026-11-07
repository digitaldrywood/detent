package validation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/reviewevidence"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type VersionReader interface {
	RuntimeChange(context.Context, tracker.NativeWorkItemID, string) (tracker.ChangeDetail, error)
	Recovery(context.Context, tracker.NativeWorkItemID) (tracker.NativeRecovery, error)
	RuntimeEvidence(context.Context, tracker.NativeWorkItemID, string, ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error)
}
type VersionAuthority interface {
	Retain(context.Context, tracker.AttemptDiffRequest) error
	Retained() *tracker.ChangeSourceCapture
	SetDiscussion([]tracker.NativeComment)
	ApplyAuditHumanAction(*tracker.SecurityAuditReview)
}
type VersionState struct {
	Change         *runner.NativeChange
	Diff           *tracker.AttemptDiffRequest
	Data           tracker.NativeRunData
	Lease          tracker.NativeLease
	Organization   tracker.OrganizationID
	Project        tracker.ProjectID
	SourceRequired bool
}

func PrepareVersion(ctx context.Context, reader VersionReader, host VersionAuthority, state VersionState, stage string) (runner.NativeValidation, error) {
	if state.Change == nil || state.Change.Landing != nil || state.Change.VersionID == "" || state.Diff == nil {
		return runner.NativeValidation{}, nil
	}
	detail, err := reader.RuntimeChange(ctx, state.Lease.WorkItemID, state.Change.ChangeID)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	for _, version := range detail.Versions {
		if version.ID != state.Change.VersionID || version.ID != detail.Change.CurrentVersion {
			continue
		}
		if version.HeadSHA != state.Diff.HeadSHA || version.BaseSHA != state.Diff.BaseSHA {
			return runner.NativeValidation{}, errors.New("native validator version differs from the published source")
		}
		var discussion runner.NativeReviewDiscussion
		if stage == "review" {
			if !ImplementationReviewRequired(version) {
				return runner.NativeValidation{}, nil
			}
			recovery, err := reader.Recovery(ctx, state.Lease.WorkItemID)
			if err != nil {
				return runner.NativeValidation{}, err
			}
			discussion = runner.NativeReviewDiscussionForVersion(recovery.Discussion, detail.Discussion, version.ID)
			host.SetDiscussion(recovery.Discussion)
			if !NeedsImplementationReview(version, detail.Reviews, discussion) {
				return runner.NativeValidation{}, nil
			}

		} else if stage == "security_audit" {
			if !version.Policy.Gates.SecurityAudit || detail.Summary.SecurityAudit == "passed" && detail.Summary.Status != "stale_policy" {
				return runner.NativeValidation{}, nil
			}
			for _, review := range detail.Reviews {
				if review.VersionID == version.ID && review.SecurityAudit != nil && (review.Decision == "changes_requested" || review.SecurityAudit.Verdict == "wait") {
					state.Change.SecurityAudit = review.SecurityAudit
					state.Change.Reviewed = false
					host.ApplyAuditHumanAction(review.SecurityAudit)
					return runner.NativeValidation{}, nil
				}
			}
			for _, review := range detail.Reviews {
				if review.VersionID == version.ID && review.SecurityAudit != nil && !review.SecurityAudit.Landing && review.Decision == "approved" && review.SecurityAudit.Run.BaseSHA == version.BaseSHA && review.SecurityAudit.Run.HeadSHA == version.HeadSHA && review.SecurityAudit.Run.ReviewerVersion == securityaudit.ReviewerVersion && review.SecurityAudit.Run.ReviewerDigest == securityaudit.ReviewerDigest() {
					return runner.NativeValidation{}, nil
				}
			}
		} else if !version.Policy.Gates.Validator || detail.Summary.Status == "reviewed" {
			return runner.NativeValidation{}, nil
		}
		raw, err := json.Marshal(state.Diff.Files)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		diff := &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest(raw)}
		var patch strings.Builder
		complete := true
		for _, file := range state.Diff.Files {
			diff.Files = append(diff.Files, file.Path)
			patch.WriteString(file.Patch)
			complete = complete && !file.Truncated && !file.Denied
		}
		if complete {
			diff.Patch = patch.String()
		}
		if state.SourceRequired && version.Source != nil {
			if err := host.Retain(ctx, *state.Diff); err != nil {
				return runner.NativeValidation{}, err
			}
			capture := host.Retained()
			if capture == nil || capture.Source.BaseSHA != version.BaseSHA || capture.Source.HeadSHA != version.HeadSHA || capture.Source.DiffSHA256 != version.Source.DiffSHA256 {
				return runner.NativeValidation{}, errors.New("retained review source differs from the immutable version")
			}
			diff.Patch = capture.Patch
			diff.Digest = capture.Source.DiffSHA256
		}
		runtime, err := reader.RuntimeEvidence(ctx, state.Lease.WorkItemID, state.Data.AttemptID)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		if runtime.Issue.OrganizationID != state.Organization || runtime.Issue.ProjectID != state.Project || runtime.Issue.WorkItemID != state.Lease.WorkItemID || runtime.Attempt == nil || runtime.Attempt.AttemptID != state.Data.AttemptID || runtime.Attempt.LeaseID != state.Lease.ID || runtime.Attempt.FencingToken != state.Lease.FencingToken || runtime.Attempt.PolicyID != state.Data.PolicyID {
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
		return runner.NativeValidation{SecurityAudit: reviewevidence.AcceptedSecurityAudit(detail, version), Discussion: discussion, Version: &version, Diff: diff, Evidence: runtime.Attempt.Evidence, Commands: commands}, nil
	}
	return runner.NativeValidation{}, errors.New("native validator version is no longer current")
}
