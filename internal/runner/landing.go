package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// landNativeChange is a hub-native landing run. There is no agent in it: the
// reviewed head is combined with the base branch by the runner's own git and
// pushed with the runner's own credentials, and the hub records the commit
// the base branch advanced to. A refusal (a head that moved, a conflict, a
// base branch the forge protects) is reported on the run for the
// orchestrator to hand back to review with its reason; only an
// infrastructure failure fails the run.
func (r *Runner) landNativeChange(ctx context.Context, req RunRequest, landing LandingExecution, backend workspace.Backend, info workspace.Info, issue workspace.Issue, policy workerGitHubPolicy) (runResult RunResult, runErr error) {
	target, err := landing.LandingTarget(ctx)
	if err != nil {
		if errors.Is(err, ErrLandingNotReviewed) {
			return r.refusedLanding(req, target, workspace.LandRefusalNothing, err.Error()), nil
		}
		return RunResult{}, fmt.Errorf("resolve landing target: %w", err)
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
	options := workspace.LandOptions{HeadSHA: target.HeadSHA, Method: target.Method, Message: message, PushAttemptBranch: true, Repository: target.Repository}
	var result workspace.LandResult
	if target.GitHubPullRequest {
		githubLander, supported := backend.(workspace.GitHubPRLander)
		if !supported {
			return RunResult{}, errors.New("workspace backend cannot land through a GitHub pull request")
		}
		if policy.Token == "" {
			workflow, _, _, _ := r.runtimeSnapshot()
			policy, err = newWorkerGitHubPolicy(ctx, workflow.Config, r.projectID, req.Issue.Identifier, r.lookupEnv, nil, nil, r.logger)
			if err != nil {
				return RunResult{}, err
			}
		}
		token := policy.Token
		if token == "" {
			token, err = resolveWorkerGitHubToken(ctx, defaultWorkerGitHubToken, workerGitHubTokenResolutionOptions{})
			if err != nil {
				return RunResult{}, err
			}
		}
		client, clientErr := newNativeLandingGitHubClient(policy, token)
		if clientErr != nil {
			return RunResult{}, clientErr
		}
		options.GitHubClient = client
		defer func() {
			usage := client.FlushRESTRateLimitUsage()
			runResult.GitHubRESTUsage = &usage
			runResult.GitHubRESTConsumer = policy.budgetConsumer()
		}()
		result, err = githubLander.LandChangeViaGitHub(ctx, info, issue, options)
	} else {
		result, err = lander.LandChange(ctx, info, issue, options)
	}
	if IsCapacityError(err) || errors.Is(err, github.ErrRateLimited) {
		return RunResult{NativeLanding: &NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA}}, err
	}
	var refusal *workspace.LandRefusal
	if errors.As(err, &refusal) {
		r.logWorkerEvent(req.Issue, "worker_native_landing_refused",
			telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "kind", refusal.Kind, "reason", refusal.Reason)
		return r.refusedLanding(req, target, refusal.Kind, refusal.Reason), nil
	}
	if err != nil {
		return RunResult{}, err
	}
	landed := NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, Landed: true, MergeSHA: result.MergeSHA, BaseRef: result.BaseRef, Method: result.Method}
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
	return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLanded, NativeLanding: &landed}, nil
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
