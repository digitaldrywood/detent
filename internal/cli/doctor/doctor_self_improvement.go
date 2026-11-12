package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	workflowProposalDefaultThreshold = 2
	workflowProposalBacklogState     = "Backlog"
	workflowProposalDeliveryIssue    = "native_issue"
)

type WorkflowImprovementProposal struct {
	ID              string                  `json:"id"`
	ProjectID       string                  `json:"project_id"`
	SignalKind      string                  `json:"signal_kind"`
	Pattern         string                  `json:"pattern"`
	Count           int                     `json:"count"`
	Title           string                  `json:"title"`
	Detail          string                  `json:"detail"`
	TargetKind      string                  `json:"target_kind"`
	TargetPath      string                  `json:"target_path,omitempty"`
	SuggestedChange string                  `json:"suggested_change"`
	Delivery        string                  `json:"delivery"`
	Governance      string                  `json:"governance"`
	Outcome         workflowProposalOutcome `json:"outcome"`
	Evidence        map[string]any          `json:"evidence,omitempty"`
	IssueBody       string                  `json:"issue_body"`
	IssueMarker     string                  `json:"issue_marker"`
}

type workflowProposalOutcome struct {
	Status string `json:"status"`
}

type WorkflowProposalConnector interface {
	connector.Connector
	connector.IssueCreator
}

func WorkflowProposalsForFindings(projectID string, findings []WorkflowOptimizationFinding, threshold int) []WorkflowImprovementProposal {
	proposals := workflowImprovementProposalsFromFindings(projectID, findings, threshold)
	out := make([]WorkflowImprovementProposal, 0, len(proposals))
	for _, proposal := range proposals {
		proposal = workflowFinalizeProposal(proposal)
		if proposal.ID == "" {
			continue
		}
		out = append(out, proposal)
	}
	return out
}

func workflowImprovementProposalsFromFindings(projectID string, findings []WorkflowOptimizationFinding, threshold int) []WorkflowImprovementProposal {
	proposals := []WorkflowImprovementProposal{}
	for _, finding := range findings {
		count := workflowFindingSignalCount(finding)
		if count < threshold {
			continue
		}
		targetKind, targetPath, suggestedChange := workflowFindingProposalTarget(finding)
		proposals = append(proposals, WorkflowImprovementProposal{
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

func workflowFinalizeProposal(proposal WorkflowImprovementProposal) WorkflowImprovementProposal {
	proposal.ProjectID = strings.TrimSpace(proposal.ProjectID)
	proposal.SignalKind = strings.TrimSpace(proposal.SignalKind)
	proposal.Pattern = strings.TrimSpace(proposal.Pattern)
	proposal.Title = strings.TrimSpace(proposal.Title)
	proposal.Detail = strings.TrimSpace(proposal.Detail)
	proposal.TargetKind = strings.TrimSpace(proposal.TargetKind)
	proposal.TargetPath = strings.TrimSpace(proposal.TargetPath)
	proposal.SuggestedChange = strings.TrimSpace(proposal.SuggestedChange)
	proposal.ID = workflowProposalID(proposal.ProjectID, proposal.SignalKind, proposal.Pattern)
	proposal.Delivery = workflowProposalDeliveryIssue
	proposal.Governance = "Create a backlog issue or reviewable PR only; Detent must not self-apply the suggested change."
	proposal.Outcome = workflowProposalOutcome{Status: "pending"}
	proposal.IssueMarker = "<!-- detent:self-improvement proposal_id=" + proposal.ID + " -->"
	proposal.IssueBody = workflowProposalIssueBody(proposal)
	return proposal
}

func workflowFindingSignalCount(finding WorkflowOptimizationFinding) int {
	switch finding.RuleID {
	case workflowRuleReworkLaps:
		return int(anyInt64(finding.Evidence["max_rework_laps_per_issue"]))
	case workflowRuleEmptyModelTelemetry:
		return int(anyInt64(finding.Evidence["empty_model_recent_sessions"]))
	case workflowRuleSchedulerSkipRate:
		return int(anyInt64(finding.Evidence["scheduler_skipped_decisions"]))
	case workflowRuleInvalidWorkpadStatusRecurrence:
		return int(anyInt64(finding.Evidence["invalid_workpad_status_decisions"]))
	case workflowRuleReviewFlowBehaviorMismatch:
		return int(anyInt64(finding.Evidence["review_entry_count"]))
	case workflowRuleReviewFlowProseMismatch:
		return int(anyInt64(finding.Evidence["matching_phrase_count"]))
	case WorkflowRuleRunawaySessionTokens:
		return int(anyInt64(finding.Evidence["session_count"]))
	case workflowRulePinnedWorkerModelStale:
		return int(anyInt64(finding.Evidence["observed_default_sessions"]))
	case workflowRuleSessionMultiplierKills:
		return int(anyInt64(finding.Evidence["session_multiplier_kill_count"]))
	case workflowRuleNoSessionTokenBrake:
		count := int(anyInt64(finding.Evidence["unbraked_session_count"]))
		if count > 0 {
			return count
		}
		return 1
	default:
		return 1
	}
}

func workflowFindingProposalTarget(finding WorkflowOptimizationFinding) (string, string, string) {
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
	case WorkflowRuleRepositoryMergePolicy:
		return "repository", strings.TrimSpace(fmt.Sprint(finding.Evidence["repository"])), strings.TrimSpace(fmt.Sprint(finding.Evidence["fix"]))
	case workflowRuleEmptyModelTelemetry:
		return "telemetry", "codex_sessions.model", "Repair effective-model resolution for completed sessions while leaving the project's pin/default policy unchanged."
	case WorkflowRulePinnedRouteModelRejected:
		return "workflow", workflowModelFindingTargetPath(finding), "Replace the rejected model with a backend-supported pin or remove the pin to inherit the provider default, according to the project's intended model policy."
	case workflowRulePinnedWorkerModelStale:
		return "workflow", workflowModelFindingTargetPath(finding), "Review the configured pin against the observed provider-default generation, then explicitly keep, update, or remove it according to the project's intended model policy."
	case workflowRuleSessionMultiplierKills:
		return "workflow", "agent.max_session_context_multiplier", "Remove or recalibrate the context multiplier and configure an intentional `agent.max_session_tokens` ceiling after human review."
	case workflowRuleNoSessionTokenBrake:
		return "workflow", "agent.max_session_tokens", "Configure an intentional absolute session-token brake after human review."
	case workflowRuleBudgetEstimateDrift:
		return "workflow", "budget", "Review budget estimates against observed p90 billable session tokens and update workflow budget guidance if humans approve."
	case workflowRuleInvalidWorkpadStatusRecurrence:
		return "workflow", "detent-status", "Update WORKFLOW.md Workpad status instructions so agents use only `in_progress`, `blocked`, or `complete` in the `detent-status` block."
	case workflowRuleReviewFlowBehaviorMismatch:
		return workflowReviewFlowProposalTarget(finding)
	case workflowRuleReviewFlowProseMismatch:
		return workflowReviewFlowProposalTarget(finding)
	default:
		return "workflow", "", "Review this repeated doctor finding and propose a WORKFLOW.md, prompt, skill, or gate update for human approval."
	}
}

func workflowModelFindingTargetPath(finding WorkflowOptimizationFinding) string {
	if strings.TrimSpace(fmt.Sprint(finding.Evidence["configured_model_source"])) == "agents.backends.command" {
		return "agents.backends.command"
	}
	return "agents.routes.model"
}

func workflowReviewFlowProposalTarget(finding WorkflowOptimizationFinding) (string, string, string) {
	choice := strings.TrimSpace(fmt.Sprint(finding.Evidence["review_flow"]))
	switch choice {
	case reviewFlowAutopilot:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md with configured autopilot: keep completed work in the active state, set `detent-status` to `complete`, and let Detent promote on the configured gate."
	case reviewFlowReviewGate:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md with configured review-gate: route completed work through the configured review state and remove prose that promises direct review-state skipping."
	default:
		return "workflow", "WORKFLOW.md", "Align WORKFLOW.md handoff prose with the configured review-flow frontmatter after human review."
	}
}

func workflowProposalIssueBody(proposal WorkflowImprovementProposal) string {
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
			fmt.Fprint(&builder, proposal.Evidence[key])
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

func workflowProposalID(projectID string, signalKind string, pattern string) string {
	raw := strings.ToLower(strings.TrimSpace(projectID)) + "|" + strings.ToLower(strings.TrimSpace(signalKind)) + "|" + strings.ToLower(strings.TrimSpace(pattern))
	sum := sha256.Sum256([]byte(raw))
	return "detent-self-improve-" + hex.EncodeToString(sum[:])[:12]
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
