package attachmentstore

import (
	"context"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
	"time"
)

type ArchiveQuery struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	Digest     string
	Binding    *string
	Section    string
	Revision   int64
}

type ArchiveRecord struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ID             string
	Uploader       string
	CreatedAt      time.Time
	Reference      attachment.ArchiveReference
}

type ArchiveReader interface {
	AttachmentArchive(context.Context, ArchiveQuery) (attachment.ArchiveReference, error)
}

type ArchiveWriter interface {
	ArchiveReader
	PrepareAttachmentArchive(context.Context, ArchiveRecord) (bool, error)
	PublishAttachmentArchive(context.Context, ArchiveRecord, tracker.NativeWorkItemID) error
}
