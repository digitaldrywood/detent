package publication

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/forgeavailability"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type Authority interface {
	Authorize(context.Context) error
	ExecutionError(context.Context, error) error
	Checkpoint(context.Context, tracker.NativeCheckpoint) error
	CheckSource(tracker.ChangeDetail, tracker.AttemptDiffRequest) error
}
type Result struct{ Receipt *tracker.NativePRPublication }

type State struct {
	Client          *api.NativeClient
	Lease           tracker.NativeLease
	Recovery        tracker.NativeRecovery
	Policy          policy.Descriptor
	Data            *tracker.NativeRunData
	RecoveredSource *tracker.NativeChangeReference
	Repository      string
	Role            string
	Diff            func() *tracker.AttemptDiffRequest
	Publication     *tracker.NativePRPublication
	WorktreeState   string
	Publish         func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
}

func Prepare(ctx context.Context, host Authority, executionState State, change *runner.NativeChange) (Result, error) {
	detail, err := executionState.Client.RuntimeChange(ctx, executionState.Lease.WorkItemID, change.ChangeID)
	if err != nil {
		return Result{}, err
	}
	for _, version := range detail.Versions {
		if version.ID != change.VersionID {
			continue
		}
		if !version.Policy.Gates.GitHubPullRequest {
			return Result{}, nil
		}
		pinned := executionState.Policy
		unresolvedAtStart := executionState.Data.Handoff.UncertainForgeEffect()
		reconcileStale := unresolvedAtStart && executionState.RecoveredSource != nil &&
			*executionState.RecoveredSource == (tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}) &&
			pinned.ID == executionState.Data.PolicyID && pinned.Gates.GitHubPullRequest
		authorize := func(ctx context.Context) error {
			if checkpoint := executionState.Data.Handoff; checkpoint != nil && (checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") && checkpoint.ExternalEffect != "git_push" && checkpoint.ExternalEffect != "pr_create" {
				return &workspace.LandRefusal{Kind: workspace.LandRefusalHeadMoved, Reason: "PR publication has an unresolved " + checkpoint.ExternalEffect + " effect; preserve source and reconcile its checkpoint before resuming"}
			}
			if err := host.Authorize(ctx); err != nil {
				return err
			}

			current, err := executionState.Client.RuntimeChange(ctx, executionState.Lease.WorkItemID, change.ChangeID)
			if err != nil {
				return err
			}
			if current.Change.CurrentVersion != version.ID || version.HeadSHA != change.HeadSHA || version.Repository != executionState.Repository || current.Summary.Status == "stale_policy" && !reconcileStale || current.Change.CurrentLanding() != nil {
				return errors.New("PR publication source, current version or review requirements changed; reconcile the current Change before retrying")
			}
			// Inherited effects need restored source proof. Effects started by
			// this execution already passed publication authorization.
			if previous := executionState.Recovery.SourceAttempt(); previous != nil && previous.Checkpoint.UncertainForgeEffect() && executionState.Data.Handoff.UncertainForgeEffect() {
				checkpoint := executionState.Data.Handoff
				if executionState.Role != runner.RoleCode && executionState.Role != runner.RoleRework || !checkpoint.UncertainForgeEffect() || executionState.RecoveredSource == nil ||
					checkpoint.Change == nil || *checkpoint.Change != (tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}) ||
					checkpoint.HeadSHA != version.HeadSHA || checkpoint.Storage != "local_only" && !checkpoint.GitRef() || checkpoint.Availability != "available" || executionState.Diff().HeadSHA != version.HeadSHA {
					return errors.New("uncertain publication requires unchanged verified current source under its newly owned lease; preserve its checkpoint and reconcile the source, version and policy before retrying")
				}
			}
			return host.CheckSource(current, *executionState.Diff())
		}
		authorizeStart := func(ctx context.Context) error {
			if err := api.MutationEffectError(ctx); err != nil {
				return host.ExecutionError(ctx, err)
			}
			if err := authorize(ctx); err != nil {
				return err
			}
			return api.MutationEffectError(ctx)
		}
		if err := authorizeStart(ctx); err != nil {
			return Result{}, err
		}
		if executionState.Publication != nil && executionState.Publication.Matches(change.ChangeID, version) {
			return Result{}, nil
		}
		if executionState.Publish == nil {
			return Result{}, errors.New("required GitHub PR preparation is unavailable; retain the current Change source and resume on its authorized publication runner")
		}
		confirmedPR := workspace.GitHubPublication{}
		external, externalHead := version.External, version.HeadSHA
		var previousNumber int64
		if external == nil {
			for _, previous := range detail.Versions {
				if detail.Change.Landed != nil && previous.ID == detail.Change.Landed.VersionID && previous.Number < version.Number {
					previousNumber = previous.Number
				}
			}
			for _, previous := range detail.Versions {
				if previous.Number < version.Number && previous.Number > previousNumber && previous.Repository == version.Repository && previous.External != nil && (previous.Landing == nil || !previous.Landing.Landed) {
					external, externalHead, previousNumber = previous.External, previous.HeadSHA, previous.Number
				}
			}
		}
		var pushes []workspace.GitHubPublicationPush
		if external != nil && version.External == nil {
			for _, previous := range detail.Versions {
				if previous.Number <= previousNumber || previous.Number >= version.Number || previous.Repository != version.Repository || previous.External != nil || previous.AttemptID == "" || !previous.Policy.Gates.GitHubPullRequest || previous.Landing != nil && previous.Landing.Landed {
					continue
				}
				ref := tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: previous.ID, HeadSHA: previous.HeadSHA}
				for _, push := range detail.PublicationPushes {
					if push.Change != ref {
						continue
					}
					pushes = append(pushes, workspace.GitHubPublicationPush{EffectID: push.EffectID, Publication: tracker.NativePRPublication{
						ChangeID: change.ChangeID, VersionID: previous.ID, Repository: previous.Repository, HeadSHA: previous.HeadSHA, PolicyID: previous.PolicyID,
						SourceVersion: ref, SourceAttemptID: previous.AttemptID, External: *external,
					}})
				}
			}
		}
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
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: executionState.WorktreeState, HeadSHA: version.HeadSHA}
			if executionState.Data.Handoff != nil {
				checkpoint = *executionState.Data.Handoff
			}
			binding := tracker.NativePRPublication{ChangeID: change.ChangeID, VersionID: version.ID, Repository: publication.Repository, HeadSHA: publication.HeadSHA, BaseRef: publication.BaseRef, Branch: publication.Branch, PolicyID: version.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: version.AttemptID, External: publication.External}
			if checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous" {
				unresolved := binding
				unresolved.External = tracker.ChangeExternalReference{}
				if checkpoint.EffectID != unresolved.EffectID(checkpoint.ExternalEffect) || checkpoint.Change == nil || *checkpoint.Change != binding.SourceVersion {
					return &workspace.LandRefusal{Kind: workspace.LandRefusalHeadMoved, Reason: "unresolved publication effect differs from the current source identity; preserve source and reconcile its checkpoint before resuming"}
				}
				if state == "pending" && !unresolvedAtStart {
					return errors.New("PR publication has an unresolved external effect; reconcile the exact repository, branch, head and PR before retrying a write")
				}
			}
			if state == "confirmed" && kind == "git_push" && checkpoint.ExternalEffect == "pr_create" {
				return nil
			}
			if kind == "git_push" && state == "pending" {
				checkpoint.ExpectedHeadSHA = publication.ExpectedHeadSHA
			}
			checkpoint.ExternalEffect, checkpoint.EffectState = kind, state
			checkpoint.Publication = nil
			checkpoint.EffectID = binding.EffectID(kind)
			checkpoint.Change = &tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			if state == "none" {
				checkpoint.ExternalEffect, checkpoint.EffectID, checkpoint.ExpectedHeadSHA = "none", "", ""
			}
			if err := host.Checkpoint(ctx, checkpoint); err != nil {
				return err
			}
			if kind == "pr_create" && state == "confirmed" {
				confirmedPR = publication
			}
			if state == "pending" {
				return api.MutationEffectError(ctx)
			}
			return nil
		}
		expectedHead := ""
		if executionState.Data.Handoff != nil {
			expectedHead = executionState.Data.Handoff.ExpectedHeadSHA
		}
		publication, err := executionState.Publish(ctx, version, workspace.LandOptions{Authorize: authorizeStart, PublicationEffect: effect, External: external, ExternalHeadSHA: externalHead, PublicationPushes: pushes, SourceIssues: detail.SourceIssues, UnresolvedEffect: unresolvedAtStart, ExpectedPublicationHead: expectedHead})
		if err != nil {
			var refusal *workspace.LandRefusal
			if errors.Is(err, forgeavailability.ErrUnavailable) || !errors.As(err, &refusal) && executionState.Data.Handoff.UncertainForgeEffect() {
				return Result{}, fmt.Errorf("%w: required GitHub PR has an ambiguous publication response; the completion retry reconciles it against the forge before another write: %w", api.ErrUnavailable, err)
			}
			return Result{}, fmt.Errorf("required GitHub PR is not ready; preserve source and resolve publication blocker: %w", err)
		}
		if err := authorize(ctx); err != nil {
			return Result{}, err
		}
		receipt := &tracker.NativePRPublication{ChangeID: change.ChangeID, VersionID: version.ID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: version.PolicyID,
			SourceVersion: tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: version.AttemptID, External: publication.External}
		if !receipt.Matches(change.ChangeID, version) || publication != confirmedPR || executionState.Data.Handoff == nil || executionState.Data.Handoff.ExternalEffect != "pr_create" || executionState.Data.Handoff.EffectState != "confirmed" || executionState.Data.Handoff.EffectID != receipt.EffectID("pr_create") {
			return Result{}, errors.New("PR publication response lacks an exact verified identity and confirmed effect; reconcile publication before reporting success")
		}
		checkpoint := *executionState.Data.Handoff
		checkpoint.Publication = receipt
		if err := host.Checkpoint(ctx, checkpoint); err != nil {
			return Result{}, err
		}
		return Result{Receipt: receipt}, nil
	}
	return Result{}, errors.New("PR publication requires the current Change version; reload the source before retrying")
}
