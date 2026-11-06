package viewstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PageCandidate struct {
	After          corestore.RelationshipCursor
	Propagate      bool
	Cleaning       bool
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	RecordID       string
	Kind           string
	Generation     int64
	SourceAt       time.Time
}

type ItemPageRecord struct {
	Value       tracker.NativeIssue
	Generation  int64
	SourceAt    time.Time
	ProjectedAt time.Time
}

type CommentPageRecord struct {
	Value       tracker.NativeComment
	Generation  int64
	SourceAt    time.Time
	ProjectedAt time.Time
}

type WorkItemPageReader interface {
	WorkItemDetailPage(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (ItemPageRecord, error)
	CommentPageRecords(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, bool, int) ([]CommentPageRecord, error)
	NextWorkItemPageCandidate(context.Context) (PageCandidate, error)
}

type WorkItemPageWriter interface {
	AdvanceWorkItemPageCursor(context.Context, PageCandidate) error
	PublishWorkItemPage(context.Context, PageCandidate, *ItemPageRecord, *CommentPageRecord) error
}

type RelationshipPageRecord struct {
	TargetExists bool
	Relationship corestore.Relationship
	WebURL       string
	Identifier   string
}
type RelationshipPageReader interface {
	RelationshipPageRecords(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, corestore.RelationshipCursor, int) ([]RelationshipPageRecord, error)
}
type RelationshipPageWriter interface {
	PublishRelationshipPage(context.Context, PageCandidate, []RelationshipPageRecord, corestore.RelationshipCursor) error
	QueueRelationshipPage(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, bool) error
}

type WorkItemReferenceReader interface {
	WorkItemReferenceRecords(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) ([]tracker.ExternalReference, error)
}

type WorkItemPageRevisionReader interface {
	WorkItemPageProjectionRevision(context.Context) (int64, error)
}
