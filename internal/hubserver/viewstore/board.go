package viewstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type BoardPosition struct {
	Lane            int
	State           string
	Priority        int
	ActivityMissing bool
	Activity        *time.Time
	Identifier      string
	SearchExact     bool
	SearchRank      float64
	SearchNumber    int64
}

type BoardDisplay struct {
	Ready             bool
	Exact             bool
	Provenance        *tracker.Provenance
	ChangePrincipalID string
	Title             string
	Number            int
	Actor             tracker.Actor
	Reason            string
	StatusLine        string
	Attempt           *tracker.NativeBoardAttempt
	Execution         *tracker.NativeBoardExecution
	Change            *tracker.NativeBoardChange
}

type BoardReporterRelations struct {
	ReporterStatus  string
	Dependencies    []tracker.NativeWorkItemID
	Blockers        []tracker.NativeDependency
	Dependents      []tracker.NativeDependency
	Related         []tracker.NativeDependency
	DependencyReady bool
	BlockedReason   tracker.DependencyReason
	UnblocksReason  tracker.DependencyReason
}

type BoardRecord struct {
	ListIssue             *tracker.NativeIssue
	ListBytes             int
	WindowMask            int
	RefreshAfter          *time.Time
	ReporterRelations     *BoardReporterRelations
	PageIssue             *tracker.NativeIssue
	ProjectedAt           time.Time
	Display               BoardDisplay
	OrganizationID        tracker.OrganizationID
	ProjectID             tracker.ProjectID
	WorkItemID            tracker.NativeWorkItemID
	SourceRevision        int64
	SourceEventSequence   int64
	SourceProjectRevision int64
	Position              BoardPosition
	Archived              bool
	Terminal              bool
	Running               bool
	Dispatchable          bool
	ClosedAt              *time.Time
	ExpiresAt             *time.Time
	ExecutionFence        int64
	ActorPrincipalID      string
	IssueType             string
	VerificationState     string
	Priority              *int
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Payload               attachment.ArchiveReference
	Labels                []string
	Assignees             []string
}

type BoardCandidate struct {
	QueuedAt   *time.Time
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	Required   bool
}

type BoardSelection struct {
	Summary             bool
	Compact             bool
	SearchText          string
	SearchRanked        bool
	Fingerprint         string
	ExcludeWorkspace    bool
	NativeCreatedAfter  *time.Time
	NativeCreatedBefore *time.Time
	NativeUpdatedAfter  *time.Time
	WorkItemIDs         []tracker.NativeWorkItemID
	ProjectID           tracker.ProjectID
	ActorPrincipalID    string
	Reporter            bool
	ReporterPrincipalID string
	Archived            *bool
	Terminal            *bool
	States              []string
	Types               []string
	Verification        []string
	Labels              []string
	Assignees           []string
	Priorities          []int
	CreatedAfter        *time.Time
	CreatedBefore       *time.Time
	UpdatedAfter        *time.Time
	CompletedFrom       *time.Time
	CompletedTo         *time.Time
	ClosedOrder         bool
	WorkOrder           bool
	After               *BoardPosition
	AfterSequence       *int64
	Limit               int
}

func (s BoardSelection) SupportsSummary() bool {
	return s.SearchText == "" && s.Fingerprint == "" && s.ActorPrincipalID == "" &&
		len(s.Types)+len(s.Verification)+len(s.Labels)+len(s.Assignees)+len(s.Priorities) == 0 &&
		s.NativeCreatedAfter == nil && s.NativeCreatedBefore == nil && s.NativeUpdatedAfter == nil &&
		s.CompletedFrom == nil && s.CompletedTo == nil
}

type BoardLane struct {
	State   string
	Total   int64
	Running int64
}

type BoardClosedCount struct {
	Window int
	State  string
	Total  int64
}

type BoardSequence struct {
	Sequence    int64
	ReplayFloor int64
}

type BoardDelta struct {
	Sequence   int64
	WorkItemID tracker.NativeWorkItemID
	Previous   *attachment.ArchiveReference
	Current    *attachment.ArchiveReference
}

type BoardUpdate struct {
	ProjectID               tracker.ProjectID
	WorkItemID              tracker.NativeWorkItemID
	ExpectedRevision        int64
	ExpectedEventSequence   int64
	ExpectedProjectRevision int64
	Current                 *BoardRecord
}

type BoardReader interface {
	BoardAttemptCount(context.Context, int64) (int64, error)
	BoardAccessRevisions(context.Context, string, string) ([3]int64, error)
	NextBoardCandidate(context.Context, tracker.NativeWorkItemID, ...tracker.NativeWorkItemID) (BoardCandidate, error)
	BoardItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (BoardRecord, error)
	BoardPage(context.Context, BoardSelection) ([]BoardRecord, error)
	BoardLanes(context.Context, BoardSelection, time.Time) ([]BoardLane, error)
	BoardClosedWindows(context.Context, BoardSelection, []time.Time, time.Time) ([]BoardClosedCount, error)
	BoardSequence(context.Context, tracker.ProjectID) (BoardSequence, error)
	BoardDeltas(context.Context, tracker.ProjectID, int64, int) ([]BoardDelta, error)
}

type BoardWriter interface {
	BoardReader
	ReplaceBoardItem(context.Context, BoardUpdate) error
}
