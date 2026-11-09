package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/compute"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type SessionFinish struct {
	TurnStartRefused      bool
	CompletedAt           time.Time
	Turns                 int64
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
	RuntimeSeconds        int64
	FinalState            string
	Model                 string
	ProviderThreadID      string
	ProviderSessionID     string
	ResumedFromSessionID  int64
	RuntimeIdentity       agentidentity.Identity
	SkillDraftProposed    bool
}

type UsageEvent struct {
	Compute                *compute.Usage
	ProjectID              string
	RunID                  int64
	SessionID              int64
	IssueID                string
	Identifier             string
	PRNumber               *int64
	Model                  string
	InputTokens            int64
	CachedInputTokens      int64
	OutputTokens           int64
	ReasoningOutputTokens  int64
	TotalTokens            int64
	ModelContextWindow     *int64
	CostUSD                float64
	ProjectedCostUSD       *float64
	ProjectionOvershootUSD float64
	RuntimeSeconds         int64
	StartedAt              time.Time
	FinishedAt             time.Time
	Outcome                string
}

type Finalization struct {
	SessionID int64
	Finish    SessionFinish
	Usage     UsageEvent
	Phase     *workflowmetrics.PhaseEvent
}

func (f Finalization) Equal(other Finalization) bool {
	left, leftErr := json.Marshal(f)
	right, rightErr := json.Marshal(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func (f Finalization) Key(stage string) (string, error) {
	if stage != "finish" && stage != "usage" && stage != "phase" {
		return "", errors.New("invalid session finalization stage")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("session:%d:%s:%x", f.SessionID, stage, sha256.Sum256(raw)), nil
}
