package leasestore

import "context"

type AttemptClaimFacts struct {
	AttemptID           string
	Sequence            int64
	Status              string
	PayloadObjectKey    string
	CheckpointObjectKey *string
	Disposition         string
	UnresolvedOnly      bool
}

func (f AttemptClaimFacts) Valid() bool {
	if f.AttemptID == "" || f.Sequence < 1 || f.Status == "" || f.PayloadObjectKey == "" {
		return false
	}
	switch f.Disposition {
	case "", "progress", "human_action", "permission_wait", "recorded_acceptance":
		return true
	default:
		return false
	}
}

type ClaimFactsWriter interface {
	SetAttemptClaimFacts(context.Context, AttemptClaimFacts) error
}
