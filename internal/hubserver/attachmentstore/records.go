package attachmentstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Record struct {
	OrganizationID    tracker.OrganizationID
	Metadata          attachment.Metadata
	EvidenceAttemptID string
	EvidenceCaption   string
	ObjectDeletedAt   *time.Time
	Kind              string
	Retention         string
	ObjectKey         string
	UncompressedSize  int64
	Codec             string
	SchemaVersion     int64
	StorageBindingID  string
	CustomerOwned     bool
	PendingContent    []byte
}

type Reference struct {
	ProjectID          tracker.ProjectID
	AttachmentID       string
	WorkItemID         tracker.NativeWorkItemID
	CommentID          string
	RetainWithWorkItem bool
}

type Reader interface {
	ArchiveReader
	Attachment(context.Context, tracker.ProjectID, string) (Record, error)
	AttachmentReferences(context.Context, tracker.ProjectID, string, int) ([]Reference, error)
	AttachmentEvidence(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int) ([]Record, error)
}
type LockingReader interface {
	LockAttachment(context.Context, tracker.ProjectID, string) (Record, error)
}

type Writer interface {
	ArchiveWriter
	Reader
	CreateAttachment(context.Context, Record) error
	DeleteIconAttachment(context.Context, tracker.ProjectID, string, time.Time) error
	BindAttachment(context.Context, Reference) error
}

type DeletionWriter interface {
	DeleteAttachment(context.Context, tracker.ProjectID, string, time.Time) error
	FinishAttachmentDeletion(context.Context, string, time.Time) error
}

type ExpiryReader interface {
	AttachmentDeletionCandidates(context.Context, time.Time, int) ([]Record, error)
}

type ExpiryWriter interface {
	ExpireAttachment(context.Context, tracker.ProjectID, string, time.Time, time.Time) (bool, error)
}
