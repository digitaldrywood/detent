package corestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var (
	ErrUniqueViolation      = fmt.Errorf("resource already exists: %w", mutation.ErrConflict)
	ErrSerializationFailure = fmt.Errorf("transaction must be retried: %w", mutation.ErrConflict)
	ErrRevisionConflict     = fmt.Errorf("resource has changed: %w", mutation.ErrConflict)
	ErrNotFound             = errors.New("resource was not found")
	ErrTenantMismatch       = errors.New("resource organization does not match transaction")
	ErrInvalidPage          = errors.New("page limit must be between 1 and 200")
	ErrInvalidCursor        = errors.New("cursor scope or version is invalid; restart the query")
	ErrInvalidRelationship  = errors.New("work item relationship is invalid")
	ErrDependencyCycle      = errors.New("work item dependency would form a cycle")
)

const MaxPageSize = 200
const MaxDuplicateChallengePruneBatch = 5000

type Workload uint8

const (
	Serving Workload = iota
	Background
)

type Repository interface {
	Read(context.Context, tracker.OrganizationID, Workload, func(Reader) error) error
	Write(context.Context, tracker.OrganizationID, Workload, func(Writer) error) error
}

type Reader interface {
	WorkItemHasActiveLease(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) (bool, error)
	NextWorkItemNumber(context.Context, tracker.ProjectID) (int64, error)
	WorkItemBySource(context.Context, tracker.ProjectID, string) (WorkItem, error)
	CommentBySource(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (Comment, error)
	SelectComments(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, bool, int) ([]Comment, error)
	Project(context.Context, tracker.ProjectID) (Project, error)
	WorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkItem, error)
	WorkItemByID(context.Context, tracker.ProjectID, int64) (WorkItem, error)
	WorkItemByNativeID(context.Context, tracker.NativeWorkItemID) (WorkItem, error)
	WorkItemByNumber(context.Context, tracker.ProjectID, int64) (WorkItem, error)
	Comment(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (Comment, error)
	Projects(context.Context, tracker.ProjectID, int) ([]Project, error)
	WorkItems(context.Context, tracker.ProjectID, int64, int) ([]WorkItem, error)
	Comments(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Comment, error)
	WorkItemPage(context.Context, tracker.ProjectID, Cursor, int) (Page[WorkItem], error)
	CommentPage(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, Cursor, int) (Page[Comment], error)
	Relationships(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, RelationshipCursor, int) (RelationshipPage, error)
	LabelNames(context.Context, tracker.ProjectID, LabelCursor, int) (LabelPage, error)
	WorkItemLabels(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, LabelCursor, int) (LabelPage, error)
	ScanWorkItems(context.Context, tracker.ProjectID, ScanCursor, int) (Scan[WorkItem], error)
	ScanComments(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, ScanCursor, int) (Scan[Comment], error)
}

type Writer interface {
	Reader
	LockRelationshipGraph(context.Context) error
	WorkItemBlockers(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, WorkItemBlockerCursor, int) ([]WorkItemBlocker, error)
	LockWorkItems(context.Context, []WorkItemReference) ([]WorkItem, error)
	RemoveOwnedDependencies(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, []WorkItemReference) error
	LockWorkItemProject(context.Context, tracker.ProjectID) error
	LockWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkItem, error)
	CreateProject(context.Context, Project) (Project, error)
	CreateWorkItem(context.Context, WorkItem) (WorkItem, error)
	CreateComment(context.Context, Comment) (Comment, error)
	UpdateProject(context.Context, Project) (Project, error)
	UpdateWorkItem(context.Context, WorkItem) (WorkItem, error)
	UpdateComment(context.Context, Comment) (Comment, error)
	SetRelationship(context.Context, Relationship, string, time.Time) error
	SetWorkItemLabels(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, []string) error
	DeleteProject(context.Context, tracker.ProjectID) error
	DeleteWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) error
	DeleteComment(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) error
}

type Project struct {
	OrganizationID          tracker.OrganizationID
	ID                      tracker.ProjectID
	Name                    string
	Slug                    string
	Profile                 string
	RequireDependencies     bool
	SchedulingRank          int64
	ContentSettingsRevision int64
	WorkflowSource          string
	WorkflowSourceRevision  string
	WorkflowObjectKey       *string
	StatesObjectKey         *string
	ModelSelectionObjectKey *string
	CreatedAt               time.Time
	DeletedAt               *time.Time
}

type WorkItem struct {
	OrganizationID         tracker.OrganizationID
	ID                     int64
	NativeID               tracker.NativeWorkItemID
	ProjectID              tracker.ProjectID
	Number                 int64
	Revision               int64
	EventSequence          int64
	NativeSourceKey        *string
	Title                  string
	BodyObjectKey          *string
	IssueContractObjectKey *string
	ReportContextObjectKey *string
	ActorKind              *string
	ActorPrincipalID       *string
	ProvenanceProvider     *string
	ProvenanceExternalID   *string
	Archived               bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
	NativeCreatedAt        time.Time
	NativeUpdatedAt        time.Time
	WorkflowStateName      *string
	LastActivityAt         time.Time
	IssueType              string
	Priority               *int
	DispatchGeneration     int64
	DispatchRequestedAt    *time.Time
}

type Comment struct {
	OrganizationID       tracker.OrganizationID
	ID                   string
	ProjectID            tracker.ProjectID
	WorkItemID           tracker.NativeWorkItemID
	Revision             int64
	Sequence             int64
	BodyObjectKey        string
	ActorKind            string
	ActorPrincipalID     string
	EditedByKind         *string
	EditedByPrincipalID  *string
	ProvenanceProvider   *string
	ProvenanceExternalID *string
	SourceKey            *string
	Deleted              bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	RecordID             int64
}

type Cursor struct {
	Version        int
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	CreatedAt      time.Time
	ID             int64
}

type Page[T any] struct {
	Items []T
	Next  Cursor
}

type Scan[T any] struct {
	Items []T
	Next  ScanCursor
}

type Relationship struct {
	OrganizationID    tracker.OrganizationID
	ProjectID         tracker.ProjectID
	WorkItemID        tracker.NativeWorkItemID
	RelatedProjectID  tracker.ProjectID
	RelatedWorkItemID tracker.NativeWorkItemID
	Type              string
	Direction         string
	CreatedAt         time.Time
	Number            int
	Title             string
	State             string
	Terminal          bool
	Dispatchable      bool
	ActorPrincipalID  *string
	OrganizationSlug  string
	ProjectSlug       string
	ClosedAt          *time.Time
	StartedAt         *time.Time
}

type WorkItemReference struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
}

type WorkItemBlockerCursor struct {
	ProjectID tracker.ProjectID
	IssueID   int64
}

type WorkItemBlocker struct {
	WorkItemReference
	IssueID         int64
	NativeSourceKey *string
}

type RelationshipCursor struct {
	Type              string
	Direction         string
	RelatedProjectID  tracker.ProjectID
	RelatedWorkItemID tracker.NativeWorkItemID
}

type RelationshipPage struct {
	Items []Relationship
	Next  RelationshipCursor
}

type LabelCursor struct {
	After string
}

type LabelPage struct {
	Items []string
	Next  LabelCursor
}

type ScanCursor struct {
	After   int64
	Through int64
}

type DuplicateCandidate struct {
	WorkItemID tracker.NativeWorkItemID
	Number     int64
	Revision   int64
}

type DuplicateChallenge struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Token          string
	PrincipalID    string
	RequestID      string
	ContentHash    string
	PolicyHash     string
	SourceSequence int64
	Candidates     []DuplicateCandidate
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type DuplicateChallengeReader interface {
	DuplicateChallenge(context.Context, tracker.ProjectID, string) (DuplicateChallenge, error)
}

type DuplicateChallengeWriter interface {
	DuplicateChallengeReader
	CreateDuplicateChallenge(context.Context, DuplicateChallenge) error
	ConsumeDuplicateChallenge(context.Context, DuplicateChallenge, time.Time) (DuplicateChallenge, error)
	PruneDuplicateChallenges(context.Context, time.Time, int) (int64, error)
}
