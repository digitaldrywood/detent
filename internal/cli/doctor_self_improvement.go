package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/store"
)

const (
	doctorWorkflowProposalDefaultThreshold = 2
	doctorWorkflowProposalBacklogState     = "Backlog"
	doctorWorkflowProposalDeliveryIssue    = "github_issue"
)

type doctorWorkflowImprovementProposal struct {
	ID              string                        `json:"id"`
	ProjectID       string                        `json:"project_id"`
	SignalKind      string                        `json:"signal_kind"`
	Pattern         string                        `json:"pattern"`
	Count           int                           `json:"count"`
	Title           string                        `json:"title"`
	Detail          string                        `json:"detail"`
	TargetKind      string                        `json:"target_kind"`
	TargetPath      string                        `json:"target_path,omitempty"`
	SuggestedChange string                        `json:"suggested_change"`
	Delivery        string                        `json:"delivery"`
	Governance      string                        `json:"governance"`
	Outcome         doctorWorkflowProposalOutcome `json:"outcome"`
	Evidence        map[string]any                `json:"evidence,omitempty"`
	IssueBody       string                        `json:"issue_body"`
	IssueMarker     string                        `json:"issue_marker"`
}

type doctorWorkflowProposalOutcome struct {
	Status string `json:"status"`
}

type doctorWorkflowCreatedProposalIssue struct {
	ProposalID    string `json:"proposal_id"`
	ProjectID     string `json:"project_id"`
	IssueID       string `json:"issue_id,omitempty"`
	Identifier    string `json:"identifier,omitempty"`
	URL           string `json:"url,omitempty"`
	OutcomeStatus string `json:"outcome_status,omitempty"`
	Reused        bool   `json:"reused,omitempty"`
}

type doctorWorkflowProposalConnector interface {
	connector.Connector
	connector.IssueCreator
}

type doctorWorkflowValidatorFindingPattern struct {
	Key      string
	Count    int
	Severity string
	Body     string
	Path     string
	Line     int
}

func doctorWorkflowOptimizationOptionsWithDefaults(options doctorWorkflowOptimizationOptions) doctorWorkflowOptimizationOptions {
	if options.ProposalThreshold <= 0 {
		options.ProposalThreshold = doctorWorkflowProposalDefaultThreshold
	}
	return options
}

func doctorWorkflowImprovementProposals(
	ctx context.Context,
	db doctorTelemetryStore,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	findings []doctorWorkflowOptimizationFinding,
	threshold int,
) ([]doctorWorkflowImprovementProposal, error) {
	threshold = doctorWorkflowNormalizedProposalThreshold(threshold)
	projectID := doctorProjectID(project)
	proposals := doctorWorkflowImprovementProposalsFromFindings(projectID, findings, threshold)

	validatorPatterns, err := doctorWorkflowValidatorFindingPatterns(ctx, db, projectID, threshold)
	if err != nil {
		return nil, err
	}
	for _, pattern := range validatorPatterns {
		proposals = append(proposals, doctorWorkflowValidatorFindingProposal(projectID, pattern))
	}

	lessonPatterns, err := doctorWorkflowLessonFailureKindPatterns(project, cfg, threshold)
	if err != nil {
		return nil, err
	}
	for _, pattern := range lessonPatterns {
		proposals = append(proposals, doctorWorkflowLessonPatternProposal(projectID, pattern))
	}

	seen := map[string]struct{}{}
	out := make([]doctorWorkflowImprovementProposal, 0, len(proposals))
	for _, proposal := range proposals {
		proposal = doctorWorkflowFinalizeProposal(proposal)
		if proposal.ID == "" {
			continue
		}
		if _, ok := seen[proposal.ID]; ok {
			continue
		}
		seen[proposal.ID] = struct{}{}
		out = append(out, proposal)
	}
	return out, nil
}

func doctorWorkflowProposalsForFindings(projectID string, findings []doctorWorkflowOptimizationFinding, threshold int) []doctorWorkflowImprovementProposal {
	proposals := doctorWorkflowImprovementProposalsFromFindings(projectID, findings, threshold)
	out := make([]doctorWorkflowImprovementProposal, 0, len(proposals))
	for _, proposal := range proposals {
		proposal = doctorWorkflowFinalizeProposal(proposal)
		if proposal.ID == "" {
			continue
		}
		out = append(out, proposal)
	}
	return out
}

func doctorWorkflowImprovementProposalsFromFindings(projectID string, findings []doctorWorkflowOptimizationFinding, threshold int) []doctorWorkflowImprovementProposal {
	proposals := []doctorWorkflowImprovementProposal{}
	for _, finding := range findings {
		count := doctorWorkflowFindingSignalCount(finding)
		if count < threshold {
			continue
		}
		targetKind, targetPath, suggestedChange := doctorWorkflowFindingProposalTarget(finding)
		proposals = append(proposals, doctorWorkflowImprovementProposal{
			ProjectID:       projectID,
			SignalKind:      "doctor_finding",
			Pattern:         finding.RuleID,
			Count:           count,
			Title:           "Propose " + finding.Title,
			Detail:          finding.Detail,
			TargetKind:      targetKind,
			TargetPath:      targetPath,
			SuggestedChange: suggestedChange,
			Evidence:        cloneAnyMap(finding.Evidence),
		})
	}
	return proposals
}

func doctorWorkflowValidatorFindingProposal(projectID string, pattern doctorWorkflowValidatorFindingPattern) doctorWorkflowImprovementProposal {
	evidence := map[string]any{
		"count":    pattern.Count,
		"severity": pattern.Severity,
		"finding":  pattern.Body,
	}
	if pattern.Path != "" {
		evidence["path"] = pattern.Path
	}
	if pattern.Line > 0 {
		evidence["line"] = pattern.Line
	}
	return doctorWorkflowImprovementProposal{
		ProjectID:       projectID,
		SignalKind:      "validator_finding",
		Pattern:         pattern.Key,
		Count:           pattern.Count,
		Title:           "Propose guard for repeated validator finding",
		Detail:          fmt.Sprintf("validator finding repeated %d times: %s", pattern.Count, pattern.Body),
		TargetKind:      "gate",
		TargetPath:      pattern.Path,
		SuggestedChange: "Update WORKFLOW.md instructions, a prompt template, or a gate rule so agents address this validator finding before review: " + pattern.Body,
		Evidence:        evidence,
	}
}

func doctorWorkflowLessonPatternProposal(projectID string, pattern lessons.FailureKindPattern) doctorWorkflowImprovementProposal {
	evidence := map[string]any{
		"count":          pattern.Count,
		"failure_kind":   pattern.FailureKind,
		"recent_lessons": append([]string(nil), pattern.Examples...),
	}
	return doctorWorkflowImprovementProposal{
		ProjectID:       projectID,
		SignalKind:      "lesson_failure_kind",
		Pattern:         strings.ToLower(strings.TrimSpace(pattern.FailureKind)),
		Count:           pattern.Count,
		Title:           "Propose skill or workflow guard for repeated " + pattern.FailureKind,
		Detail:          fmt.Sprintf("lessons recorded failure kind %q %d times", pattern.FailureKind, pattern.Count),
		TargetKind:      "skill",
		TargetPath:      ".detent/skills",
		SuggestedChange: "Draft or update a reviewed skill, prompt checklist, or gate note that prevents repeated " + pattern.FailureKind + " failures. The change must enter the backlog or a PR for human review before use.",
		Evidence:        evidence,
	}
}

func doctorWorkflowFinalizeProposal(proposal doctorWorkflowImprovementProposal) doctorWorkflowImprovementProposal {
	proposal.ProjectID = strings.TrimSpace(proposal.ProjectID)
	proposal.SignalKind = strings.TrimSpace(proposal.SignalKind)
	proposal.Pattern = strings.TrimSpace(proposal.Pattern)
	proposal.Title = strings.TrimSpace(proposal.Title)
	proposal.Detail = strings.TrimSpace(proposal.Detail)
	proposal.TargetKind = strings.TrimSpace(proposal.TargetKind)
	proposal.TargetPath = strings.TrimSpace(proposal.TargetPath)
	proposal.SuggestedChange = strings.TrimSpace(proposal.SuggestedChange)
	proposal.ID = doctorWorkflowProposalID(proposal.ProjectID, proposal.SignalKind, proposal.Pattern)
	proposal.Delivery = doctorWorkflowProposalDeliveryIssue
	proposal.Governance = "Create a backlog issue or reviewable PR only; Detent must not self-apply the suggested change."
	proposal.Outcome = doctorWorkflowProposalOutcome{Status: "pending"}
	proposal.IssueMarker = "<!-- detent:self-improvement proposal_id=" + proposal.ID + " -->"
	proposal.IssueBody = doctorWorkflowProposalIssueBody(proposal)
	return proposal
}

func doctorWorkflowFindingSignalCount(finding doctorWorkflowOptimizationFinding) int {
	switch finding.RuleID {
	case doctorWorkflowRuleReworkLaps:
		return int(anyInt64(finding.Evidence["max_rework_laps_per_issue"]))
	case doctorWorkflowRuleEmptyModelTelemetry:
		return int(anyInt64(finding.Evidence["empty_model_recent_sessions"]))
	case doctorWorkflowRuleSchedulerSkipRate:
		return int(anyInt64(finding.Evidence["scheduler_skipped_decisions"]))
	case doctorWorkflowRuleInvalidWorkpadStatusRecurrence:
		return int(anyInt64(finding.Evidence["invalid_workpad_status_decisions"]))
	case doctorWorkflowRuleReviewFlowBehaviorMismatch:
		return int(anyInt64(finding.Evidence["review_entry_count"]))
	case doctorWorkflowRuleReviewFlowProseMismatch:
		return int(anyInt64(finding.Evidence["matching_phrase_count"]))
	case doctorWorkflowRuleRunawaySessionTokens:
		return int(anyInt64(finding.Evidence["session_count"]))
	case doctorWorkflowRulePinnedWorkerModelStale:
		return int(anyInt64(finding.Evidence["observed_default_sessions"]))
	case doctorWorkflowRuleSessionMultiplierKills:
		return int(anyInt64(finding.Evidence["session_multiplier_kill_count"]))
	case doctorWorkflowRuleNoSessionTokenBrake:
		count := int(anyInt64(finding.Evidence["unbraked_session_count"]))
		if count > 0 {
			return count
		}
		return 1
	default:
		return 1
	}
}

func doctorWorkflowFindingProposalTarget(finding doctorWorkflowOptimizationFinding) (string, string, string) {
	if len(finding.Patch) > 0 {
		patch := finding.Patch[0]
		path := strings.TrimSpace(patch.Path)
		target := "workflow"
		if strings.HasPrefix(path, "gate.") {
			target = "gate"
		}
		return target, path, fmt.Sprintf("Set `%s` to `%v` in WORKFLOW.md after human review.", path, patch.Value)
	}
	switch finding.RuleID {
	case doctorWorkflowRuleRepositoryMergePolicy:
		return "repository", strings.TrimSpace(fmt.Sprint(finding.Evidence["repository"])), strings.TrimSpace(fmt.Sprint(finding.Evidence["fix"]))
	case doctorWorkflowRuleEmptyModelTelemetry:
		return "telemetry", "codex_sessions.model", "Repair effective-model resolution for completed sessions while leaving the project's pin/default policy unchanged."
	case doctorWorkflowRulePinnedRouteModelRejected:
		return "workflow", doctorWorkflowModelFindingTargetPath(finding), "Replace the rejected model with a backend-supported pin or remove the pin to inherit the provider default, according to the project's intended model policy."
	case doctorWorkflowRulePinnedWorkerModelStale:
		return "workflow", doctorWorkflowModelFindingTargetPath(finding), "Review the configured pin against the observed provider-default generation, then explicitly keep, update, or remove it according to the project's intended model policy."
	case doctorWorkflowRuleSessionMultiplierKills:
		return "workflow", "agent.max_session_context_multiplier", "Remove or recalibrate the context multiplier and configure an intentional `agent.max_session_tokens` ceiling after human review."
	case doctorWorkflowRuleNoSessionTokenBrake:
		return "workflow", "agent.max_session_tokens", "Configure an intentional absolute session-token brake after human review."
	case doctorWorkflowRuleBudgetEstimateDrift:
		return "workflow", "budget", "Review budget estimates against observed p90 billable session tokens and update workflow budget guidance if humans approve."
	case doctorWorkflowRuleInvalidWorkpadStatusRecurrence:
		return "workflow", "detent-status", "Update WORKFLOW.md Workpad status instructions so agents use only `in_progress`, `blocked`, or `complete` in the `detent-status` block."
	case doctorWorkflowRuleReviewFlowBehaviorMismatch:
		return doctorWorkflowReviewFlowProposalTarget(finding)
	case doctorWorkflowRuleReviewFlowProseMismatch:
		return doctorWorkflowReviewFlowProposalTarget(finding)
	default:
		return "workflow", "", "Review this repeated doctor finding and propose a WORKFLOW.md, prompt, skill, or gate update for human approval."
	}
}

func doctorWorkflowModelFindingTargetPath(finding doctorWorkflowOptimizationFinding) string {
	if strings.TrimSpace(fmt.Sprint(finding.Evidence["configured_model_source"])) == "agents.backends.command" {
		return "agents.backends.command"
	}
	return "agents.routes.model"
}

func doctorWorkflowReviewFlowProposalTarget(finding doctorWorkflowOptimizationFinding) (string, string, string) {
	choice := strings.TrimSpace(fmt.Sprint(finding.Evidence["review_flow"]))
	switch choice {
	case doctorReviewFlowAutopilot:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md with configured autopilot: keep completed work in the active state, set `detent-status` to `complete`, and let Detent promote on the configured gate."
	case doctorReviewFlowReviewGate:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md with configured review-gate: route completed work through the configured review state and remove prose that promises direct review-state skipping."
	default:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md handoff prose with the configured review-flow frontmatter after human review."
	}
}

func doctorWorkflowValidatorFindingPatterns(ctx context.Context, db doctorTelemetryStore, projectID string, threshold int) ([]doctorWorkflowValidatorFindingPattern, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(findings_json, '[]')
FROM validator_verdicts
WHERE submitted = 1
  AND (? = '' OR project_id = ?)
ORDER BY updated_at DESC, id DESC
LIMIT 200`, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("read validator verdict findings: %w", err)
	}
	defer rows.Close()

	byKey := map[string]*doctorWorkflowValidatorFindingPattern{}
	keys := []string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var findings []store.ValidatorFinding
		if err := json.Unmarshal([]byte(raw), &findings); err != nil {
			return nil, fmt.Errorf("decode validator findings: %w", err)
		}
		for _, finding := range findings {
			key := doctorWorkflowValidatorFindingKey(finding)
			if key == "" {
				continue
			}
			pattern, ok := byKey[key]
			if !ok {
				pattern = &doctorWorkflowValidatorFindingPattern{
					Key:      key,
					Severity: strings.ToLower(strings.TrimSpace(finding.Severity)),
					Body:     strings.Join(strings.Fields(finding.Body), " "),
					Path:     strings.TrimSpace(finding.Path),
					Line:     finding.Line,
				}
				byKey[key] = pattern
				keys = append(keys, key)
			}
			pattern.Count++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(keys, func(i, j int) bool {
		left := byKey[keys[i]]
		right := byKey[keys[j]]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		return left.Key < right.Key
	})
	patterns := make([]doctorWorkflowValidatorFindingPattern, 0, len(keys))
	for _, key := range keys {
		pattern, ok := byKey[key]
		if !ok || pattern == nil {
			continue
		}
		if pattern.Count < threshold {
			continue
		}
		patterns = append(patterns, *pattern)
	}
	return patterns, nil
}

func doctorWorkflowValidatorFindingKey(finding store.ValidatorFinding) string {
	body := strings.ToLower(strings.Join(strings.Fields(finding.Body), " "))
	if body == "" {
		return ""
	}
	path := strings.ToLower(strings.TrimSpace(finding.Path))
	severity := strings.ToLower(strings.TrimSpace(finding.Severity))
	return strings.Join([]string{severity, path, body}, "|")
}

func doctorWorkflowLessonFailureKindPatterns(project globalconfig.Project, cfg workflowconfig.Config, threshold int) ([]lessons.FailureKindPattern, error) {
	path := strings.TrimSpace(cfg.Agent.Lessons.Path)
	if path == "" {
		path = lessons.DefaultPath
	}
	resolved, err := resolveDoctorProjectPath(project, path)
	if err != nil {
		return nil, err
	}
	patterns, err := lessons.FailureKindPatterns(resolved, threshold)
	if errors.Is(err, os.ErrNotExist) {
		return []lessons.FailureKindPattern{}, nil
	}
	return patterns, err
}

func createDoctorWorkflowImprovementProposalIssues(
	ctx context.Context,
	projectID string,
	cfg workflowconfig.Config,
	deps doctorDeps,
	proposals []doctorWorkflowImprovementProposal,
) (created []doctorWorkflowCreatedProposalIssue, err error) {
	if len(proposals) == 0 {
		return nil, nil
	}
	if err := doctorWorkflowProposalIssueDeliverySupported(cfg); err != nil {
		return nil, err
	}

	if deps.proposalConnector == nil {
		deps.proposalConnector = defaultDoctorProposalConnector
	}
	projectConnector, err := deps.proposalConnector(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := closeDoctorAutoPromoteConnector(projectConnector); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	existingIssueStates := doctorWorkflowProposalIssueSearchStates(cfg)
	created = make([]doctorWorkflowCreatedProposalIssue, 0, len(proposals))
	for _, proposal := range proposals {
		issue, reused, err := doctorWorkflowExistingProposalIssue(ctx, projectConnector, proposal, existingIssueStates)
		if err != nil {
			return nil, err
		}
		if !reused {
			issue, err = projectConnector.CreateIssue(ctx, connector.IssueDraft{
				Title: proposal.Title,
				Body:  proposal.IssueBody,
			})
			if err != nil {
				return nil, err
			}
			if err := projectConnector.UpdateIssueState(ctx, issue.ID, doctorWorkflowProposalBacklogState); errors.Is(err, connector.ErrStateUpdateBlocked) {
				slog.Default().Debug("skip blocked self-improvement proposal state update", "issue_id", issue.ID, "target_state", doctorWorkflowProposalBacklogState, "error", err)
			} else if err != nil {
				return nil, err
			}
		}
		created = append(created, doctorWorkflowCreatedProposalIssue{
			ProposalID:    proposal.ID,
			ProjectID:     projectID,
			IssueID:       issue.ID,
			Identifier:    issue.Identifier,
			URL:           issue.URL,
			OutcomeStatus: doctorWorkflowProposalOutcomeStatus(issue.Description),
			Reused:        reused,
		})
	}
	return created, nil
}

func doctorWorkflowProposalIssueDeliverySupported(cfg workflowconfig.Config) error {
	switch cfg.Tracker.Kind {
	case workflowconfig.TrackerGitHub:
		if cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2 {
			return errors.New("self-improvement proposal issue creation requires GitHub label or issue-field status mode; project_v2 cannot place new issues in Backlog")
		}
	case workflowconfig.TrackerMemory:
	default:
		return fmt.Errorf("self-improvement proposal issue creation is not supported for tracker kind %q", cfg.Tracker.Kind)
	}
	return nil
}

func defaultDoctorProposalConnector(cfg workflowconfig.Config) (doctorWorkflowProposalConnector, error) {
	projectConnector, err := factory.NewFromConfig(factory.Config{
		Kind:   cfg.Tracker.Kind,
		Memory: memory.Config{Issues: cfg.Tracker.Issues, Stateful: true},
		LocalSQLite: local.Config{
			Path:           cfg.Tracker.LocalSQLite.Path,
			ProjectID:      cfg.Tracker.LocalSQLite.ProjectID,
			Issues:         cfg.Tracker.Issues,
			ActiveStates:   cfg.Tracker.ActiveStates,
			ObservedStates: cfg.Tracker.ObservedStates,
			TerminalStates: cfg.Tracker.TerminalStates,
		},
		Endpoint:                    cfg.Tracker.Endpoint,
		APIKey:                      cfg.Tracker.APIKey,
		HTTPMaxIdleConns:            cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost:     cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:       cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubRESTMinReserve:        cfg.Tracker.GitHubRESTMinReserve,
		GitHubRESTFanoutMaxRequests: cfg.Tracker.GitHubRESTFanoutMaxRequests,
		GitHubRESTDebugLogging:      cfg.Tracker.GitHubRESTDebugLogging,
		GitHubAppID:                 cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:         cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath:     cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID:     cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:          cfg.Tracker.GitHubStatusSource,
		DependencySource:            cfg.Dependencies.Source,
		ProjectSlug:                 cfg.Tracker.ProjectSlug,
		Repository:                  cfg.Tracker.Repository,
		StatusField:                 cfg.Tracker.StatusField,
		StatusLabelPrefix:           cfg.Tracker.StatusLabelPrefix,
		ActiveStates:                cfg.Tracker.ActiveStates,
		ObservedStates:              cfg.Tracker.ObservedStates,
		TerminalStates:              cfg.Tracker.TerminalStates,
		StateMap:                    doctorTrackerStateMap(cfg.Tracker.StateMap),
		PriorityMap:                 doctorTrackerPriorityMap(cfg.Tracker.PriorityMap),
		RequiredStatusChecks:        cfg.Gate.RequiredStatusChecks,
	})
	if err != nil {
		return nil, err
	}
	proposalConnector, ok := projectConnector.(doctorWorkflowProposalConnector)
	if !ok {
		if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil {
			return nil, err
		}
		return nil, connector.ErrNotImplemented
	}
	return proposalConnector, nil
}

func doctorWorkflowProposalIssueSearchStates(cfg workflowconfig.Config) []string {
	states := make([]string, 0, 1+len(cfg.Tracker.ObservedStates)+len(cfg.Tracker.ActiveStates)+len(cfg.Tracker.TerminalStates))
	seen := make(map[string]struct{})
	add := func(state string) {
		state = strings.TrimSpace(state)
		if state == "" {
			return
		}
		key := strings.ToLower(state)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	add(doctorWorkflowProposalBacklogState)
	for _, state := range cfg.Tracker.ObservedStates {
		add(state)
	}
	for _, state := range cfg.Tracker.ActiveStates {
		add(state)
	}
	for _, state := range cfg.Tracker.TerminalStates {
		add(state)
	}
	return states
}

func doctorWorkflowExistingProposalIssue(ctx context.Context, projectConnector doctorWorkflowProposalConnector, proposal doctorWorkflowImprovementProposal, states []string) (connector.Issue, bool, error) {
	issues, err := projectConnector.FetchIssuesByStates(ctx, states)
	if err != nil {
		return connector.Issue{}, false, err
	}
	for _, issue := range issues {
		if strings.Contains(issue.Description, proposal.IssueMarker) {
			return issue, true, nil
		}
	}
	return connector.Issue{}, false, nil
}

func doctorWorkflowProposalIssueBody(proposal doctorWorkflowImprovementProposal) string {
	var builder strings.Builder
	builder.WriteString(proposal.IssueMarker)
	builder.WriteString("\n\n## Problem\n\n")
	builder.WriteString(proposal.Detail)
	builder.WriteString("\n\n## Proposed Change\n\n")
	builder.WriteString(proposal.SuggestedChange)
	builder.WriteString("\n\n## Evidence\n\n")
	builder.WriteString("- project: ")
	builder.WriteString(proposal.ProjectID)
	builder.WriteString("\n- signal: ")
	builder.WriteString(proposal.SignalKind)
	builder.WriteString("\n- pattern: ")
	builder.WriteString(proposal.Pattern)
	builder.WriteString("\n- count: ")
	builder.WriteString(strconv.Itoa(proposal.Count))
	if len(proposal.Evidence) > 0 {
		for _, key := range sortedAnyMapKeys(proposal.Evidence) {
			builder.WriteString("\n- ")
			builder.WriteString(key)
			builder.WriteString(": ")
			builder.WriteString(fmt.Sprint(proposal.Evidence[key]))
		}
	}
	builder.WriteString("\n\n## Target\n\n")
	builder.WriteString("- kind: ")
	builder.WriteString(proposal.TargetKind)
	if proposal.TargetPath != "" {
		builder.WriteString("\n- path: ")
		builder.WriteString(proposal.TargetPath)
	}
	builder.WriteString("\n\n## Governance\n\n")
	builder.WriteString(proposal.Governance)
	builder.WriteString("\n\n## Outcome\n\n- status: pending\n- accepted/rejected record: change status to `accepted` or `rejected`; future proposal runs reuse this marker instead of opening a duplicate.\n")
	return builder.String()
}

func doctorWorkflowProposalOutcomeStatus(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, "- status:")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.Trim(strings.TrimSpace(value), "`")) {
		case "pending", "accepted", "rejected":
			return strings.ToLower(strings.Trim(strings.TrimSpace(value), "`"))
		}
	}
	return ""
}

func doctorWorkflowProposalID(projectID string, signalKind string, pattern string) string {
	raw := strings.ToLower(strings.TrimSpace(projectID)) + "|" + strings.ToLower(strings.TrimSpace(signalKind)) + "|" + strings.ToLower(strings.TrimSpace(pattern))
	sum := sha256.Sum256([]byte(raw))
	return "detent-self-improve-" + hex.EncodeToString(sum[:])[:12]
}

func doctorWorkflowNormalizedProposalThreshold(threshold int) int {
	if threshold <= 0 {
		return doctorWorkflowProposalDefaultThreshold
	}
	return threshold
}

func doctorTrackerPriorityMap(value workflowconfig.StringOrMap) map[string]*int {
	if !value.IsMap {
		return nil
	}
	out := make(map[string]*int, len(value.Map))
	for name, rank := range value.Map {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch rank := rank.(type) {
		case nil:
			out[name] = nil
		case int:
			rankValue := rank
			out[name] = &rankValue
		}
	}
	return out
}

func cloneAnyMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func sortedAnyMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func anyInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case int32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		out, err := typed.Int64()
		if err != nil {
			return 0
		}
		return out
	default:
		return 0
	}
}
