package store

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
)

type AIDebugAttemptReader interface {
	ListIssueAIDebugWorkAttempts(context.Context, IssueIdentity) ([]WorkAttempt, error)
}

type AIDebugSession struct {
	RuntimeIdentity       agentidentity.Identity `json:"runtime_identity,omitzero"`
	ID                    int64                  `json:"id"`
	WorkAttemptID         int64                  `json:"work_attempt_id,omitempty"`
	StartedAt             *time.Time             `json:"started_at,omitempty"`
	CompletedAt           *time.Time             `json:"completed_at,omitempty"`
	InputTokens           int64                  `json:"input_tokens"`
	CachedInputTokens     int64                  `json:"cached_input_tokens"`
	OutputTokens          int64                  `json:"output_tokens"`
	ReasoningOutputTokens int64                  `json:"reasoning_output_tokens"`
	TotalTokens           int64                  `json:"total_tokens"`
	Turns                 int64                  `json:"turns"`
	Model                 string                 `json:"model,omitempty"`
	RequestedModel        string                 `json:"requested_model,omitempty"`
	Effort                string                 `json:"effort,omitempty"`
	RuntimeSeconds        int64                  `json:"runtime_seconds"`
	FinalState            string                 `json:"final_state,omitempty"`
	ResumedFromSessionID  int64                  `json:"resumed_from_session_id,omitempty"`
	ProviderSessionID     string                 `json:"provider_session_id,omitempty"`
}
