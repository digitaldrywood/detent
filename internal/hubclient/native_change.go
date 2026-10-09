package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// A hub-native item has no pull request, so a successful work run's commits
// reach review only through the item's native Change Request. The agent cannot
// open it: every work item mutation is fenced by the run's lease, which only
// the runner holds. The execution therefore opens it when the run finishes,
// under that lease, and reports what it found; the orchestrator moves the item
// (INV-1), so a run that changed something and a run that changed nothing both
// leave the dispatchable set instead of being continued again.

const maxNativeChangeTitle = 512

func (e *nativeExecution) ownsIssueCompletion() bool {
	return !e.conversationContinuation && (e.role == runner.RoleCode || e.role == runner.RoleRework)
}

func (e *nativeExecution) ownsChangeCompletion() bool {
	return e.ownsIssueCompletion() && e.worktreeState != "dirty" && (e.preparedDisposition == nil || e.preparedDisposition.Status != "blocked")
}

func (e *nativeExecution) settle(ctx context.Context, outcome string, finish int64) (diagnosticErr error) {
	done := e.diagnosticOperation("native.settle")
	defer func() { done(diagnosticErr) }()
	if e.settled || outcome != "succeeded" || !e.ownsChangeCompletion() {
		return nil
	}
	if err := nativeEffectError(ctx); err != nil {
		return err
	}
	if e.worktreeState != "clean" && e.worktreeState != "unpushed" {
		return errors.New("the run's final worktree checkpoint is unavailable")
	}
	if e.claim.source == nil || e.claim.source.client == nil {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if e.remaining() <= 0 {
		if err := e.renew(ctx); err != nil {
			return err
		}
	}
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.data.PolicyID); err != nil {
		return e.scheduler.nativeClaimError(string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	}
	if _, err := e.claim.source.client.ValidateLease(ctx, e.claim.lease); err != nil {
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
		Changed: diff.HeadSHA != "" && diff.HeadSHA != diff.BaseSHA && len(diff.Files) > 0,
		BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: len(diff.Files),
	}
	e.change = change
	e.finalizationSource, e.finalizationAttempt = nil, ""
	if !change.Changed {
		detail, err := e.claim.source.client.currentChange(ctx, e.claim.lease.WorkItemID)
		if err != nil {
			change.Error = err.Error()
			return err
		}
		if detail.Change.ID == "" {
			e.settled = true
			return nil
		}
		if err := e.requireRecoveredSource(detail, diff); err != nil {
			return err
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
			if version.PolicyID != e.data.PolicyID || detail.Summary.Status == "stale_policy" {
				change.VersionCode = "policy_mismatch"
			} else if detail.Summary.Status == "reviewed" && detail.Change.CurrentLanding() == nil &&
				diff.BaseSHA == diff.HeadSHA && len(diff.Files) == 0 && e.integrationSource != nil &&
				e.preparedDisposition != nil && e.preparedDisposition.Status == "complete" && !e.preparedDisposition.Blockers && !e.preparedDisposition.HumanAction {
				result, err := e.integrationSource(ctx, version, diff.BaseSHA)
				if err != nil {
					var refusal *workspace.LandRefusal
					if !errors.As(err, &refusal) {
						change.Error = err.Error()
						return err
					}
					change.VersionError += ": " + err.Error()
				} else {
					landing := runner.NativeLanding{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Landed: true, MergeSHA: result.MergeSHA, BaseRef: result.BaseRef, Method: result.Method, Rebased: result.Rebased}
					if err := e.RecordLanding(ctx, landing); err != nil {
						change.Error = err.Error()
						return err
					}
					change.VersionID, change.Reviewed, change.VersionError = version.ID, true, ""
					change.Landing = &landing
				}
			}
			e.settled = true
			return nil
		}
		change.Error = "the final attempt diff does not identify the current Change Request head"
		return errors.New(change.Error)
	}
	var err error
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
		change.VersionError, change.VersionCode, change.Reviewed = err.Error(), hubErrorCode(err), false
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
	detail, err := e.claim.source.client.Change(ctx, e.claim.lease.WorkItemID, change.ChangeID)
	if err != nil {
		return err
	}
	for _, version := range detail.Versions {
		if version.ID != change.VersionID {
			continue
		}
		if !version.Policy.Gates.GitHubPullRequest {
			return nil
		}
		authorize := func(ctx context.Context) error {
			if checkpoint := e.data.Handoff; checkpoint != nil && (checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") && checkpoint.ExternalEffect != "git_push" && checkpoint.ExternalEffect != "pr_create" {
				return fmt.Errorf("%w: PR publication has an unresolved external effect; preserve source and reconcile its checkpoint before resuming", ErrUnavailable)
			}
			if e.remaining() <= 0 {
				if err := e.renew(ctx); err != nil {
					return e.executionError(err)
				}
			}
			if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.data.PolicyID); err != nil {
				return e.executionError(e.scheduler.nativeClaimError(string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err))
			}
			if _, err := e.claim.source.client.ValidateLease(ctx, e.claim.lease); err != nil {
				return e.executionError(err)
			}
			current, err := e.claim.source.client.Change(ctx, e.claim.lease.WorkItemID, change.ChangeID)
			if err != nil {
				return err
			}
			if current.Change.CurrentVersion != version.ID || version.HeadSHA != change.HeadSHA || version.Repository != e.repository || version.PolicyID != e.data.PolicyID || current.Summary.Status == "stale_policy" || current.Change.CurrentLanding() != nil {
				return errors.New("PR publication source, current version or approved policy changed; reconcile the current Change before retrying")
			}
			if previous := e.claim.recovery.SourceAttempt(); previous != nil && previous.Checkpoint != nil &&
				(previous.Checkpoint.EffectState == "pending" || previous.Checkpoint.EffectState == "ambiguous") {
				checkpoint := previous.Checkpoint
				if e.role != runner.RoleRework || !checkpoint.UncertainForgeEffect() || e.recoveredSource == nil || previous.MachineID != e.claim.lease.MachineID ||
					previous.PolicyID != version.PolicyID || checkpoint.Change == nil || *checkpoint.Change != (tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}) ||
					checkpoint.HeadSHA != version.HeadSHA || checkpoint.Storage != "local_only" || checkpoint.Availability != "available" || e.lastDiff.HeadSHA != version.HeadSHA {
					return errors.New("uncertain publication requires unchanged verified current Rework source under its newly owned lease; preserve its checkpoint and reconcile the source, version and policy before retrying")
				}
			}
			return e.requireRecoveredSource(current, *e.lastDiff)
		}
		authorizeStart := func(ctx context.Context) error {
			if err := nativeEffectError(ctx); err != nil {
				return e.executionError(err)
			}
			if err := authorize(ctx); err != nil {
				return err
			}
			return nativeEffectError(ctx)
		}
		if err := authorizeStart(ctx); err != nil {
			return err
		}
		if e.publication != nil && e.publication.Matches(change.ChangeID, version) {
			return nil
		}
		if e.publicationSource == nil {
			return errors.New("required GitHub PR preparation is unavailable; retain the current Change source and resume on its authorized publication runner")
		}
		confirmedPR := workspace.GitHubPublication{}
		effect := func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
			check := authorize
			if state == "pending" {
				check = authorizeStart
			}
			if err := check(ctx); err != nil {
				return err
			}
			if publication.Repository != version.Repository || publication.HeadSHA != version.HeadSHA || publication.BaseRef == "" || publication.Branch == "" {
				return errors.New("publication effect differs from the current source repository, head, branch or target")
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: e.worktreeState, HeadSHA: version.HeadSHA}
			if e.data.Handoff != nil {
				checkpoint = *e.data.Handoff
			}
			binding := tracker.NativePRPublication{ChangeID: change.ChangeID, VersionID: version.ID, Repository: publication.Repository, HeadSHA: publication.HeadSHA, BaseRef: publication.BaseRef, Branch: publication.Branch, PolicyID: version.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: version.AttemptID, External: publication.External}
			if checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous" {
				unresolved := binding
				unresolved.External = tracker.ChangeExternalReference{}
				if checkpoint.EffectID != unresolved.EffectID(checkpoint.ExternalEffect) || checkpoint.Change == nil || *checkpoint.Change != binding.SourceVersion {
					return errors.New("unresolved publication effect differs from the current source identity; preserve source and reconcile its checkpoint before resuming")
				}
			}
			if state == "pending" && (checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") {
				return errors.New("PR publication has an unresolved external effect; reconcile the exact repository, branch, head and PR before retrying a write")
			}
			if state == "confirmed" && kind == "git_push" && checkpoint.ExternalEffect == "pr_create" {
				return nil
			}
			checkpoint.ExternalEffect, checkpoint.EffectState = kind, state
			checkpoint.EffectID = binding.EffectID(kind)
			checkpoint.Change = &tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			if state == "none" {
				checkpoint.ExternalEffect, checkpoint.EffectID = "none", ""
			}
			if err := e.append(ctx, "run.checkpointed", "", &checkpoint); err != nil {
				return err
			}
			if kind == "pr_create" && state == "confirmed" {
				confirmedPR = publication
			}
			if state == "pending" {
				return nativeEffectError(ctx)
			}
			return nil
		}
		publication, err := e.publicationSource(ctx, version, workspace.LandOptions{Authorize: authorizeStart, PublicationEffect: effect, External: version.External, SourceIssues: detail.SourceIssues})
		if err != nil {
			if e.data.Handoff != nil && e.data.Handoff.EffectState == "ambiguous" {
				return fmt.Errorf("%w: required GitHub PR has an ambiguous publication response; retain source and reconcile the exact existing PR before another write: %w", ErrUnavailable, err)
			}
			return fmt.Errorf("required GitHub PR is not ready; preserve source and resolve publication blocker: %w", err)
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		receipt := &tracker.NativePRPublication{ChangeID: change.ChangeID, VersionID: version.ID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: version.PolicyID,
			SourceVersion: tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: version.AttemptID, External: publication.External}
		if !receipt.Matches(change.ChangeID, version) || publication != confirmedPR || e.data.Handoff == nil || e.data.Handoff.ExternalEffect != "pr_create" || e.data.Handoff.EffectState != "confirmed" || e.data.Handoff.EffectID != receipt.EffectID("pr_create") {
			return errors.New("PR publication response lacks an exact verified identity and confirmed effect; reconcile publication before reporting success")
		}
		e.publication = receipt
		return nil
	}
	return errors.New("PR publication requires the current Change version; reload the source before retrying")
}

// publishChangeVersion reports the version that carries the head and whether
// the project's review policy already accepts it, which is what lets the
// run's completion send the item straight to landing.
func (e *nativeExecution) publishChangeVersion(ctx context.Context, changeID string, diff tracker.AttemptDiffRequest) (string, bool, error) {
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	detail, err := client.Change(ctx, item, changeID)
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
		if version.PolicyID != e.data.PolicyID || detail.Summary.Status == "stale_policy" {
			if !e.sourceRequired {
				return "", false, &APIError{Status: 409, Code: "policy_mismatch", Message: "the current head's version no longer matches the approved policy"}
			}
			break
		}
		return version.ID, detail.Summary.Status == "reviewed", nil
	}
	if diff.HeadSHA == diff.BaseSHA || len(diff.Files) == 0 {
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
	if err := nativeEffectError(ctx); err != nil {
		return "", false, err
	}
	version, err := client.PublishChangeVersion(ctx, item, changeID, tracker.PublishChangeVersion{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":version", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
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
	})
	if err != nil {
		return "", false, fmt.Errorf("publish version: %w", err)
	}
	return version.ID, acceptedOnPublish(version), nil
}

// acceptedOnPublish reports a freshly published version that its review
// policy accepts as it stands: nobody has to review it and no check has to
// report on it. It reads the policy the version was published under, so the
// answer does not depend on a second request succeeding.
func acceptedOnPublish(version tracker.ChangeVersion) bool {
	return !version.Policy.Gates.Validator && version.ReviewPolicy.ID != "" && !version.ReviewPolicy.RequireReview && len(version.ReviewPolicy.RequiredChecks) == 0
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
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	existing, err := client.Changes(ctx, item)
	if err != nil {
		return "", fmt.Errorf("list changes: %w", err)
	}
	if len(existing) > 0 {
		return existing[len(existing)-1].ID, nil
	}
	if err := nativeEffectError(ctx); err != nil {
		return "", err
	}
	created, err := client.CreateChange(ctx, item, tracker.CreateChange{
		Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":change", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		Title:    nativeChangeTitle(e.claim.recovery.Issue.Title, item),
		Body:     nativeChangeBody(e.data.AttemptID, diff),
	})
	if err != nil {
		return "", fmt.Errorf("create change: %w", err)
	}
	return created.ID, nil
}

func nativeChangeTitle(title string, item tracker.NativeWorkItemID) string {
	if title == "" {
		title = string(item)
	}
	if len(title) <= maxNativeChangeTitle {
		return title
	}
	cut := maxNativeChangeTitle
	for cut > 0 && title[cut]&0xC0 == 0x80 {
		cut--
	}
	return title[:cut]
}

func nativeChangeBody(attempt string, diff tracker.AttemptDiffRequest) string {
	return fmt.Sprintf("Opened by the runner when attempt %s succeeded.\n\nBase: %s\nHead: %s\nFiles changed: %d\n\nThe change is the attempt's stored diff.",
		attempt, diff.BaseSHA, diff.HeadSHA, len(diff.Files))
}

// nativeLeaseLost reports whether the hub refused a mutation because the lease
// that fenced it is no longer the current one.
func nativeLeaseLost(err error) bool {
	if claimLost(err) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Code == "stale_execution"
}

func hubErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.Code
	}
	return ""
}
