package leasestore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type PhaseReceiptWriter interface {
	RecordPhaseReceipt(context.Context, tracker.ProjectID, int64, tracker.NativePhaseReceipt) (tracker.NativePhaseReceipt, error)
}

type PhaseReceiptLookup interface {
	PhaseReceiptByKey(context.Context, tracker.ProjectID, int64, tracker.FencingToken, string) (tracker.NativePhaseReceipt, error)
}

type PhaseReceiptReader interface {
	RecentPhaseReceipts(context.Context, tracker.ProjectID, int64) ([]tracker.NativePhaseReceipt, error)
	PhaseContinuation(context.Context, tracker.ProjectID, int64, string, []string, []string) (tracker.NativePhaseContinuation, error)
}
