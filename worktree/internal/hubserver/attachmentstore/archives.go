package attachmentstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
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

type StorageArchiveCursor struct {
	TerminalEnteredAt time.Time
	ID                int64
}

type StorageArchiveCandidate struct {
	Stamp  StorageArchiveStamp
	Cursor StorageArchiveCursor
}

type StorageArchivePage struct {
	Items []StorageArchiveCandidate
	Next  StorageArchiveCursor
}

type StorageArchiveStamp struct {
	ProjectID            tracker.ProjectID
	WorkItemID           tracker.NativeWorkItemID
	Revision             int64
	EventSequence        int64
	ConversationSequence int64
}

type StorageArchivePointer struct {
	LastMessageScanned int64
	OrganizationID     tracker.OrganizationID
	Stamp              StorageArchiveStamp
	Root               ArchiveRecord
	ThroughHistory     int64
	PublishedAt        time.Time
}

type StorageArchiveSection struct {
	OrganizationID     tracker.OrganizationID
	Stamp              StorageArchiveStamp
	Kind               string
	Subject            string
	First              int64
	Last               int64
	ID                 string
	Archive            ArchiveRecord
	RetiredThrough     int64
	RetirementComplete bool
}

type StorageArchiveSectionQuery struct {
	SectionID  string
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	Kind       string
	Subject    string
	Before     int64
	At         int64
}

type StorageArchiveSnapshot struct {
	LastMessageScanned int64
	Contents           []conversationstore.ContentObject
	ThroughHistory     int64
	Item               corestore.WorkItem
	Stamp              StorageArchiveStamp
	Previous           *StorageArchivePointer
	History            []eventstore.Event
	Conversation       *conversationstore.Conversation
	Messages           []conversationstore.Message
	Attempt            *leasestore.Attempt
}

type StorageArchiveMessagePayload struct {
	ConversationID    string
	MessageID         string
	Sequence          int64
	Revision          int64
	ExpectedReference string
	Content           conversationstore.ContentObject
}

type StorageArchiveMessageSource struct {
	ConversationID   string
	MessageID        string
	Sequence         int64
	Revision         int64
	ContentReference string
}

type StorageArchivePublication struct {
	ExpectedMessageCursor int64
	LastMessageScanned    int64
	MessageSources        []StorageArchiveMessageSource
	MessagePayloads       []StorageArchiveMessagePayload
	ExpectedRootID        string
	Stamp                 StorageArchiveStamp
	Root                  ArchiveRecord
	Sections              []StorageArchiveSection
	ThroughHistory        int64
	PublishedAt           time.Time
}

type StorageArchiveManifestSection struct {
	Kind      string                      `json:"kind"`
	Subject   string                      `json:"subject_id"`
	First     int64                       `json:"first_sequence"`
	Last      int64                       `json:"last_sequence"`
	ID        string                      `json:"section_id"`
	Reference attachment.ArchiveReference `json:"reference"`
}

type StorageArchiveContentReference struct {
	ID        string                      `json:"id"`
	Reference attachment.ArchiveReference `json:"reference"`
}

type StorageArchiveDocument struct {
	Contents       []StorageArchiveContentReference `json:"content_objects,omitempty"`
	Item           *corestore.WorkItem              `json:"issue,omitempty"`
	Sections       []StorageArchiveManifestSection  `json:"sections,omitempty"`
	Previous       *attachment.ArchiveReference     `json:"previous,omitempty"`
	SchemaVersion  int                              `json:"schema_version"`
	OrganizationID tracker.OrganizationID           `json:"organization_id"`
	ProjectID      tracker.ProjectID                `json:"project_id"`
	WorkItemID     tracker.NativeWorkItemID         `json:"work_item_id"`
	SectionID      string                           `json:"section_id"`
	Kind           string                           `json:"kind"`
	Subject        string                           `json:"subject_id"`
	First          int64                            `json:"first_sequence"`
	Last           int64                            `json:"last_sequence"`
	Messages       []conversationstore.Message      `json:"messages,omitempty"`
	Attempt        *leasestore.Attempt              `json:"attempt,omitempty"`
	History        []eventstore.Event               `json:"history,omitempty"`
	Timeline       []json.RawMessage                `json:"timeline,omitempty"`
}

type StorageArchiveRetirementCursor struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	Revision   int64
	SectionID  string
}

type StorageArchiveRetirementPage struct {
	Items []StorageArchiveSection
	Next  StorageArchiveRetirementCursor
}

type StorageArchiveReader interface {
	StorageArchiveScanCursor(context.Context) (StorageArchiveCursor, error)
	StorageArchiveRetirementCursor(context.Context) (StorageArchiveRetirementCursor, error)
	StorageArchiveCandidates(context.Context, time.Time, StorageArchiveCursor, int) (StorageArchivePage, error)
	StorageArchiveSnapshot(context.Context, StorageArchiveStamp, time.Time, int) (StorageArchiveSnapshot, error)
	StorageArchiveUploader(context.Context, tracker.ProjectID) (string, error)
	StorageArchivePointer(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (StorageArchivePointer, error)
	StorageArchiveSection(context.Context, StorageArchiveSectionQuery) (StorageArchiveSection, error)
	StorageArchiveRetirements(context.Context, StorageArchiveRetirementCursor, int) (StorageArchiveRetirementPage, error)
}

type StorageArchiveRetirementResult struct {
	Processed int
	Complete  bool
}

type StorageArchiveWriter interface {
	AdvanceStorageArchiveScan(context.Context, StorageArchiveCursor, StorageArchiveCursor) (bool, error)
	AdvanceStorageArchiveRetirement(context.Context, StorageArchiveRetirementCursor, StorageArchiveRetirementCursor) (bool, error)
	StorageArchiveReader
	PublishStorageArchive(context.Context, StorageArchivePublication, time.Time) (bool, error)
	RetireStorageArchiveSection(context.Context, StorageArchiveSection, time.Time, int) (StorageArchiveRetirementResult, error)
}
