package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ReconcileReleaseDelivery(ctx context.Context, localAttempt int64, generation uint64) (runner.ReleaseDeliveryResult, error) {
	delivery := e.claim.recovery.Issue.ReleaseDelivery
	if delivery == nil || e.conversationContinuation {
		return runner.ReleaseDeliveryResult{}, nil
	}
	if err := e.Validate(ctx); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	path, err := nativeItemPath(e.claim.lease.WorkItemID)
	if err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	var status tracker.ReleaseDeliveryStatus
	if err := e.claim.source.client.client.request(ctx, http.MethodGet, e.claim.source.client.base()+path+"/release-pick", nil, &status); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	if status.ReleasePickInput != delivery.ReleasePickInput || status.ReleaseHeadSHA == "" {
		return runner.ReleaseDeliveryResult{}, errors.New("release delivery verification identity mismatch")
	}
	if status.DeliveredSHA == "" && !status.Closed {
		return runner.ReleaseDeliveryResult{}, nil
	}
	identity := tracker.NativeExecutionIdentity{Role: runner.RoleMerge, Backend: "git", Model: "none"}
	if reservation := e.claim.lease.ProviderReservation; reservation != nil {
		identity.Role, identity.Backend, identity.Model = reservation.Role, reservation.Backend, reservation.Model
	}
	e.mu.Lock()
	if e.data.Identity != nil {
		identity = *e.data.Identity
	}
	e.mu.Unlock()
	if err := e.Start(ctx, identity); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	e.mu.Lock()
	e.role = runner.RoleMerge
	e.releaseDelivery = &status
	e.mu.Unlock()
	at := e.scheduler.now().UTC()
	runtimeIdentity := runner.NativeLandingIdentity(at)
	if err := e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: generation, Phase: "merging", HeartbeatAt: at, Identity: runtimeIdentity}); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	result := runner.RunResult{FinalState: runner.FinalStateCompleted, RuntimeIdentity: runtimeIdentity}
	if status.DeliveredSHA != "" {
		result.NativeLanding = &runner.NativeLanding{HeadSHA: delivery.SourceMergeSHA, BaseSHA: delivery.FrozenSHA, BaseRef: delivery.BaseRef, MergeSHA: status.DeliveredSHA, Landed: true}
		result.FinalMessage = fmt.Sprintf("Verified source commit %s on %s as %s at release head %s.", delivery.SourceMergeSHA, delivery.BaseRef, status.DeliveredSHA, status.ReleaseHeadSHA)
	} else {
		action := "The delivery PR is closed without merge and its source is absent from the release branch. Reopen the delivery PR or confirm that this release delivery should be cancelled."
		result.FinalMessage = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: " + strconv.Quote(action) + "\n```"
	}
	return runner.ReleaseDeliveryResult{Settled: true, Result: result}, nil
}

// LandingTarget names the reviewed version a landing run puts on the base
// branch: the item's Change Request, its current version, and the merge
// method of the policy that version was published under.
func (e *nativeExecution) LandingTarget(ctx context.Context) (runner.NativeLandingTarget, error) {
	if err := nativeEffectError(ctx); err != nil {
		return runner.NativeLandingTarget{}, err
	}
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
	if d := e.claim.recovery.Issue.ReleaseDelivery; d != nil {
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

// RecordLanding tells the hub the commit the base branch advanced to. The
// idempotency key is the attempt's, so a retried report records one landing.
func (e *nativeExecution) RecordLanding(ctx context.Context, landing runner.NativeLanding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recordLanding(ctx, landing)
}

func (e *nativeExecution) recordLanding(ctx context.Context, landing runner.NativeLanding) error {
	if e.claim.source == nil || e.claim.source.client == nil {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if !landing.Landed {
		return errors.New("only a landed version is recorded")
	}
	receipt := &tracker.NativeLandingReceipt{Incorporation: landing.Incorporation, ConflictFiles: landing.ConflictFiles, SecurityAudit: landing.SecurityAudit, Pipeline: landing.Pipeline, PipelineDropped: landing.PipelineDropped, Path: landing.Path, Packages: landing.Packages, ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, BaseSHA: landing.BaseSHA, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Gate: landing.Gate, Landed: true, Rebased: landing.Rebased}
	var objects []byte
	if landing.Incorporation != nil {
		objects = landing.Incorporation.Objects
	}
	_, err := e.claim.source.client.LandChangeVersion(ctx, e.claim.lease.WorkItemID, landing.ChangeID, landing.VersionID, tracker.LandChangeVersion{
		Mutation:             tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":landing", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		Receipt:              receipt,
		IncorporationObjects: objects,
		MergeSHA:             landing.MergeSHA, BaseRef: landing.BaseRef, Method: landing.Method, Rebased: landing.Rebased,
	})
	if err != nil {
		return err
	}
	e.landed = true
	return nil
}
