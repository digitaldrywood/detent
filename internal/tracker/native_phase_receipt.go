package tracker

import (
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/policy"
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
	InputDigest   string `json:"input_digest,omitempty"`
	ReviewPending bool   `json:"review_pending,omitempty"`
	Findings      string `json:"findings,omitempty"`
	Draft         string `json:"draft,omitempty"`
	Answer        string `json:"answer,omitempty"`
	AnswerID      string `json:"answer_id,omitempty"`
	Accepted      bool   `json:"accepted,omitempty"`
}

func (p *NativePlanState) NeedsProvider() bool {
	return p != nil && !p.Accepted && (p.ReviewPending || p.Draft == "" || p.Answer != "" && !strings.EqualFold(p.Answer, "approve plan"))
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
