package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type progressTextBackend struct {
	generate DependencyReasonGenerator
}

func (req RunRequest) rerouteImplementation() bool {
	return req.ProgressDecision.RequestID != "" && req.ProgressDecision.Decision.Outcome == progresscheck.Reroute && req.ProgressDecision.Decision.Step == RunModeImplement
}

func (b progressTextBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	return b.generate(ctx, instructions+"\n\nEvidence:\n"+input)
}

func (r *Runner) CheckProgress(ctx context.Context, req RunRequest, evidence progresscheck.Request) (decision progresscheck.Decision, returnErr error) {
	if req.Issue.IsolationPolicy != nil {
		ctx = isolation.WithPolicy(ctx, *req.Issue.IsolationPolicy)
	}
	ctx, cancel := context.WithTimeout(ctx, progresscheck.MaxDuration)
	defer cancel()
	if req.Execution == nil {
		return progresscheck.Decision{}, errors.New("progress check requires an admitted execution")
	}
	guarded, release, err := req.Execution.Guard(ctx)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	defer release()
	ctx = gobudget.WithContext(guarded, r.goBudget)
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), runtime.effectiveRunRole(role))
	if err != nil {
		return progresscheck.Decision{}, err
	}
	modelProvider, serviceTier, effort := agentTurnIdentityOptions(backendConfig)
	request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: serviceTier, ReasoningEffort: effort, MaxDuration: min(progresscheck.MaxDuration, durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS)), MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
	if request.MaxDuration <= 0 {
		request.MaxDuration = progresscheck.MaxDuration
	}
	identity := tracker.NativeExecutionIdentity{Role: role, Backend: selection.BackendID, Model: request.Model}
	if reserved, ok := req.Execution.(ProviderCapacityExecution); ok {
		if reservation := reserved.ProviderCapacity(); reservation != nil {
			if reservation.Backend != selection.BackendID || reservation.Role != role {
				return progresscheck.Decision{}, errors.New("progress backend differs from its admission")
			}
			identity.Model = reservation.Model
			if identity.Model != "provider_default" {
				request.Model = identity.Model
			}
		}
	}
	if identity.Model == "" {
		identity.Model = "provider_default"
	}
	if observed, ok := req.Execution.(RuntimeExecution); ok {
		if err := observed.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: req.WorkAttemptID, Generation: req.Generation, Phase: nativeRuntimePhase(req), HeartbeatAt: r.now()}); err != nil {
			return progresscheck.Decision{}, err
		}
	}
	if err := req.Execution.Start(ctx, identity); err != nil {
		return progresscheck.Decision{}, err
	}
	local, ok := r.workspaceSnapshot().(interface {
		Existing(workspace.Issue) (workspace.Info, error)
	})
	if !ok {
		return progresscheck.Decision{}, errors.New("progress worker workspace is unavailable")
	}
	info, err := local.Existing(workspaceIssue(r.projectID, req.Issue))
	if err != nil {
		return progresscheck.Decision{}, fmt.Errorf("resolve progress workspace: %w", err)
	}
	scratch, err := workspace.PrepareWorkerScratch(ctx, info.Path)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, workspace.CleanupWorkerScratch(info.Path, scratch)) }()
	request.TempDir = scratch
	generate := r.textGenerator(backend, request, req.Execution, backendConfig.Kind, progresscheck.MaxOutputBytes, "Return only the requested JSON object. Do not call tools or read files.")
	return (progresscheck.AI{Backend: progressTextBackend{generate: generate}}).Decide(ctx, evidence)
}
