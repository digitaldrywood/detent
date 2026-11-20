package leasestore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChecklistReader interface {
	ExecutionChecklist(context.Context, tracker.ProjectID, string) (*tracker.ExecutionChecklist, error)
}

type ChecklistWriter interface {
	ChecklistChangesWriter
	RecordLoopIntervention(context.Context, tracker.ProjectID, string, tracker.ExecutionLoopIntervention) error
	FreezeExecutionChecklist(context.Context, tracker.ProjectID, tracker.ExecutionChecklist) error
}

type ChecklistChangesWriter interface {
	RecordExecutionCheckChanges(context.Context, tracker.ProjectID, string, string, string, time.Time, []tracker.ExecutionCheckUpdate) error
}
