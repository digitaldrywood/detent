package tracker

import (
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/markdownfence"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/reviewseverity"
)

type NativePhaseReceipt struct {
	Mutation
	Phase       string `json:"phase"`
	AttemptID   string `json:"attempt_id"`
	Branch      string `json:"branch"`
	HeadSHA     string `json:"head_sha"`
	TreeSHA     string `json:"tree_sha"`
	PolicyID    string `json:"policy_id"`
	InputDigest string `json:"input_digest"`
}

const PlanApprovalQuestion = "Do you approve the implementation plan? Reply with Approve plan or describe the changes needed."

type NativePhaseContinuation struct {
	Plan          *NativePlanState     `json:"plan,omitempty"`
	RepositoryURL string               `json:"repository_url,omitempty"`
	LastPushed    *NativePhaseReceipt  `json:"last_pushed,omitempty"`
	NextPhase     string               `json:"next_phase,omitempty"`
	Receipts      []NativePhaseReceipt `json:"receipts"`
}

type NativePlanState struct {
	LegacySource   string `json:"legacy_source,omitempty"`
	LegacyReview   string `json:"legacy_review,omitempty"`
	LegacyApproval string `json:"legacy_approval,omitempty"`
	InputDigest    string `json:"input_digest,omitempty"`
	ReviewPending  bool   `json:"review_pending,omitempty"`
	Findings       string `json:"findings,omitempty"`
	Draft          string `json:"draft,omitempty"`
	Answer         string `json:"answer,omitempty"`
	AnswerID       string `json:"answer_id,omitempty"`
	Accepted       bool   `json:"accepted,omitempty"`
}

func (p *NativePlanState) NeedsProvider() bool {
	return p != nil && !p.Accepted && (p.ReviewPending || p.Draft == "" || p.Answer != "" && !strings.EqualFold(p.Answer, "approve plan"))
}

func (p *NativePlanState) ResolveLegacyReview(cfg gate.PlanConfig, labels []string) {
	if p == nil || p.LegacySource == "" {
		return
	}
	cfg = gate.EffectivePlan(cfg)
	if reviewseverity.Contains(p.LegacyReview, "P1") {
		p.Findings, p.ReviewPending = p.LegacyReview, true
		return
	}
	for line := range strings.SplitSeq(p.LegacyReview, "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-")), ":")
		if !ok || !slices.Contains([]string{"state", "reviewstate", "review_state", "result"}, strings.ToLower(field)) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "p1", "priority 1", "priority-1", "changes_requested", "requested_changes", "request changes", "changes requested":
			p.Findings, p.ReviewPending = p.LegacyReview, true
			return
		case "approved", "approve", "pass", "passed", "ready", "commented", "p2", "priority 2", "priority-2", "concerns":
			if cfg.Review != gate.PlanReviewHuman {
				p.LegacyApproval = "automated:" + p.LegacySource + ":" + p.LegacyReview
				return
			}
		}
	}
	if cfg.Review != gate.PlanReviewAutomated && slices.ContainsFunc(labels, func(label string) bool {
		return strings.EqualFold(strings.TrimSpace(label), cfg.ApprovalLabel)
	}) {
		p.LegacyApproval = "human:" + p.LegacySource + ":" + cfg.ApprovalLabel
		return
	}
	p.ReviewPending = cfg.Review == gate.PlanReviewAutomated
}

func IsPlanPhase(phase string) bool {
	return phase == "plan" || phase == "plan_draft" || phase == "plan_review"
}

func PhaseInputDigest(phase, head, tree, policyID, reviewState, input string) string {
	return policy.Digest([]byte(strings.Join([]string{phase, head, tree, policyID, reviewState, input}, "\x00")))
}

func PlanIssueDigest(recovery NativeRecovery) string {
	return PhaseInputDigest("direction", "", "", "", NativeStepReviewState(NativeRecovery{Issue: recovery.Issue}), strconv.FormatInt(recovery.HumanDiscussionSequence, 10))
}

func PlanPhaseDigest(phase NativePhaseReceipt, plan NativeStepReceipt) string {
	return PhaseInputDigest(phase.Phase, phase.HeadSHA, phase.TreeSHA, phase.PolicyID, plan.ReviewState, policy.Digest([]byte(plan.CompletionBody))+plan.PlanApproval)
}

func NativeLegacyPlanOutput(output, source string) *NativePlanState {
	var fence markdownfence.Fence
	offset := 0
	for line := range strings.SplitSeq(output, "\n") {
		if !fence.Consume(line) && fence == "" && strings.EqualFold(strings.TrimSpace(line), "## Detent Plan Review") {
			return &NativePlanState{Draft: strings.TrimRight(output[:offset], "\n"), LegacySource: source, LegacyReview: output[offset:]}
		}
		offset += len(line) + 1
	}
	return &NativePlanState{Draft: output, LegacySource: source}
}
