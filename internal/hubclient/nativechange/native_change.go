package nativechange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/changecreation"
	"github.com/digitaldrywood/detent/internal/hubclient/publication"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (e *nativeExecution) ownsIssueCompletion() bool {
	return !e.conversationContinuation && (e.role == runner.RoleCode || e.role == runner.RoleRework)
}

func (e *nativeExecution) ownsChangeCompletion() bool {
	return (e.ownsIssueCompletion() || e.mergeResolution || e.change != nil && e.change.NoChange != nil) && e.worktreeState != "dirty" && (e.preparedDisposition == nil || e.preparedDisposition.Status != "blocked")
}

func (e *nativeExecution) PrepareMergeResolution() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.role != runner.RoleMerge || e.recoveredSource == nil {
		return errors.New("merge resolution requires recovered reviewed source")
	}
	e.mergeResolution = true
	return nil
}

func (e *nativeExecution) settle(ctx context.Context, outcome string, finish int64) (diagnosticErr error) {
	done := e.diagnosticOperation("native.settle")
	defer func() { done(diagnosticErr) }()
	if e.settled || outcome != "succeeded" || !e.ownsChangeCompletion() {
		return nil
	}
	if err := api.MutationEffectError(ctx); err != nil {
		return err
	}
	if e.worktreeState != "clean" && e.worktreeState != "unpushed" {
		return errors.New("the run's final worktree checkpoint is unavailable")
	}
	if e.claim.source == nil || e.claim.source.NativeClient() == nil {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if e.remaining() <= 0 {
		if err := e.renew(ctx, 0); err != nil {
			return err
		}
	}
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.data.PolicyID); err != nil {
		return e.scheduler.nativeClaimError(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	}
	if _, err := e.claim.source.NativeClient().ValidateLease(ctx, e.claim.lease); err != nil {
		return err
	}
	if e.lastDiff == nil {
		return errors.New("the run's final attempt diff is unavailable")
	}
	if e.storedSeq != finish {
		return errors.New("the hub has not stored the run's final attempt diff")
	}
	diff := *e.lastDiff
	if e.worktreeHead != "" && diff.HeadSHA != e.worktreeHead {
		return errors.New("the attempt diff does not match the final checkpoint head")
	}
	change := &runner.NativeChange{
		MergeResolution: e.mergeResolution,
		Changed:         diff.HeadSHA != "" && diff.HeadSHA != diff.BaseSHA && (len(diff.Files) > 0 || e.verifiedEmptyRework(diff)),
		BaseSHA:         diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: len(diff.Files),
	}
	e.change = change
	e.finalizationSource, e.finalizationAttempt = nil, ""
	detail, err := e.claim.source.NativeClient().CurrentChange(ctx, e.claim.lease.WorkItemID)
	if err != nil {
		change.Error = err.Error()
		return err
	}
	if e.prepareNoChange(diff, detail) {
		e.settled = true
		return nil
	}
	if err := e.requireRecoveredSource(detail, diff); err != nil {
		return err
	}
	if delivered, err := e.settleIntegratedChange(ctx, detail, diff, change); err != nil {
		change.Error = err.Error()
		return err
	} else if delivered {
		e.settled = true
		return nil
	}
	if !change.Changed {
		if detail.Change.ID == "" {
			e.settled = true
			return nil
		}
		for _, version := range detail.Versions {
			if version.ID != detail.Change.CurrentVersion {
				continue
			}
			e.finalizationSource = &tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			e.finalizationAttempt = version.AttemptID
			change.Changed, change.ChangeID, change.BaseSHA, change.HeadSHA = true, detail.Change.ID, version.BaseSHA, version.HeadSHA
			if version.HeadSHA == diff.HeadSHA {
				if err := e.publishVersion(ctx, diff, change); err != nil {
					return err
				}
				e.settled = true
				return nil
			}
			change.VersionError = "the final attempt diff does not identify the current Change Request head"
			if detail.Summary.Status == "stale_policy" {
				change.VersionCode = "policy_mismatch"
			}
			e.settled = true
			return nil
		}
		change.Error = "the final attempt diff does not identify the current Change Request head"
		return errors.New(change.Error)
	}
	for try := range nativeChangeCreateTries {
		if try > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(nativeChangeCreateBackoff):
			}
		}
		if ctx.Err() != nil {
			break
		}
		var id string
		if id, err = e.openChange(ctx, diff); err == nil {
			change.ChangeID, change.Error = id, ""
			if err := e.publishVersion(ctx, diff, change); err != nil {
				return err
			}
			e.settled = true
			return nil
		}
		if e.remaining() <= 0 || nativeLeaseLost(err) {
			break
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	change.Error = errorText(err)
	if err == nil {
		return errors.New(change.Error)
	}
	return err
}

func (e *nativeExecution) settleIntegratedChange(ctx context.Context, detail tracker.ChangeDetail, diff tracker.AttemptDiffRequest, change *runner.NativeChange) (bool, error) {
	if detail.Summary.Status != "reviewed" || detail.Change.CurrentLanding() != nil || e.integrationSource == nil ||
		e.preparedDisposition != nil && (e.preparedDisposition.Blockers || e.preparedDisposition.HumanAction) {
		return false, nil
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion || version.PolicyID != e.data.PolicyID {
			continue
		}
		if d := e.claim.recovery.Issue.ReleaseDelivery; d != nil && (d.ChangeID != detail.Change.ID || d.VersionID != version.ID || d.HeadSHA != version.HeadSHA || d.BaseSHA != version.BaseSHA || d.Repository != version.Repository) {
			return false, errors.New("release association differs from current version")
		}
		result, err := e.integrationSource(ctx, version, diff.HeadSHA)
		if err != nil {
			var refusal *workspace.LandRefusal
			if errors.As(err, &refusal) {
				if change.Changed || diff.HeadSHA == version.HeadSHA {
					return false, nil
				}
				landing := &runner.NativeLanding{
					ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA,
					BaseSHA: refusal.BaseSHA, BaseRef: refusal.TargetBranch,
					RefusalKind: refusal.Kind, Refusal: refusal.Reason, ConflictFiles: refusal.ConflictFiles,
				}
				if refusal.Kind == workspace.LandRefusalConflict {
					landing.Path, landing.Waiting = "merge_resolution_pending", true
				}
				e.finalizationSource = &tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
				e.finalizationAttempt = version.AttemptID
				change.Changed, change.ChangeID, change.VersionID = true, detail.Change.ID, version.ID
				change.BaseSHA, change.HeadSHA, change.Reviewed, change.Landing = version.BaseSHA, version.HeadSHA, true, landing
				return true, nil
			}
			return false, fmt.Errorf("verify delivered source: %w", err)
		}
		if version.Policy.Gates.SecurityAudit && !version.Policy.Gates.GitHubPullRequest && e.claim.recovery.Issue.ReleaseDelivery == nil {
			return false, nil
		}
		landing := runner.NativeLanding{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Landed: true, MergeSHA: result.MergeSHA, BaseRef: result.BaseRef, Method: result.Method, Rebased: result.Rebased}
		if err := e.recordLanding(ctx, landing); err != nil {
			return false, err
		}
		e.finalizationSource = &tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
		e.finalizationAttempt = version.AttemptID
		change.Changed, change.ChangeID, change.VersionID = true, detail.Change.ID, version.ID
		change.BaseSHA, change.HeadSHA, change.Reviewed, change.Landing = version.BaseSHA, version.HeadSHA, true, &landing
		return true, nil
	}
	return false, nil
}

func (e *nativeExecution) SetIntegrationSource(source func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.integrationSource = source
}

func (e *nativeExecution) publishVersion(ctx context.Context, diff tracker.AttemptDiffRequest, change *runner.NativeChange) (diagnosticErr error) {
	done := e.diagnosticOperation("native.publishVersion")
	defer func() { done(diagnosticErr) }()
	id, reviewed, err := e.publishChangeVersion(ctx, change.ChangeID, diff)
	if err != nil {
		change.VersionID, change.VersionError, change.VersionCode, change.Reviewed = "", err.Error(), hubErrorCode(err), false
		return err
	}
	change.VersionID, change.VersionError, change.VersionCode, change.Reviewed = id, "", "", reviewed
	if err := e.preparePRPublication(ctx, change); err != nil {
		change.VersionError, change.VersionCode, change.Reviewed = err.Error(), publicationErrorCode(err), false
		return err
	}
	return nil
}

func (e *nativeExecution) SetPublicationSource(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.publicationSource = source
}

func (e *nativeExecution) preparePRPublication(ctx context.Context, change *runner.NativeChange) error {
	result, err := publication.Prepare(ctx, publicationAuthority{e}, publication.State{Client: e.claim.source.NativeClient(), Lease: e.claim.lease, Recovery: e.claim.recovery, Policy: e.scheduler.host.PinnedExecutionPolicy(string(e.claim.lease.WorkItemID)), Data: &e.data, RecoveredSource: e.recoveredSource, Repository: e.repository, Role: e.role, Diff: func() *tracker.AttemptDiffRequest { return e.lastDiff }, Publication: e.publication, WorktreeState: e.worktreeState, Publish: e.publicationSource}, change)
	if result.Receipt != nil {
		e.publication = result.Receipt
	}
	return err
}

type publicationAuthority struct{ execution *nativeExecution }

func (a publicationAuthority) Authorize(ctx context.Context) error {
	e := a.execution
	if e.remaining() <= 0 {
		if err := e.renew(ctx, 0); err != nil {
			return e.executionError(ctx, err)
		}
	}
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.data.PolicyID); err != nil {
		return e.executionError(ctx, e.scheduler.nativeClaimError(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err))
	}
	if _, err := e.claim.source.NativeClient().ValidateLease(ctx, e.claim.lease); err != nil {
		return e.executionError(ctx, err)
	}
	return nil
}
func (a publicationAuthority) ExecutionError(ctx context.Context, err error) error {
	return a.execution.executionError(ctx, err)
}
func (a publicationAuthority) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	return a.execution.append(ctx, "run.checkpointed", "", &checkpoint)
}
func (a publicationAuthority) CheckSource(detail tracker.ChangeDetail, diff tracker.AttemptDiffRequest) error {
	return a.execution.requireRecoveredSource(detail, diff)
}

// publishChangeVersion reports the version that carries the head and whether
// the project's review policy already accepts it, which is what lets the
// run's completion send the item straight to landing.
func (e *nativeExecution) publishChangeVersion(ctx context.Context, changeID string, diff tracker.AttemptDiffRequest) (string, bool, error) {
	if err := e.flushObservations(ctx); err != nil {
		return "", false, err
	}
	client, item := e.claim.source.NativeClient(), e.claim.lease.WorkItemID
	detail, err := client.RuntimeChange(ctx, item, changeID)
	if err != nil {
		return "", false, fmt.Errorf("read change: %w", err)
	}
	if err := e.requireRecoveredSource(detail, diff); err != nil {
		return "", false, err
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion || version.HeadSHA != diff.HeadSHA {
			continue
		}

		if detail.Summary.Status == "stale_policy" {
			if e.data.Handoff.UncertainForgeEffect() {
				if err := e.preparePRPublication(ctx, &runner.NativeChange{ChangeID: changeID, VersionID: version.ID, HeadSHA: version.HeadSHA}); err != nil {
					return "", false, err
				}
			}
			break
		}

		// Imported versions have no implementing attempt. Native reviews need
		// that recorded session, so publish this execution's validated source
		// rather than repeatedly auditing an unreviewable imported version.
		if version.AttemptID == "" && detail.Summary.NativeReview != "changes_requested" && (version.Policy.Gates.SecurityAudit || version.Policy.Gates.Validator || version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired) {
			break
		}
		e.recoveredSource = &tracker.NativeChangeReference{ChangeID: changeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
		return version.ID, detail.Summary.Status == "reviewed", nil
	}
	if diff.HeadSHA == diff.BaseSHA || len(diff.Files) == 0 && !e.verifiedEmptyRework(diff) {
		return "", false, errors.New("the final attempt diff does not identify a changed source head to publish")
	}
	if e.repository == "" {
		return "", false, errors.New("the checkout's origin remote is not an https repository the version can name")
	}
	digest := sha256.Sum256([]byte(diff.HeadSHA))
	var source *tracker.ChangeSource
	var bundle []byte
	if err := e.retainChangeSource(ctx, diff); err != nil {
		return "", false, fmt.Errorf("retain finalized Change source: %w", err)
	}
	if e.sourceRequired {
		source, bundle = &e.retainedSource.Source, e.retainedSource.Bundle
	}
	if err := api.MutationEffectError(ctx); err != nil {
		return "", false, err
	}
	request := tracker.PublishChangeVersion{
		Mutation:          tracker.Mutation{LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: detail.Change.CurrentVersion,
		SourceBundle:      bundle,
		ChangeVersionInput: tracker.ChangeVersionInput{
			Validation: e.validation,
			BaseSHA:    diff.BaseSHA, HeadSHA: diff.HeadSHA, MergeBaseSHA: diff.BaseSHA,
			Repository: e.repository,
			Code:       tracker.ChangeArtifact{Kind: "code", URI: e.repository + "/commit/" + diff.HeadSHA, SHA256: hex.EncodeToString(digest[:]), Availability: "unverified"},
			Artifacts:  []tracker.ChangeArtifact{},
			RunID:      e.data.RunID, AttemptID: e.data.AttemptID, PolicyID: e.data.PolicyID,
			Source: source,
		},
	}
	if signal, ok := workpad.SignalFromComment(e.preparedMessage, "", ""); ok && signal.Invalid == nil && signal.Status == workpad.StatusComplete && signal.HumanAction == "" && len(signal.Blockers) == 0 {
		for _, d := range signal.ReviewDispositions {
			request.ReviewDispositions = append(request.ReviewDispositions, tracker.ReviewDisposition{HeadSHA: diff.HeadSHA, ThreadID: d.ThreadID, Disposition: d.Disposition, Evidence: d.Evidence})
		}
	}
	if err := e.retainChangeValidation(ctx, &request.ChangeVersionInput); err != nil {
		return "", false, err
	}
	identity, err := json.Marshal(struct {
		Project tracker.ProjectID
		Item    tracker.NativeWorkItemID
		Change  string
		Request tracker.PublishChangeVersion
	}{client.ProjectID(), item, changeID, request})
	if err != nil {
		return "", false, fmt.Errorf("identify version publication: %w", err)
	}
	retryDigest := sha256.Sum256(identity)
	request.IdempotencyKey = "version:" + hex.EncodeToString(retryDigest[:])
	version, err := client.PublishChangeVersion(ctx, item, changeID, request)
	if err != nil {
		return "", false, fmt.Errorf("publish version: %w", err)
	}
	e.recoveredSource = &tracker.NativeChangeReference{ChangeID: changeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
	return version.ID, acceptedOnPublish(version), nil
}

// acceptedOnPublish reports a freshly published version that its review
// policy accepts as it stands: nobody has to review it and no check has to
// report on it. It reads the policy the version was published under, so the
// answer does not depend on a second request succeeding.
func acceptedOnPublish(version tracker.ChangeVersion) bool {
	return version.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired && !version.Policy.Gates.Validator && !version.Policy.Gates.SecurityAudit && version.ReviewPolicy.ID != "" && !version.ReviewPolicy.RequireReview && len(version.ReviewPolicy.RequiredChecks) == 0
}

// SetRepository names the repository a published version refers to.
func (e *nativeExecution) SetRepository(repository string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repository = repository
}

const (
	nativeChangeCreateTries   = 3
	nativeChangeCreateBackoff = 250 * time.Millisecond
)

func errorText(err error) string {
	if err == nil {
		return "the Change Request could not be opened"
	}
	return err.Error()
}

// openChange returns the item's Change Request, opening one when the item has
// none. A rework run's commits belong to the change the item already has.
// The idempotency key is the attempt's, so a retried open cannot open two.
func (e *nativeExecution) openChange(ctx context.Context, diff tracker.AttemptDiffRequest) (string, error) {
	return changecreation.Open(ctx, e.claim.source.NativeClient(), changecreation.Input{Item: e.claim.lease.WorkItemID, AttemptID: e.data.AttemptID, Title: e.claim.recovery.Issue.Title, Lease: e.claim.lease, Diff: diff})
}

func publicationErrorCode(err error) string {
	var refusal *workspace.LandRefusal
	if errors.As(err, &refusal) && refusal.Kind == workspace.LandRefusalHeadMoved {
		return refusal.Kind
	}
	return hubErrorCode(err)
}
