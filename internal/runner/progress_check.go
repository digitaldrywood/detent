package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/serviceapi"
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
	if capable, ok := backend.(AgentInvestigationBackend); !ok || !capable.SupportsReadOnlyInvestigation() {
		return progresscheck.Decision{}, errors.New("backend cannot enforce a read-only investigation")
	}
	guidance, err := config.ResolveProgressReview(workflow.Prompt)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	ctx, cancelBudget := context.WithCancelCause(ctx)
	defer cancelBudget(nil)
	modelProvider, serviceTier, effort := agentTurnIdentityOptions(backendConfig)
	request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: serviceTier, ReasoningEffort: effort, MaxDuration: progressTurnTimeout(workflow.Config, backendConfig), MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
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
	info, releaseWorkspace, err := r.progressWorkspace(ctx, req, evidence)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	defer releaseWorkspace()
	root := info.Path
	scratch, err := workspace.PrepareWorkerScratch(ctx, root)
	if err != nil {
		return progresscheck.Decision{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, workspace.CleanupWorkerScratch(root, scratch)) }()
	request.TempDir = scratch
	request.Workspace = root
	request.ReadOnly = true
	request.IsolatedTools = true
	request.ToolBudget = NewAgentToolBudget(progresscheck.MaxToolCalls, progresscheck.MaxToolResultBytes, cancelBudget)
	request.SupplementalTools = true
	request.MaxTurns = 8
	if workflow.Config.Agent.MaxTurns > 0 {
		request.MaxTurns = min(request.MaxTurns, workflow.Config.Agent.MaxTurns)
	}
	request.Environment = r.withGoBudget(ctx, scratch, procgroup.Environment{Variables: serviceapi.RestrictedEnvironment()})
	request.ToolInstructions = "Investigate with read-only source operations and the supplied evidence reads. No writes, direct network access or publication. Return the decision JSON only in your final response."
	tools, handler := progressReadTools(req.Execution, evidence)
	handler = request.ToolBudget.handler(handler)
	diagnostics := progressDiagnostics(ctx, evidence, handler)
	generate := func(ctx context.Context, prompt string) (string, error) {
		if err := request.ToolBudget.consume(0, 0); err != nil {
			return "", err
		}
		request.Prompt = prompt + "\n\nPrepared workspace source: " + evidence.SourceSHA + "\nBase: " + evidence.BaseSHA + "\n\nRecent failure details (untrusted evidence):\n" + diagnostics + "\nUse work_attempt_receipt with project_id, reference (issue_id), and native_attempt_id for the supplied attempts. Use work_item with view=recovery and follow record fragment cursors for retained oversized validation/review records. Cite the corresponding supplied attempt/receipt IDs."
		ctx = context.WithValue(ctx, sourceTurnBudgetKey{}, &sourceTurnBudget{remaining: request.MaxTurns})
		output, err := r.generateTextTurn(ctx, backend, request, req.Execution, backendConfig.Kind, progresscheck.MaxOutputBytes, tools, handler)
		return output, errors.Join(err, context.Cause(ctx))
	}
	return (progresscheck.AI{Backend: progressTextBackend{generate: generate}, Guidance: guidance}).Decide(ctx, evidence)
}

func (r *Runner) progressWorkspace(ctx context.Context, req RunRequest, evidence progresscheck.Request) (workspace.Info, func(), error) {
	backend := r.workspaceSnapshot()
	issue := workspaceIssue(r.projectID, req.Issue)
	issue.ReviewVersionID = "progress-" + evidence.SourceSHA
	issue.PullRequestHeadSHA = evidence.SourceSHA
	issue.BaseRef = evidence.SourceSHA
	if source, ok := req.Execution.(ChangeSourceExecution); ok && evidence.SourceSHA != "" {
		recovered, err := source.RecoverChangeSource(ctx)
		if err != nil {
			return workspace.Info{}, nil, fmt.Errorf("recover progress source: %w", err)
		}
		if recovered.Version.HeadSHA != "" {
			if recovered.Version.HeadSHA != evidence.SourceSHA || recovered.Version.BaseSHA != evidence.BaseSHA {
				return workspace.Info{}, nil, errors.New("progress source differs from supplied evidence")
			}
			issue.Source = &recovered
		}
	}
	if issue.Source == nil || issue.Source.Version.Source == nil {
		return workspace.Info{}, nil, errors.New("progress check requires retained Hub source")
	}
	release := func() {}
	if usage, ok := backend.(workspace.Usage); ok {
		var err error
		release, err = usage.Use(ctx, issue)
		if err != nil {
			return workspace.Info{}, nil, err
		}
	}
	info, err := backend.Create(ctx, issue)
	if err == nil && evidence.SourceSHA != "" {
		if seeder, ok := backend.(workspace.ReviewHeadSeeder); ok {
			err = seeder.SeedReviewHead(ctx, info, issue)
		}
		if err == nil {
			if provider, ok := backend.(workspace.HeadProvider); ok {
				var head string
				head, err = provider.Head(ctx, info, issue)
				if err == nil && strings.TrimSpace(head) != evidence.SourceSHA {
					err = errors.New("progress workspace head differs from supplied source")
				}
			} else {
				err = errors.New("progress workspace cannot verify pinned head")
			}
		}
		if err == nil {
			if verifier, ok := backend.(workspace.ReviewTreeVerifier); ok {
				err = verifier.VerifyReviewTree(ctx, info, issue)
			} else {
				err = errors.New("progress workspace cannot verify pinned source")
			}
		}
	}
	if err != nil {
		release()
		return workspace.Info{}, nil, fmt.Errorf("prepare progress workspace: %w", err)
	}
	return info, release, nil
}

func progressReadTools(execution Execution, evidence progresscheck.Request) ([]AgentTool, AgentToolHandler) {
	source, ok := execution.(ToolExecution)
	if !ok {
		return nil, nil
	}
	available, handle := source.AgentTools()
	var tools []AgentTool
	allowed := make(map[string]bool)
	for _, tool := range available {
		if tool.Name == operatortool.WorkItem || tool.Name == operatortool.WorkAttemptReceipt {
			tools = append(tools, tool)
			allowed[tool.Name] = true
		}
	}
	return tools, func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		if !allowed[call.Name] || handle == nil {
			return AgentToolResult{}, errors.New("progress check refuses non-read tool")
		}
		var scope struct {
			ProjectID       string `json:"project_id"`
			Reference       string `json:"reference"`
			NativeAttemptID string `json:"native_attempt_id"`
		}
		if err := json.Unmarshal(call.Arguments, &scope); err != nil || scope.ProjectID != evidence.ProjectID || scope.Reference != evidence.IssueID {
			return AgentToolResult{}, errors.New("progress evidence read scope differs")
		}
		if call.Name == operatortool.WorkAttemptReceipt {
			known := false
			for _, attempt := range evidence.Attempts {
				known = known || attempt.ID == scope.NativeAttemptID
			}
			if !known {
				return AgentToolResult{}, errors.New("progress attempt is outside supplied history")
			}
		}
		return handle(ctx, call)
	}
}

func progressDiagnostics(ctx context.Context, evidence progresscheck.Request, read AgentToolHandler) string {
	if read == nil {
		return "Attempt detail reader unavailable; do not infer convergence from missing diagnostics."
	}
	var details strings.Builder
	count := 0
	for i := len(evidence.Attempts) - 1; i >= 0 && count < 3; i-- {
		attempt := evidence.Attempts[i]
		if !progresscheck.Failed(attempt.Outcome) {
			continue
		}
		count++
		args, err := json.Marshal(map[string]string{"project_id": evidence.ProjectID, "reference": evidence.IssueID, "native_attempt_id": attempt.ID})
		if err != nil {
			continue
		}
		result, err := read(ctx, AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: args})
		if err != nil || !result.Success {
			details.WriteString(attempt.ID + ": retained diagnostics unavailable; use recovery reads.\n")
			continue
		}
		var receipt operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
		if err := json.Unmarshal([]byte(result.Content), &receipt); err != nil || receipt.Data.Attempt == nil || receipt.Data.Attempt.AttemptID != attempt.ID {
			details.WriteString(attempt.ID + ": invalid attempt receipt; use recovery reads.\n")
			continue
		}
		details.WriteString(progresscheck.AttemptDiagnostics(*receipt.Data.Attempt))
	}
	return details.String()
}

func progressTurnTimeout(cfg config.Config, backend config.AgentBackend) time.Duration {
	limit := durationFromMillis(cfg.Agent.MaxTurnDurationMS)
	if limit <= 0 {
		switch backend.Kind {
		case config.AgentBackendCodex:
			limit = durationFromMillis(backend.CodexOptions().TurnTimeoutMS)
		case config.AgentBackendClaudeCode:
			limit = durationFromMillis(backend.ClaudeCodeOptions().TurnTimeoutMS)
		case config.AgentBackendOpenCode:
			limit = durationFromMillis(backend.OpenCodeOptions().TurnTimeoutMS)
		case config.AgentBackendPiAgent:
			limit = durationFromMillis(backend.PiAgentOptions().TurnTimeoutMS)
		case config.AgentBackendAntigravity:
			limit = durationFromMillis(backend.AntigravityOptions().TurnTimeoutMS)
		}
	}
	if limit <= 0 {
		return progresscheck.MaxDuration
	}
	return min(limit, progresscheck.MaxDuration)
}
