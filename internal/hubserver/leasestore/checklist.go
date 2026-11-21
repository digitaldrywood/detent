package leasestore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChecklistReader interface {
	ExecutionChecklist(context.Context, tracker.ProjectID, string) (*tracker.ExecutionChecklist, error)
}

type ProjectGateFailureEvent struct {
	Event     eventstore.Event
	ReceiptID string
}

type ProjectGateFailureReader interface {
	ProjectGateFailureEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, string, string, string, string) ([]ProjectGateFailureEvent, error)
}

type ChecklistWriter interface {
	ChecklistChangesWriter
	RecordLoopIntervention(context.Context, tracker.ProjectID, string, tracker.ExecutionLoopIntervention) error
	FreezeExecutionChecklist(context.Context, tracker.ProjectID, tracker.ExecutionChecklist) error
}

type ChecklistChangesWriter interface {
	RecordExecutionCheckChanges(context.Context, tracker.ProjectID, string, string, string, time.Time, []tracker.ExecutionCheckUpdate) error
}
