package healthstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RunnerObservation struct {
	ID        string
	MachineID string
	State     string
	CreatedAt time.Time
	Heartbeat time.Time
	Leases    int
	FreeSlots int
}

type DependencyObservation struct {
	Dependent    string
	Blocker      string
	SameProject  bool
	Dispatchable bool
	Since        time.Time
}

type OpenItemObservation struct {
	Project          tracker.ProjectID
	Item             tracker.NativeWorkItemID
	Revision         int64
	AttemptID        string
	AttemptStatus    string
	AttemptUpdatedAt time.Time
}

type QuestionObservation struct {
	Project   string
	Item      string
	AttemptID string
	Since     time.Time
}

type UsageObservation struct {
	Item          string
	AttemptID     string
	Cost          float64
	BaselineCost  float64
	BaselineKnown bool
}

type UsagePage struct {
	Values   []UsageObservation
	ReadRows int
	Complete bool
}

type ObservationReader interface {
	HealthArchivedWorkItems(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) (map[tracker.NativeWorkItemID]bool, error)
	HealthUsage(context.Context, tracker.ProjectID, time.Time, time.Time, int, int) (UsagePage, error)
	HealthRunners(context.Context, string, time.Time, int) ([]RunnerObservation, error)
	HealthDependencies(context.Context, tracker.ProjectID, int) ([]DependencyObservation, error)
	HealthOpenItems(context.Context, int) ([]OpenItemObservation, error)
	HealthQuestions(context.Context, time.Time, int) ([]QuestionObservation, error)
	HealthSchedulerEvents(context.Context, time.Time, time.Time, int) ([]eventstore.Event, error)
	HealthRunnerProjects(context.Context, []string, int) (map[string][]tracker.ProjectID, error)
}
