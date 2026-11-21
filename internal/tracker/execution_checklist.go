package tracker

import "time"

type ExecutionCheckKind string

type ExecutionCheckStatus string

const (
	CheckAcceptance        ExecutionCheckKind = "acceptance"
	CheckVerification      ExecutionCheckKind = "verification"
	CheckProjectGate       ExecutionCheckKind = "project_gate"
	CheckStatusGate        ExecutionCheckKind = "status_gate"
	CheckIndependentReview ExecutionCheckKind = "implementation_review"
	CheckSecurityAudit     ExecutionCheckKind = "security_audit"

	CheckUnverified  ExecutionCheckStatus = "unverified"
	CheckNeedsReview ExecutionCheckStatus = "needs_review"
	CheckPassed      ExecutionCheckStatus = "passed"
	CheckFailed      ExecutionCheckStatus = "failed"
	CheckFlaky       ExecutionCheckStatus = "flaky"
	CheckManual      ExecutionCheckStatus = "manual_approved"
	CheckDisclosed   ExecutionCheckStatus = "disclosed"
)

type ExecutionCheck struct {
	Ordinal     int                  `json:"ordinal"`
	Kind        ExecutionCheckKind   `json:"kind"`
	Text        string               `json:"text"`
	CheckDigest string               `json:"check_digest,omitempty"`
	Status      ExecutionCheckStatus `json:"status"`
	Head        string               `json:"head_sha,omitempty"`
	Tree        string               `json:"tree_sha,omitempty"`
	Receipt     string               `json:"receipt_id,omitempty"`
	CheckedAt   time.Time            `json:"checked_at,omitzero"`
}

type ExecutionChecklist struct {
	Revision      string                      `json:"revision"`
	AttemptID     string                      `json:"attempt_id,omitempty"`
	PolicyID      string                      `json:"policy_id"`
	FrozenAt      time.Time                   `json:"frozen_at,omitzero"`
	Checks        []ExecutionCheck            `json:"checks"`
	Interventions []ExecutionLoopIntervention `json:"interventions,omitempty"`
}

type ExecutionCheckUpdate struct {
	Ordinal int                  `json:"ordinal"`
	Status  ExecutionCheckStatus `json:"status"`
	Head    string               `json:"head_sha,omitempty"`
	Tree    string               `json:"tree_sha,omitempty"`
	Receipt string               `json:"receipt_id"`
}

type ExecutionLoopIntervention struct {
	Revision    string    `json:"revision"`
	ThreadID    string    `json:"thread_id"`
	TurnID      string    `json:"turn_id"`
	Action      string    `json:"action"`
	Fingerprint string    `json:"fingerprint"`
	Period      int       `json:"period"`
	Repeats     int       `json:"repeats"`
	Sequence    uint64    `json:"sequence"`
	ObservedAt  time.Time `json:"observed_at,omitzero"`
}
