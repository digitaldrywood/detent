package reportstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type SummaryAttentionQuery struct {
	Project              tracker.ProjectID
	Principal, Recipient string
	From, To             time.Time
	Limit                int
}

type SummaryAttentionCandidate struct {
	WorkItemID        tracker.NativeWorkItemID
	At                time.Time
	Attempts, Reworks int64
}

type SummaryAttentionReader interface {
	ReadSummaryAttention(context.Context, SummaryAttentionQuery) ([]SummaryAttentionCandidate, error)
}
