package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func NativeLandingIdentity(at time.Time) agentidentity.Identity {
	identity := agentidentity.RuntimeUpdate("none", "none", "none", "", at)
	identity.Role, identity.BackendID, identity.BackendKind = RoleMerge, "git", "git"
	return identity
}

// landNativeChange is a hub-native landing run. There is no agent in it: the
// reviewed head is combined with the base branch by the runner's own git and
// pushed with the runner's own credentials, and the hub records the commit
// the base branch advanced to. A refusal (a head that moved, a conflict, a
// base branch the forge protects) is reported on the run for the
// orchestrator to hand back to review with its reason; only an
// infrastructure failure fails the run.
func (r *Runner) landNativeChange(ctx context.Context, req RunRequest, landing LandingExecution, backend workspace.Backend, info workspace.Info, issue workspace.Issue, policy workerGitHubPolicy, prepared *NativeLandingTarget) (runResult RunResult, runErr error) {
	if req.Execution != nil {
		if err := req.Execution.Validate(ctx); err != nil {
			return RunResult{}, err
		}
	}
	target, err := landing.LandingTarget(ctx)
	if err != nil {
		if errors.Is(err, ErrLandingNotReviewed) {
			return r.refusedLanding(req, target, workspace.LandRefusalNothing, err.Error()), nil
		}
		return RunResult{}, fmt.Errorf("resolve landing target: %w", err)
	}
	if prepared != nil {
		previous := *prepared
		if previous.ChangeID != target.ChangeID || previous.VersionID != target.VersionID || previous.HeadSHA != target.HeadSHA || previous.Repository != target.Repository || previous.Method != target.Method || previous.GitHubPullRequest != target.GitHubPullRequest || !sameLandingExternal(previous.External, target.External) {
			return r.refusedLanding(req, previous, workspace.LandRefusalHeadMoved, "the reviewed version changed during workspace preparation"), nil
		}
	}
	lander, ok := backend.(workspace.Lander)
	if !ok {
		return RunResult{}, errors.New("workspace backend cannot land changes")
	}
	message := target.Title
	if message == "" {
		message = "Land " + target.HeadSHA
	}
	message += fmt.Sprintf("\n\nChange Request %s, round %d, head %s.", target.ChangeID, target.Number, target.HeadSHA)
	if target.GitHubPullRequest {
		message = tracker.AppendGitHubIssueClosingReferences(message, target.SourceIssues)
	}
	workflow, _, _, _ := r.runtimeSnapshot()
	effectiveGate := gate.Effective(workflow.Config.Gate)
	options := workspace.LandOptions{LandingMode: effectiveGate.LandingMode, Native: !target.GitHubPullRequest, Validation: target.Validation, RebaseRequired: target.RebaseRequired, RequiredStatusChecks: effectiveGate.RequiredStatusChecks, CITriggerLabel: effectiveGate.CITriggerLabel, PreviousCI: target.CI, ValidationCommand: effectiveGate.Run, HeadSHA: target.HeadSHA, Method: target.Method, Message: message, PushAttemptBranch: true, Repository: target.Repository, External: target.External, SourceIssues: target.SourceIssues}
	if effectiveGate.CITriggerLabelStaggerSeconds != nil {
		options.CITriggerLabelStagger = time.Duration(*effectiveGate.CITriggerLabelStaggerSeconds) * time.Second
	}
	if effectiveGate.LandingMode == gate.LandingRollingBarrier {
		options.Authorize = req.Execution.Validate
	}
	var result workspace.LandResult
	defer func() {
		if runResult.NativeLanding != nil {
			if result.Gate.Command == "" && options.Validation != nil && options.Validation != target.Validation {
				result.Gate = *options.Validation
			}
			runResult.NativeLanding.Path, runResult.NativeLanding.Packages = result.Path, result.Packages
			runResult.NativeLanding.CI = result.CI
			runResult.NativeLanding.Rebased = result.Rebased
			if result.Gate.Command != "" {
				runResult.NativeLanding.Gate = &result.Gate
			}
		}
	}()
	if target.GitHubPullRequest {
		scope := connector.RESTScopeFromContext(ctx)
		if scope == nil {
			started := time.Now()
			scope = &connector.RESTScope{Name: "native_landing", ProjectID: r.projectID}
			ctx = connector.WithRESTScope(ctx, scope)
			defer func() { runResult.GitHubScope = nativeGitHubScope(scope, started) }()
		}
		scope.Set("merging", "land")
		githubLander, supported := backend.(workspace.GitHubPRLander)
		if !supported {
			return RunResult{}, errors.New("workspace backend cannot land through a GitHub pull request")
		}
		if issue.Landing != nil {
			options.GitHubClient = issue.Landing.GitHubClient
		}
		if options.GitHubClient == nil {
			client, resolvedPolicy, clientErr := r.nativeLandingGitHubClient(ctx, req, policy)
			if clientErr != nil {
				return RunResult{}, clientErr
			}
			options.GitHubClient = client
			defer func() {
				usage := client.FlushRESTRateLimitUsage()
				runResult.GitHubRESTUsage = &usage
				runResult.GitHubRESTConsumer = resolvedPolicy.budgetConsumer()
			}()
		}
		result, err = githubLander.LandChangeViaGitHub(ctx, info, issue, options)
	} else if options.Validation, err = validateLandingHead(ctx, backend, info, issue, options); err == nil {
		batchLander, batched := backend.(workspace.BatchLander)
		remoteBatch, remoteBatched := req.Execution.(BatchLandingExecution)
		remoteBatched = remoteBatched && remoteBatch.BatchLandingEnabled()
		if target.RebaseRequired && req.LandingBatch != nil {
			req.LandingBatch.Finish()
			result, err = lander.LandChange(ctx, info, issue, options)
		} else if batched && remoteBatched {
			if target.RebaseRequired {
				if err = remoteBatch.FinishLandingBatch(ctx); err == nil {
					result, err = lander.LandChange(ctx, info, issue, options)
				}
			} else {
				result, err = remoteBatch.LandBatch(ctx, batchLander, workspace.LandRequest{Info: info, Issue: issue, Options: options})
			}
		} else if batched && req.LandingBatch != nil {
			result, err = req.LandingBatch.Land(batchLander, workspace.LandRequest{Info: info, Issue: issue, Options: options, Validate: func(batchCtx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := req.Execution.Validate(batchCtx); err != nil {
					return err
				}
				current, err := landing.LandingTarget(batchCtx)
				if err != nil {
					return err
				}
				if current.ChangeID != target.ChangeID || current.VersionID != target.VersionID || current.HeadSHA != target.HeadSHA {
					return errors.New("reviewed version changed before batch push")
				}
				return nil
			}})
		} else {
			result, err = lander.LandChange(ctx, info, issue, options)
		}
	}
	if err == nil && result.CI != nil && result.CI.State == "pending" {
		waiting := r.refusedLanding(req, target, "", "waiting for required CI on "+result.CI.HeadSHA)
		waiting.WorkspaceBranch = info.Branch
		return waiting, nil
	}
	if err != nil {
		if result.MergeSHA == "" {
			if IsCapacityError(err) || errors.Is(err, github.ErrRateLimited) || errors.Is(err, forgeavailability.ErrUnavailable) {
				return RunResult{WorkspaceBranch: info.Branch, NativeLanding: &NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA}}, err
			}
			var validation *workspace.ValidationError
			if errors.As(err, &validation) {
				return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLandingRefused, NativeLanding: &NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, GateFailed: true, Refusal: validation.Error()}}, nil
			}
			var refusal *workspace.LandRefusal
			if errors.As(err, &refusal) {
				if refusal.Kind == workspace.LandRefusalBaseMoved {
					waiting := r.refusedLanding(req, target, refusal.Kind, err.Error())
					if effectiveGate.LandingMode != gate.LandingRollingBarrier {
						waiting.NativeLanding.BaseSHA = refusal.BaseSHA
					}
					waiting.WorkspaceBranch = info.Branch
					return waiting, nil
				}
				r.logWorkerEvent(req.Issue, "worker_native_landing_refused",
					telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "kind", refusal.Kind, "reason", refusal.Reason)
				return r.refusedLanding(req, target, refusal.Kind, refusal.Reason), nil
			}
			return RunResult{WorkspaceBranch: info.Branch, NativeLanding: &NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA}}, err
		}
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_native_landing_warning",
			telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "merge_sha", result.MergeSHA, "error", err.Error())
	}
	landed := NativeLanding{Path: result.Path, Packages: result.Packages, CI: result.CI, ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, BaseSHA: result.BaseBefore, Landed: true, MergeSHA: result.MergeSHA, BaseRef: result.BaseRef, Method: result.Method, Rebased: result.Rebased}
	if result.Gate.Command != "" {
		landed.Gate = &result.Gate
	}
	// The base branch already carries the commit. Keep the landing beside the
	// worktree before reporting it, so a report the hub cannot take right now
	// is delivered by the next landing run instead of being lost.
	if err := workspace.RecordLanding(ctx, info, target.HeadSHA, result); err != nil {
		r.logWorkerEvent(req.Issue, "worker_native_landing_record_failed", telemetry.WorkAttemptIDKey, req.WorkAttemptID, "error", err.Error())
	}
	if err := r.reportLanding(ctx, landing, landed); err != nil {
		return RunResult{}, fmt.Errorf("record landing %s on %s: %w", result.MergeSHA, result.BaseRef, err)
	}
	if err := workspace.ForgetLanding(ctx, info); err != nil {
		r.logWorkerEvent(req.Issue, "worker_native_landing_record_failed", telemetry.WorkAttemptIDKey, req.WorkAttemptID, "error", err.Error())
	}
	r.logWorkerEvent(req.Issue, "worker_native_landed",
		telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "merge_sha", result.MergeSHA, "base_ref", result.BaseRef, "method", result.Method)
	return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLanded, NativeLanding: &landed, ForgeWriteCompleted: target.GitHubPullRequest}, nil
}

func validateLandingHead(ctx context.Context, backend workspace.Backend, info workspace.Info, issue workspace.Issue, options workspace.LandOptions) (*gate.CommandResult, error) {
	commands, canRun := backend.(workspace.ReviewCommandRunner)
	heads, canRead := backend.(workspace.HeadProvider)
	if !canRun || !canRead || options.ValidationCommand == "" || workspace.ValidationCoversHead(options.Validation, options.ValidationCommand, options.HeadSHA) {
		return options.Validation, nil
	}
	if head, err := heads.Head(ctx, info, issue); err != nil || strings.TrimSpace(head) != options.HeadSHA {
		return options.Validation, nil
	}
	issue.PullRequestHeadSHA = options.HeadSHA
	receipt, err := commands.RunReviewCommand(ctx, info, issue, options.ValidationCommand)
	if err != nil {
		return options.Validation, err
	}
	if receipt.ExitCode != 0 {
		return &receipt, &workspace.ValidationError{Output: receipt.Output, Err: fmt.Errorf("exit status %d", receipt.ExitCode)}
	}
	return &receipt, nil
}

func sameLandingExternal(a, b *tracker.ChangeExternalReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

const (
	landingReportTries   = 3
	landingReportBackoff = 500 * time.Millisecond
)

// reportLanding delivers the landing to the hub, retrying a refusal that a
// moment later may accept, such as a hub that is briefly unreachable.
func (r *Runner) reportLanding(ctx context.Context, landing LandingExecution, landed NativeLanding) error {
	var err error
	for try := range landingReportTries {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(landingReportBackoff):
			}
		}
		if err = landing.RecordLanding(ctx, landed); err == nil {
			return nil
		}
	}
	return err
}

func (r *Runner) refusedLanding(_ RunRequest, target NativeLandingTarget, kind, reason string) RunResult {
	return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLandingRefused, NativeLanding: &NativeLanding{
		ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, RefusalKind: kind, Refusal: reason,
	}}
}

func newNativeLandingGitHubClient(policy workerGitHubPolicy, token string) (*github.Client, error) {
	return github.NewClient(github.ClientConfig{
		Endpoint:                   policy.GraphQLURL,
		TokenSource:                github.StaticTokenSource(token),
		HTTPClient:                 policy.HTTPClient,
		DisableConditionalRequests: true,
		Logger:                     policy.Logger,
	})
}

func (r *Runner) nativeLandingGitHubClient(ctx context.Context, req RunRequest, policy workerGitHubPolicy) (*github.Client, workerGitHubPolicy, error) {
	var err error
	if policy.Token == "" {
		workflow, _, _, _ := r.runtimeSnapshot()
		policy, err = newWorkerGitHubPolicy(ctx, workflow.Config, r.projectID, req.Issue.Identifier, r.lookupEnv, nil, nil, r.logger)
		if err != nil {
			return nil, policy, err
		}
	}
	token := policy.Token
	if token == "" {
		token, err = resolveWorkerGitHubToken(ctx, defaultWorkerGitHubToken, workerGitHubTokenResolutionOptions{})
		if err != nil {
			return nil, policy, err
		}
	}
	client, err := newNativeLandingGitHubClient(policy, token)
	return client, policy, err
}
