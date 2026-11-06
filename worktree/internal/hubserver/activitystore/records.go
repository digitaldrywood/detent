package activitystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const PopulationLimit = 10000
const HistoryLimit = 50
const ObjectLimit = 32

type Issue struct {
	ID                        int64
	ProjectID                 tracker.ProjectID
	WorkItemID                tracker.NativeWorkItemID
	Number                    int64
	Title, ProjectName, State string
	CreatedAt                 time.Time
	Partial                   bool
	WorkspaceIDs              []string
	ChangeID, PublicationURL  string
}

type Attempt struct {
	ID                                                    string
	IssueID                                               int64
	ProjectID                                             tracker.ProjectID
	WorkItemID                                            tracker.NativeWorkItemID
	Number                                                int64
	Title, ProjectName, RunnerID, SessionID, Role, Status string
	StartedAt, FinishedAt                                 time.Time
	Live, Selected                                        bool
	Event                                                 *eventstore.Event
}

type Transition struct {
	IssueID  int64
	Sequence int64
	From, To string
	At       time.Time
}

type Snapshot struct {
	Issues      []Issue
	Attempts    []Attempt
	Transitions []Transition
	States      map[tracker.ProjectID][]tracker.NativeState
	Sequences   map[string]int64
	Created     map[int64]bool
	Partial     bool
}

type Filter struct {
	Projects            []tracker.ProjectID
	Principal, RunnerID string
	From, To, Now       time.Time
	Limit               int
}

type Reader interface {
	ActivitySnapshot(context.Context, Filter) (Snapshot, error)
}
