package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) audit(ctx context.Context, req SecurityAuditRequest) (SecurityAuditExecution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, _, _, _ := r.runtimeSnapshot()
	if timeout := durationFromMillis(gateSecurityAuditConfig(workflow.Config).TurnTimeoutMS); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	originalComplete := req.complete
	originalUsage := req.OnUsageUpdate
	var firstUsage TokenTotals
	req.OnUsageUpdate = func(update UsageUpdate) error {
		firstUsage = update.Tokens
		if originalUsage != nil {
			return originalUsage(update)
		}
		return nil
	}
	var verified *SecurityAuditExecution
	req.complete = func(ctx context.Context, first SecurityAuditExecution) error {
		if !first.Result.NeedsVerification() {
			if originalComplete != nil {
				return originalComplete(ctx, first)
			}
			return nil
		}
		verification := req
		verification.Snapshot.Verification = &first.Result
		verification.OnUsageUpdate = func(update UsageUpdate) error {
			update.Tokens = addAgentTokenTotals(firstUsage, update.Tokens)
			if originalUsage != nil {
				return originalUsage(update)
			}
			return nil
		}
		verification.complete = func(ctx context.Context, final SecurityAuditExecution) error {
			final.Verification = &securityaudit.Verification{Initial: first.Result, Final: final.Result}
			raw, err := json.Marshal(struct {
				Initial string `json:"initial_output"`
				Final   string `json:"verification_output"`
			}{first.Output, final.Output})
			if err != nil {
				return err
			}
			final.Output = string(raw)
			final.StartedAt = first.StartedAt
			verified = &final
			if originalComplete != nil {
				return originalComplete(ctx, final)
			}
			return nil
		}
		_, err := r.auditSession(ctx, verification, true)
		return err
	}
	first, err := r.auditSession(ctx, req, false)
	if verified != nil {
		return *verified, err
	}
	return first, err
}

func (r *Runner) auditSession(ctx context.Context, req SecurityAuditRequest, verification bool) (execution SecurityAuditExecution, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, agentRuntime, _, _ := r.runtimeSnapshot()
	if req.Issue.IsolationPolicy != nil {
		ctx = isolation.WithPolicy(ctx, *req.Issue.IsolationPolicy)
	} else if workflow.Config.Tracker.Kind == config.TrackerHubNative {
		ctx = isolation.WithPolicy(ctx, isolation.Policy{})
	}
	auditConfig := gateSecurityAuditConfig(workflow.Config)
	plan, err := securityaudit.BuildPlan(req.Snapshot, securityAuditMaxDiffBytes(auditConfig))
	if err != nil {
		return execution, err
	}
	selection, backend, backendConfig, err := agentRuntime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), RoleSecurityAudit)
	if err != nil {
		return execution, err
	}

	auditWorkspace, err := r.prepareSecurityAuditWorkspace()
	if err != nil {
		return execution, err
	}
	removeAuditWorkspace := true
	defer func() {
		if err == nil && req.complete != nil {
			err = req.complete(ctx, execution)
			if err != nil {
				diagnosticErr := r.publishSecurityAuditDiagnostic(ctx, req, execution, auditWorkspace, err)
				if diagnosticErr != nil {
					removeAuditWorkspace = false
					err = errors.Join(err, fmt.Errorf("security audit diagnostics retained at %s: %w", auditWorkspace, diagnosticErr))
				}
			}
		}
		if !removeAuditWorkspace {
			return
		}
		if cleanupErr := os.RemoveAll(auditWorkspace); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove security audit workspace: %w", cleanupErr))
		}
	}()

	startedAt := req.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	execution.InvocationID = uuid.NewString()
	execution.StartedAt = startedAt
	selectedModel := selection.Model
	baseModel := effectiveModel("", selectedModel, agentRuntime.defaultModelForRole(RoleSecurityAudit))
	if verification {
		baseModel = workflow.Config.EffectiveModelSelection().Model("complex")
		if baseModel == "" {
			return execution, errors.New("security audit complex model is not configured")
		}
	}
	selectionIssue := req.Issue
	selectionIssue.Description = ""
	baseEnvironment := r.withGoBudget(ctx, auditWorkspace, workerEnvironment(map[string]string{
		"GH_TOKEN":                             "",
		"GITHUB_TOKEN":                         "",
		serviceapi.AddressEnvironment:          "",
		serviceapi.TokenEnvironment:            "",
		serviceapi.DispositionTokenEnvironment: "",
	}, workspace.Info{Path: auditWorkspace}, workspaceIssue(r.projectID, req.Issue)))
	processRequest, cleanupPreflight, err := prepareAgentProcessRequest(ctx, AgentProcessRequest{
		Workspace:   auditWorkspace,
		Environment: baseEnvironment,
	}, workerGitHubPolicy{})
	if err != nil {
		return execution, err
	}
	resolvedSelection := resolveAgentSelection(ctx, selectionIssue, processRequest, baseModel, RoleSecurityAudit, workflow.Config, backendConfig, backend)
	if verification && resolvedSelection.Err == nil {
		provider, ok := backend.(AgentModelCatalogProvider)
		if !ok {
			resolvedSelection.Err = errors.New("security audit verification requires a backend model catalog")
		} else {
			models, catalogErr := provider.ListModels(ctx, processRequest)
			if catalogErr != nil {
				resolvedSelection.Err = fmt.Errorf("security audit verification model catalog: %w", catalogErr)
			} else if model, available := availableSelectionModel(models, baseModel); !available {
				resolvedSelection.Err = errors.New("security audit complex model is unavailable or retired")
			} else {
				resolvedSelection.Model = canonicalAgentModel(model, baseModel)
				resolvedSelection.Selection.ModelSource = selectionSource(workflow.Config.EffectiveModelSelection(), "complex_model", "complex")
			}
		}
	}
	if err := r.agentPreflightError(resolvedSelection.Err, cleanupPreflight()); err != nil {
		return execution, err
	}
	selectedModel = resolvedSelection.Model
	sessionModel := effectiveModel("", selectedModel, agentRuntime.defaultModelForRole(RoleSecurityAudit))
	runtimeIdentity := configuredRuntimeIdentity(selection, backendConfig, RoleSecurityAudit, sessionModel, startedAt)
	runtimeIdentity.Selection = resolvedSelection.Selection
	if resolvedSelection.Effort != "" {
		runtimeIdentity.ReasoningEffort = agentidentity.NewValue(resolvedSelection.Effort, agentidentity.ProvenanceConfigured)
	}
	runReq := RunRequest{Issue: req.Issue, StartedAt: startedAt, SelectorContext: req.SelectorContext}
	cacheCtx, release, err := toolcache.UseFromContext(ctx)
	if err != nil {
		return execution, fmt.Errorf("%w: acquire managed Go build cache: %w", ErrValidatorInfrastructure, err)
	}
	defer func() { err = errors.Join(err, release()) }()
	sessionID, sessionStarted, err := r.startSession(ctx, runReq, startedAt, runtimeIdentity, store.AgentResumeState{}, "", "")
	if err != nil {
		return execution, err
	}

	runResult := RunResult{FinalState: FinalStateCompleted, RuntimeIdentity: runtimeIdentity}
	modelProvider, serviceTier, effort := agentTurnIdentityOptions(backendConfig)
	if resolvedSelection.Effort != "" {
		effort = resolvedSelection.Effort
	}
	var turnResult AgentTurnResult
	var turns int64
	var resume AgentResume
	usage := newSessionTokenUsage(false)
	outputs := make([]securityaudit.ChunkOutput, 0, plan.Len())
	for index := range plan.Len() {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = os.MkdirAll(auditWorkspace, 0o700); err != nil {
			break
		}
		if index > 0 {
			if agentResumeEmpty(resume) {
				err = errors.New("security audit chunk conversation identity is unavailable")
				break
			}
			resumeProcess, cleanupResume, prepareErr := prepareAgentProcessRequest(ctx, AgentProcessRequest{Workspace: auditWorkspace, Environment: baseEnvironment}, workerGitHubPolicy{})
			if prepareErr != nil {
				err = prepareErr
				break
			}
			if err = r.agentPreflightError(verifyAgentResume(ctx, backend, resumeProcess, resume), cleanupResume()); err != nil {
				break
			}
		}
		identity, prompt := plan.Chunk(index)
		var output strings.Builder
		previousUsage := runResult.Tokens
		runResult.ReportedCostMicros, runResult.CostSource = nil, ""
		usageReport := &turnUsageReporter{tokens: previousUsage}
		removeAuditWorkspace = false
		turns++
		execution.WorkerProcess = procgroup.Identity{}
		currentTurn, cleanupScratch, turnErr := runAgentBackendTurnWithToolsUsingLimitPreservingScratch(cacheCtx, backend, AgentTurnRequest{
			Workspace:        auditWorkspace,
			Prompt:           prompt,
			Resume:           resume,
			ToolInstructions: securityaudit.ToolInstructions(),
			ReadOnly:         true,
			Model:            selectedModel,
			ModelProvider:    modelProvider,
			ServiceTier:      serviceTier,
			ReasoningEffort:  effort,
			TurnTimeout:      durationFromMillis(auditConfig.TurnTimeoutMS),
			MaxDuration:      durationFromMillis(auditConfig.TurnTimeoutMS),
			Environment:      baseEnvironment,
			MaxRSSBytes:      r.maxAgentRSSBytes,
			RSSPollInterval:  r.rssPollInterval,
			processRSS:       r.processRSS,
			reserveScratch:   r.reserveSessionWorkerScratch(sessionID, r.securityAuditRoot, auditWorkspace),
		}, nil, nil, func(updateCtx context.Context, update AgentUpdate) error {
			r.providerUsage.ObserveUpdate(runtimeIdentity.BackendID, update)
			if update.Type == AgentUpdateToolStarted || update.Type == AgentUpdateToolOutput || update.Type == AgentUpdateToolCompleted {
				return ErrSecurityAuditToolUse
			}
			if update.WorkerProcess.PID > 0 {
				execution.WorkerProcess = update.WorkerProcess
			}
			if err := r.persistSessionWorkerProcess(updateCtx, sessionID, update, r.securityAuditRoot, auditWorkspace); err != nil {
				return err
			}
			r.persistSessionProviderIdentity(updateCtx, sessionID, update)
			if update.Type == AgentUpdateMessageDelta {
				output.WriteString(update.Delta)
			}
			previousIdentity := runResult.RuntimeIdentity
			if update.Type == AgentUpdateTokenUsage {
				update.Tokens = usage.normalize(update.Tokens)
			}
			applyAgentUpdate(&runResult, update)
			if update.Type == AgentUpdateTokenUsage && update.Tokens.ThreadTotal == nil {
				runResult.Tokens = addAgentTokenTotals(previousUsage, runResult.Tokens)
			}
			if update.Type == AgentUpdateTokenUsage {
				usageReport.report(updateCtx, req.usageExecution, runResult, sessionModel, backendConfig.Kind, r.usageCostUSD, r.logger)
			}
			if req.OnUsageUpdate != nil {
				if err := req.OnUsageUpdate(UsageUpdate{DetentSessionID: sessionID, Tokens: runResult.Tokens, RuntimeIdentity: runResult.RuntimeIdentity, RateLimits: runResult.RateLimits}); err != nil {
					return err
				}
			}
			if !previousIdentity.MateriallyEqual(runResult.RuntimeIdentity) {
				if err := r.persistSessionIdentity(updateCtx, sessionID, runResult.RuntimeIdentity); err != nil {
					return err
				}
			}
			return nil
		}, r.turnLimit)
		reapErr := r.reapSessionWorkerProcess(ctx, sessionID, req.Issue, workerProcessReapReason(ctx, turnErr))
		removeAuditWorkspace = reapErr == nil
		turnErr = errors.Join(turnErr, cleanupWorkerScratchAfterProcessReap(cleanupScratch, reapErr))
		if reapErr != nil {
			turnErr = errors.Join(turnErr, fmt.Errorf("%w: %w", ErrWorkerProcessReap, reapErr))
		}
		turnResult = currentTurn
		runResult.ReportedCostMicros, runResult.CostSource = turnResult.ReportedCostMicros, turnResult.CostSource
		usageCtx, usageCancel := context.WithTimeout(context.WithoutCancel(ctx), r.afterRunTimeout)
		usageReport.report(usageCtx, req.usageExecution, runResult, sessionModel, backendConfig.Kind, r.usageCostUSD, r.logger)
		usageCancel()
		execution.RuntimeIdentity = runResult.RuntimeIdentity
		execution.SessionID = sessionID
		execution.AuthenticationMode = turnResult.AuthenticationMode
		execution.ProviderThreadID = turnResult.ThreadID
		execution.ProviderSessionID = turnResult.SessionID
		execution.Output = output.String()
		if turnErr != nil {
			err = fmt.Errorf("run security audit turn: %w", turnErr)
			break
		}
		if plan.Len() > 1 && (execution.WorkerProcess.PID <= 0 || execution.WorkerProcess.GroupID <= 0 || execution.WorkerProcess.StartedAt.IsZero()) {
			err = errors.New("security audit chunk worker identity is unavailable")
			break
		}
		if index > 0 && ((resume.ThreadID != "" && turnResult.ThreadID != resume.ThreadID) || (resume.ThreadID == "" && resume.SessionID != "" && turnResult.SessionID != resume.SessionID)) {
			err = errors.New("security audit chunk conversation identity changed")
			break
		}
		chunkResult, parseErr := securityaudit.ParseOutput(execution.Output)
		if parseErr == nil {
			parseErr = securityaudit.ValidateSnapshotContinuation(req.Snapshot, chunkResult)
		}
		if parseErr != nil {
			err = parseErr
			break
		}
		outputs = append(outputs, securityaudit.ChunkOutput{Identity: identity, Output: execution.Output})
		resume = AgentResume{ThreadID: turnResult.ThreadID, SessionID: turnResult.SessionID}
	}
	execution.CompletedAt = r.now().UTC()
	runResult.Tokens.RuntimeSeconds = runtimeSeconds(startedAt, execution.CompletedAt)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		execution.Result, execution.Output, err = plan.Aggregate(outputs)
		if err == nil {
			securityaudit.AttributeFindings(req.Snapshot.BaseSHA, req.Snapshot.HeadSHA, execution.Result.Findings, req.Snapshot.Checks)
		}
	}
	if err != nil {
		runResult.FinalState = finalStateForTurnError(err)
	}

	finishErr := r.finishSession(context.WithoutCancel(ctx), sessionID, sessionStarted, 0, req.Issue, startedAt, execution.CompletedAt, runResult, sessionModel, backendConfig.Kind, turns, turnResult, 0)
	if err != nil {
		return execution, errors.Join(err, finishErr)
	}
	return execution, finishErr
}

func (r *Runner) prepareSecurityAuditWorkspace() (string, error) {
	root := filepath.Clean(r.securityAuditRoot)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create security audit root: %w", err)
	}
	path, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", fmt.Errorf("create security audit workspace: %w", err)
	}
	return path, nil
}

func securityAuditMaxDiffBytes(cfg gate.SecurityAuditConfig) int {
	if cfg.MaxDiffBytes == nil {
		return securityaudit.DefaultMaxDiffBytes
	}
	return *cfg.MaxDiffBytes
}

func gateSecurityAuditConfig(cfg config.Config) gate.SecurityAuditConfig {
	return gate.Effective(cfg.Gate).SecurityAudit
}

func (r *Runner) publishSecurityAuditDiagnostic(ctx context.Context, req SecurityAuditRequest, execution SecurityAuditExecution, directory string, failure error) error {
	content, err := json.MarshalIndent(struct {
		Run    securityaudit.Run
		Output string
		Error  string
	}{Run: execution.auditRun(req.Snapshot), Output: execution.Output, Error: failure.Error()}, "", "  ")
	if err != nil {
		return err
	}
	name := "security-audit-" + execution.InvocationID + ".json"
	if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
		return err
	}
	if req.evidence == nil {
		return errors.New("security audit evidence publisher is unavailable")
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.afterRunTimeout)
	defer cancel()
	return req.evidence.PublishValidationEvidence(publishCtx, []ValidationEvidence{{Name: name, ContentType: "application/json", Content: content}})
}
