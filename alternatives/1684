package diagnosticstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const Limit = 10000
const ObjectLimit = 16

type Issue struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	State      string
	CreatedAt  time.Time
}

type Lease struct {
	AcquiredAt time.Time
	FinishedAt time.Time
}

type Snapshot struct {
	Events           []eventstore.Event
	Issues           []Issue
	Leases           []Lease
	Attempts         int
	Partial          bool
	ResidencePartial bool
	SchedulerPartial bool
}

type Reader interface {
	DiagnosticsSnapshot(context.Context, []tracker.ProjectID, time.Time, time.Time, time.Time, string) (Snapshot, error)
}
