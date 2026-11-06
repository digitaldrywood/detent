package claimstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrInvalidWorkspaceItem = errors.New("workspace item association is invalid")
var ErrInvalidCandidateFact = errors.New("claim candidate fact is invalid")
var ErrInvalidCandidateQuery = errors.New("claim candidate query is invalid")

type WorkspaceItem struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	WorkspaceID    string
	CreatedAt      time.Time
	ClosedAt       *time.Time
}

type CandidateQuery struct {
	ProjectID            tracker.ProjectID
	Scope                string
	WorkItemID           tracker.NativeWorkItemID
	WorkItemIDs          []tracker.NativeWorkItemID
	States               []string
	Authors              []string
	Repositories         []string
	Assignees            []string
	IncludeLabels        []string
	ExcludeLabels        []string
	WorkspaceLane        bool
	RunnerSelection      bool
	PrioritizeUnblockers bool
	StatePriority        []string
	LabelPriority        []string
	AvailableAt          time.Time
	After                CandidateCursor
	AfterWorkItemID      tracker.NativeWorkItemID
	IncludeLeasedAnchor  bool
	Limit                int
}

type CandidateCursor struct {
	Priority       int64
	StateRank      int64
	LabelRank      int64
	AgingBoost     int64
	LandingRank    int64
	LaneRank       int64
	UnblockerRank  int64
	Rank           string
	Created        time.Time
	WorkItemID     tracker.NativeWorkItemID
	SourceRevision int64
}

type SelectionCursor struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	ProjectID      tracker.ProjectID
	SourceRevision string
	Position       CandidateCursor
	Revision       int64
}

type Candidate struct {
	OrganizationID    tracker.OrganizationID
	ProjectID         tracker.ProjectID
	WorkItemID        tracker.NativeWorkItemID
	ID                int64
	Revision          int64
	Number            int64
	State             string
	Author            string
	Repository        string
	Assignees         []string
	Labels            []string
	Rank              string
	Priority          int64
	StateRank         int64
	LabelRank         int64
	AgingBoost        int64
	LandingRank       int64
	LaneRank          int64
	CreatedAt         time.Time
	DispatchWaitSince *time.Time
	Unblockers        int64
	UnblockerRank     int64
}

type CandidatePage struct {
	Items []Candidate
	Next  CandidateCursor
}

type WorkItemFacts struct {
	OrganizationID    tracker.OrganizationID
	ProjectID         tracker.ProjectID
	WorkItemID        tracker.NativeWorkItemID
	Author            string
	Repository        string
	Assignees         []string
	DispatchWaitSince *time.Time
}

type QueueEntry struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	Scope          string
	State          string
	Rank           string
	Priority       *int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ProviderReservation struct {
	OrganizationID tracker.OrganizationID
	LeaseID        tracker.LeaseID
	Reservation    providercapacity.Reservation
}

type LandingCapacityUsage struct {
	Active  int64
	Limit   int64
	Unknown bool
}

type Reader interface {
	CandidatePage(context.Context, CandidateQuery) (CandidatePage, error)
	SelectionCursor(context.Context, string, tracker.ProjectID) (SelectionCursor, error)
	WorkItemFacts(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkItemFacts, error)
	QueueEntry(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (QueueEntry, error)
	IsWorkspaceWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
	ProviderReservation(context.Context, tracker.LeaseID) (ProviderReservation, error)
	ProviderReservationUsage(context.Context, providercapacity.Report, string, time.Time) (int, error)
	LandingCapacityUsage(context.Context, tracker.ProjectID, time.Time) (LandingCapacityUsage, error)
}

type Writer interface {
	Reader
	PutSelectionCursor(context.Context, SelectionCursor, int64) (bool, error)
	CreateWorkItemWithClaimFacts(context.Context, corestore.WorkItem, []string, WorkItemFacts, QueueEntry) (corestore.WorkItem, error)
	PutWorkItemFacts(context.Context, WorkItemFacts) error
	PutQueueEntry(context.Context, QueueEntry) error
	DeleteQueueEntry(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) error
	PutWorkspaceItem(context.Context, WorkspaceItem) error
	CloseWorkspaceItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) error
	CreateProviderReservation(context.Context, ProviderReservation) error
}
