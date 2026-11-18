package landing

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	CurrentChange(context.Context, tracker.NativeWorkItemID) (tracker.ChangeDetail, error)
}

func Target(ctx context.Context, client Reader, item tracker.NativeWorkItemID, delivery *tracker.ReleaseDelivery) (runner.NativeLandingTarget, error) {
	detail, err := client.CurrentChange(ctx, item)
	if err != nil {
		return runner.NativeLandingTarget{}, err
	}
	if detail.Change.ID == "" {
		return runner.NativeLandingTarget{}, fmt.Errorf("%w: the item has no Change Request", runner.ErrLandingNotReviewed)
	}
	target := runner.NativeLandingTarget{ChangeID: detail.Change.ID, VersionID: detail.Change.CurrentVersion, Title: detail.Change.Title, Method: "squash", SourceIssues: detail.SourceIssues}
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion {
			target.HeadSHA, target.Number = version.HeadSHA, version.Number
			target.BaseSHA, target.SecurityAudit = version.BaseSHA, version.Policy.Gates.SecurityAudit
			target.Repository, target.GitHubPullRequest = version.Repository, version.Policy.Gates.GitHubPullRequest
			target.External = version.External
			target.Validation = version.Validation
			target.ReviewDispositions = version.ReviewDispositions
			if version.Landing != nil {
				target.ConflictFiles = version.Landing.ConflictFiles
				target.CI = version.Landing.CI
				target.RebaseRequired = version.Landing.RefusalKind == "conflict"
				target.MergeResolution = version.Landing.RefusalKind == "conflict" && version.Landing.Path == "merge_resolution_pending"
			}
			if version.Policy.Gates.MergeMethod != "" {
				target.Method = version.Policy.Gates.MergeMethod
			}
		}
	}
	if d := delivery; d != nil {
		if d.ChangeID != target.ChangeID || d.VersionID != target.VersionID || d.HeadSHA != target.HeadSHA || d.BaseSHA != target.BaseSHA || d.Repository != target.Repository {
			return target, fmt.Errorf("%w: release association differs from current version", runner.ErrLandingNotReviewed)
		}
		target.BaseRef, target.SourceMergeSHA = d.BaseRef, d.SourceMergeSHA
		target.GitHubPullRequest = true
	}
	if landed := detail.Change.CurrentLanding(); landed != nil {
		return target, fmt.Errorf("%w: the Change Request already landed as %s", runner.ErrLandingNotReviewed, landed.MergeSHA)
	}
	if target.HeadSHA == "" {
		return target, fmt.Errorf("%w: no version is published", runner.ErrLandingNotReviewed)
	}
	if detail.Summary.Status != "reviewed" {
		target.ReviewPending = true
		return target, fmt.Errorf("%w: the current version is %s", runner.ErrLandingNotReviewed, detail.Summary.Status)
	}
	return target, nil
}
