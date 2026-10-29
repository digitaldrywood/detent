package runner

import (
	"context"

	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/progresscheck"
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

func (r *Runner) CheckProgress(ctx context.Context, req RunRequest, evidence progresscheck.Request) (progresscheck.Decision, error) {
	ctx = gobudget.WithContext(ctx, r.goBudget)
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runtime.effectiveRunRole(runRole(req.Mode, req.Issue))
	selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), role)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	modelProvider, serviceTier, effort := agentTurnIdentityOptions(backendConfig)
	request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: serviceTier, ReasoningEffort: effort, MaxDuration: durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS), MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
	generate := r.textGenerator(backend, request, req.Execution, backendConfig.Kind, progresscheck.MaxOutputBytes, "Return only the requested JSON object. Do not call tools or read files.")
	return (progresscheck.AI{Backend: progressTextBackend{generate: generate}}).Decide(ctx, evidence)
}
