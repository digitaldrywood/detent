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
	Projects          []tracker.ProjectID
	Principal         string
	ActivityWorkItems []tracker.NativeWorkItemID
	From              time.Time
	Split             time.Time
	To                time.Time
	Filter            Filter
	Limit             int
}

type OverviewFact struct {
	ProjectID                   tracker.ProjectID
	WorkItemID                  tracker.NativeWorkItemID
	Title                       string
	CreatedAt                   time.Time
	State                       string
	FilerOriginKnown            bool
	FilerEventID                string
	FilerKnown                  bool
	FilerKind                   string
	FilerID                     string
	CompletedAt                 *time.Time
	CompletedEventID            string
	CompletedActorKnown         bool
	CompletedState              string
	CompletedKind               string
	CompletedID                 string
	CompletedName               string
	PreviousCompletedAt         *time.Time
	PreviousCompletedEventID    string
	PreviousCompletedActorKnown bool
	PreviousCompletedState      string
	PreviousCompletedKind       string
	PreviousCompletedName       string
	PreviousCompletedID         string
	PreviousCycleSeconds        *float64
	CycleSeconds                *float64
	Worked                      bool
	PreviousWorked              bool
	Reworked                    bool
	PreviousReworked            bool
	ChangesLanded               bool
	PreviousChangesLanded       bool
	ActivityObserved            bool
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
	EventID         string
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

type HistoryEventCursor struct {
	After   int64
	Through int64
}

type HistoryEventPage struct {
	Events    []WorkflowEvent
	After     int64
	Examined  int
	Exhausted bool
}

type HistoryIssuePageReader interface {
	ReadHistoryIssueEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, HistoryEventCursor, int) (HistoryEventPage, error)
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

type PriorProjectReader interface {
	HasPriorProjects(context.Context, []tracker.ProjectID, time.Time) (bool, error)
}

type UsageCoverageReader interface {
	ReadReportUsageCoverage(context.Context, OverviewQuery) (bool, error)
}

type AttemptUsageTotal struct {
	Provider, Model                           string
	Input, CachedInput, Output                int64
	ReportedCost                              *float64
	InputPrice, CachedInputPrice, OutputPrice float64
}

type AttemptUsage struct {
	PricingUnavailable               bool `json:"-"`
	EventSequence                    int64
	Termination                      bool
	ReplayEventID, ReplayEventSHA256 string
	OrganizationID                   tracker.OrganizationID
	ProjectID                        tracker.ProjectID
	WorkItemID                       tracker.NativeWorkItemID
	AttemptID, LeaseID               string
	FencingToken, Sequence           int64
	ReceivedAt                       time.Time
	Currency                         string
	Totals                           []AttemptUsageTotal
	Historical                       []billingstore.CostObservation
}

type UsageWriter interface {
	RecordAttemptUsage(context.Context, AttemptUsage) error
	RefreshReportUsage(context.Context, []billingstore.CostObservation) error
}

const AnalyticsAttemptLimit = 1000

type AnalyticsAttemptCursor struct {
	StartedAt time.Time `json:"started_at"`
	Submicro  int16     `json:"submicro"`
	ID        string    `json:"id"`
}

type AnalyticsAttemptQuery struct {
	RunnerIDs       []string
	StartedInWindow bool
	OldestFirst     bool
	ProjectID       tracker.ProjectID
	Principal       string
	RunnerID        string
	From            time.Time
	To              time.Time
	After           AnalyticsAttemptCursor
	Limit           int
}

type AnalyticsAttemptRow struct {
	Role       *string
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	AttemptID  string
	Status     string
	StartedAt  time.Time
	UpdatedAt  time.Time
}

type AnalyticsAttemptPage struct {
	Items    []AnalyticsAttemptRow
	Examined int
	Next     AnalyticsAttemptCursor
	Partial  bool
}

type AnalyticsAttemptReader interface {
	ReadAnalyticsAttemptPage(context.Context, AnalyticsAttemptQuery) (AnalyticsAttemptPage, error)
}

type AnalyticsUsageCursor struct {
	Period    time.Time `json:"period"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	AttemptID string    `json:"attempt_id"`
	Source    int16     `json:"source"`
	SourceKey string    `json:"source_key"`
	Origin    string    `json:"origin,omitempty"`
}

type AnalyticsUsageQuery struct {
	ProjectID tracker.ProjectID
	Principal string
	From      time.Time
	To        time.Time
	After     AnalyticsUsageCursor
	Limit     int
}

type AnalyticsUsagePage struct {
	Items                   []UsageFact
	Examined                int
	Next                    AnalyticsUsageCursor
	Partial                 bool
	ContinuationUnavailable bool
}

type AnalyticsUsageReader interface {
	ReadAnalyticsUsagePage(context.Context, AnalyticsUsageQuery) (AnalyticsUsagePage, error)
}

type CohortReader interface {
	ReadCohortWorkItems(context.Context, OverviewQuery) ([]tracker.NativeWorkItemID, error)
}

type UsageReplay struct {
	Usage      AttemptUsage
	Projection EventProjection
}

type UsageReplayWriter interface {
	ReplayAttemptUsage(context.Context, UsageReplay) (bool, error)
}

type PendingUsageAttempt struct {
	ProjectID          tracker.ProjectID
	WorkItemID         tracker.NativeWorkItemID
	AttemptID          string
	AfterEventSequence int64
}

type UsageReplayReader interface {
	NextPendingUsageAttempt(context.Context, tracker.ProjectID, string) (PendingUsageAttempt, error)
}
