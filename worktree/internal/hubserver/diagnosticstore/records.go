package diagnosticstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const Limit = 10000
const ObjectLimit = 16
const AttemptLimit = 1000

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

type Attempt struct {
	RunnerID   string
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	ID         string
	Status     string
	StartedAt  time.Time
	UpdatedAt  time.Time
	FailureAt  *time.Time
	Event      eventstore.Event
}

type Usage struct {
	AttemptID string
	Period    time.Time
	Cost      float64
	Tokens    int64
	Complete  bool
}

type Landing struct {
	ChangeID   string
	WorkItemID tracker.NativeWorkItemID
	At         time.Time
	EnteredAt  *time.Time
}

type Snapshot struct {
	AttemptRecords   []Attempt
	Usage            []Usage
	Landings         []Landing
	PriorDecisions   map[string]eventstore.Event
	UsagePartial     bool
	AttemptsPartial  bool
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

type AnalyticsFailureCursor struct {
	At time.Time
	ID string
}
type AnalyticsFailureQuery struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	Principal  string
	From, To   time.Time
	Limit      int
	After      AnalyticsFailureCursor
}
type AnalyticsFailurePage struct {
	Attempts                   []Attempt
	Next                       AnalyticsFailureCursor
	Partial, ProjectionMissing bool
}
type AnalyticsFailureReader interface {
	ReadAnalyticsFailurePage(context.Context, AnalyticsFailureQuery) (AnalyticsFailurePage, error)
}
