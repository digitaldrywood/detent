package hubclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// LandingTarget names the reviewed version a landing run puts on the base
// branch: the item's Change Request, its current version, and the merge
// method of the policy that version was published under.
func (e *nativeExecution) LandingTarget(ctx context.Context) (runner.NativeLandingTarget, error) {
	if e.claim.source == nil || e.claim.source.client == nil {
		return runner.NativeLandingTarget{}, runner.ErrExecutionAuthorityUnavailable
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	detail, err := client.currentChange(ctx, item)
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
			target.Repository, target.GitHubPullRequest = version.Repository, version.Policy.Gates.GitHubPullRequest
			target.External = version.External
			if version.Policy.Gates.MergeMethod != "" {
				target.Method = version.Policy.Gates.MergeMethod
			}
		}
	}
	if detail.Change.Landed != nil {
		return target, fmt.Errorf("%w: the Change Request already landed as %s", runner.ErrLandingNotReviewed, detail.Change.Landed.MergeSHA)
	}
	if target.HeadSHA == "" {
		return target, fmt.Errorf("%w: no version is published", runner.ErrLandingNotReviewed)
	}
	if detail.Summary.Status != "reviewed" {
		return target, fmt.Errorf("%w: the current version is %s", runner.ErrLandingNotReviewed, detail.Summary.Status)
	}
	return target, nil
}

// RecordLanding tells the hub the commit the base branch advanced to. The
// idempotency key is the attempt's, so a retried report records one landing.
func (e *nativeExecution) RecordLanding(ctx context.Context, landing runner.NativeLanding) error {
	if e.claim.source == nil || e.claim.source.client == nil {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if !landing.Landed {
		return errors.New("only a landed version is recorded")
	}
	_, err := e.claim.source.client.LandChangeVersion(ctx, e.claim.lease.WorkItemID, landing.ChangeID, landing.VersionID, tracker.LandChangeVersion{
		Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":landing", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method,
	})
	return err
}
