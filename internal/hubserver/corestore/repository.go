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
)

const MaxPageSize = 200

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
	Project(context.Context, tracker.ProjectID) (Project, error)
	WorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (WorkItem, error)
	Comment(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (Comment, error)
	Projects(context.Context, tracker.ProjectID, int) ([]Project, error)
	WorkItems(context.Context, tracker.ProjectID, int64, int) ([]WorkItem, error)
	Comments(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Comment, error)
}

type Writer interface {
	Reader
	CreateProject(context.Context, Project) (Project, error)
	CreateWorkItem(context.Context, WorkItem) (WorkItem, error)
	CreateComment(context.Context, Comment) (Comment, error)
	UpdateProject(context.Context, Project) (Project, error)
	UpdateWorkItem(context.Context, WorkItem) (WorkItem, error)
	UpdateComment(context.Context, Comment) (Comment, error)
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
}
