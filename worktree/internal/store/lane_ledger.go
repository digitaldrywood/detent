package store

import (
	"context"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
)

type LaneLedgerStore interface {
	LaneWriteLock() sync.Locker
	PrepareLaneWrite(context.Context, string, coordination.LaneWrite) (coordination.LaneWrite, error)
	ResolveLaneWrite(context.Context, uint64, string) error
	LatestLaneWrite(context.Context, IssueIdentity) (coordination.LaneWrite, string, error)
	LaneObservation(context.Context, IssueIdentity) (LaneObservation, error)
	SaveLaneObservation(context.Context, IssueIdentity, LaneObservation) error
}

type LaneObservation struct {
	Origin    string
	State     string
	EnteredAt time.Time
}

// RecordLaneWriteAction stamps a prepared lane write with the acting origin and
// action kind so the operations report can list it without a reason-code namespace.

// ShippedOutcome is a verified delivery recorded by the existing lane writer.
// It does not imply that a worker session ended successfully.
type ShippedOutcome struct {
	IssueIdentity
	State       string
	CompletedAt time.Time
	PRNumber    int64
}

// ShippedOutcomeStore projects deliveries from the lane ledger without creating
// another completion owner or requiring a worker attempt.
type ShippedOutcomeStore interface {
	ShippedOutcomes(context.Context) ([]ShippedOutcome, error)
}

// The applied writer ledger records verified deliverables, unlike observed Done
// cards or successful sessions. Keep the first such outcome for an issue across
// all history so a repeated/imported observation cannot move it to another day.
