package progresscheck

import (
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	RoundContinue    = "continue"
	RoundDone        = "done"
	RoundHumanAction = "human_action"
)

type RoundInput struct {
	RoundID          string                   `json:"round_id,omitempty"`
	Phase            string                   `json:"phase,omitempty"`
	HeadSHA          string                   `json:"head_sha,omitempty"`
	TreeSHA          string                   `json:"tree_sha,omitempty"`
	FailureSignature string                   `json:"failure_signature,omitempty"`
	Criteria         []gate.CriterionEvidence `json:"criterion_evidence,omitempty"`
	Action           string                   `json:"action,omitempty"`
	NextPhase        string                   `json:"next_phase,omitempty"`
	Reason           string                   `json:"reason,omitempty"`
	HumanAction      string                   `json:"human_action,omitempty"`
}

type RoundVerdict struct {
	RoundInput
	LeaseID          tracker.LeaseID      `json:"lease_id"`
	FencingToken     tracker.FencingToken `json:"fencing_token,string"`
	AttemptID        string               `json:"attempt_id"`
	PolicyID         string               `json:"policy_id"`
	PreviousRoundID  string               `json:"previous_round_id,omitempty"`
	CriteriaRevision string               `json:"criteria_revision"`
	FailedCriteria   []string             `json:"failed_criteria,omitempty"`
	VerifiedCriteria []string             `json:"verified_criteria,omitempty"`
	Regressed        bool                 `json:"regressed"`
	Advanced         bool                 `json:"advanced"`
	Disposition      string               `json:"disposition,omitempty"`
}
