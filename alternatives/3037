package reportstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrPopulationExceeded = errors.New("report population exceeds its bounded read limit")

type Filter struct {
	Labels     []string
	Priorities []int
	Types      []string
	FiledBy    []string
	Runners    []string
	Models     []string
	Stages     []string
}

type OverviewQuery struct {
	Projects  []tracker.ProjectID
	Principal string
	From      time.Time
	Split     time.Time
	To        time.Time
	Filter    Filter
	Limit     int
}

type OverviewFact struct {
	ProjectID              tracker.ProjectID
	WorkItemID             tracker.NativeWorkItemID
	Title                  string
	CreatedAt              time.Time
	State                  string
	FilerKind              string
	FilerID                string
	CompletedAt            *time.Time
	CompletedState         string
	CompletedKind          string
	CompletedID            string
	CompletedName          string
	PreviousCompletedAt    *time.Time
	PreviousCompletedState string
	PreviousCompletedKind  string
	PreviousCompletedID    string
	PreviousCycleSeconds   *float64
	CycleSeconds           *float64
	Worked                 bool
	PreviousWorked         bool
	Reworked               bool
	PreviousReworked       bool
	ChangesLanded          bool
	PreviousChangesLanded  bool
	ActivityObserved       bool
}

type Facets struct {
	Labels      []string
	FiledBy     []Facet
	Runners     []Facet
	Models      []string
	Stages      []string
	Unavailable []string
}

type Facet struct {
	ID   string
	Name string
}

type AttemptQuery struct {
	Projects  []tracker.ProjectID
	Principal string
	From      time.Time
	To        time.Time
	Filter    Filter
	Limit     int
}

type AttemptFact struct {
	OutcomeAt       time.Time
	ProjectID       tracker.ProjectID
	WorkItemID      tracker.NativeWorkItemID
	AttemptID       string
	RunnerID        string
	RunnerName      string
	Status          string
	StartedAt       time.Time
	UpdatedAt       time.Time
	Role            string
	Model           string
	Effort          string
	DurationSeconds float64
	ProjectionKnown bool
	Failed          bool
	FailureClass    string
	FailureKind     string
	LandingReceipt  bool
	Landed          bool
	LandingChangeID string
}

type WorkflowEvent struct {
	ID        string
	VersionID string
	At        time.Time
	Type      string
	FromState string
	ToState   string
	Class     Class
}

type HistoryIssue struct {
	WorkItemID   tracker.NativeWorkItemID
	CreatedAt    time.Time
	InitialState string
	Events       []WorkflowEvent
}

type ProjectHistory struct {
	ProjectID tracker.ProjectID
	States    []tracker.NativeState
	Issues    []HistoryIssue
}

type HistoryReader interface {
	ReadHistory(context.Context, OverviewQuery) ([]ProjectHistory, error)
}

type LandingFact struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	ChangeID   string
	At         time.Time
	Attempts   int
	Complete   bool
	VersionID  string
	HeadSHA    string
	MergeSHA   string
	BaseRef    string
}

type EscapeProjection struct {
	OccurrenceID  string
	Kind          string
	Cause         string
	ObservedAt    time.Time
	InstanceID    string
	HumanOverride bool
}

type EscapeFact struct {
	EscapeProjection
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	ChangeID   string
	VersionID  string
	CreatedAt  time.Time
	ActorKind  string
	ActorID    string
}

type QualityReader interface {
	ReadEscapeFacts(context.Context, OverviewQuery) ([]EscapeFact, error)
}

type LandingReader interface {
	ReadLandingFacts(context.Context, OverviewQuery) ([]LandingFact, error)
}

type OpenQuery struct {
	Projects  []tracker.ProjectID
	Principal string
	At        time.Time
	Filter    Filter
	Limit     int
}

type OpenFact struct {
	ProjectID     tracker.ProjectID
	WorkItemID    tracker.NativeWorkItemID
	Title         string
	State         string
	Terminal      bool
	EnteredAt     *time.Time
	FirstWorkedAt *time.Time
}

type OpenReader interface {
	ReadOpenFacts(context.Context, OpenQuery) ([]OpenFact, error)
}

type AttemptReader interface {
	ReadAttemptFacts(context.Context, AttemptQuery) ([]AttemptFact, error)
}

type CostKey struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	From       time.Time
	To         time.Time
}

type CostReader interface {
	ReadIssueCosts(context.Context, []CostKey) (map[CostKey]float64, error)
}

type UsageFact struct {
	ProjectID   tracker.ProjectID
	WorkItemID  tracker.NativeWorkItemID
	AttemptID   string
	Period      time.Time
	Provider    string
	Model       string
	Input       int64
	CachedInput int64
	Output      int64
	Cost        float64
	Currency    string
	RunnerID    string
	RunnerName  string
}

type UsageRecord struct {
	OrganizationID tracker.OrganizationID
	UsageFact
	UpdatedAt time.Time
}

type UsageReader interface {
	ReadReportUsage(context.Context, OverviewQuery) ([]UsageFact, error)
}

type Reader interface {
	ReadOverviewFacts(context.Context, OverviewQuery) ([]OverviewFact, error)
	ReadFacets(context.Context, []tracker.ProjectID, time.Time, time.Time, string, int) (Facets, error)
	HasPriorData(context.Context, []tracker.ProjectID, time.Time, string) (bool, error)
}

type UsageCoverageReader interface {
	ReadReportUsageCoverage(context.Context, OverviewQuery) (bool, error)
}

type UsageWriter interface {
	RefreshReportUsage(context.Context, []billingstore.CostObservation) error
}
