package eventstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Event struct {
	OrganizationID       tracker.OrganizationID
	ProjectID            tracker.ProjectID
	WorkItemID           tracker.NativeWorkItemID
	ID                   string
	Sequence             int64
	Type                 string
	SchemaVersion        int64
	ActorKind            string
	ActorPrincipalID     string
	EventRevision        int64
	ChangeID             string
	VersionID            string
	HeadSHA              string
	RunID                string
	AttemptID            string
	FromState            string
	ToState              string
	RecordedAt           time.Time
	PayloadObjectKey     string
	PayloadSHA256        string
	PayloadBytes         int64
	PayloadContentBytes  int64
	PayloadFormatVersion int64
}

type CollaborationVersion struct {
	OrganizationID      tracker.OrganizationID
	ProjectID           tracker.ProjectID
	WorkItemID          tracker.NativeWorkItemID
	RecordID            string
	Revision            int64
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type Change struct {
	OrganizationID      tracker.OrganizationID
	ProjectID           tracker.ProjectID
	WorkItemID          tracker.NativeWorkItemID
	ID                  string
	Revision            int64
	Title               string
	CurrentVersionID    *string
	EvidenceSequence    int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type Link struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	ChangeID       string
}

type Version struct {
	OrganizationID       tracker.OrganizationID
	ID                   string
	ChangeID             string
	Number               int64
	HeadSHA              string
	BaseSHA              string
	MergeBaseSHA         string
	Repository           string
	PolicyRevisionID     string
	AttemptID            string
	ActorKind            string
	ActorPrincipalID     string
	ExternalProvider     string
	ExternalID           string
	ExternalURL          string
	CreatedAt            time.Time
	PayloadObjectKey     string
	PayloadSHA256        string
	PayloadBytes         int64
	PayloadContentBytes  int64
	PayloadFormatVersion int64
	SourceObjectKey      string
	SourceSHA256         string
	SourceBytes          int64
	SourceContentBytes   int64
}

type Evidence struct {
	OrganizationID       tracker.OrganizationID
	ChangeID             string
	Sequence             int64
	VersionID            *string
	Kind                 string
	SourceKey            *string
	ActorKind            string
	ActorPrincipalID     string
	Decision             string
	CreatedAt            time.Time
	PayloadObjectKey     string
	PayloadSHA256        string
	PayloadBytes         int64
	PayloadContentBytes  int64
	PayloadFormatVersion int64
}

type LinkedSource struct {
	OrganizationID         tracker.OrganizationID
	ProjectID              tracker.ProjectID
	WorkItemID             tracker.NativeWorkItemID
	URL                    string
	TitleSupplied          bool
	BodySupplied           bool
	TitleOwned             bool
	BodyOwned              bool
	SnapshotObjectKey      string
	SnapshotSHA256         string
	SnapshotBytes          int64
	SnapshotContentBytes   int64
	ExpectedSnapshotSHA256 *string
}

type Receipt struct {
	OrganizationID      tracker.OrganizationID
	ActorID             string
	Operation           string
	CommandKey          string
	RequestHash         string
	CreatedAt           time.Time
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type Reader interface {
	Receipt(context.Context, string, string, string) (Receipt, error)
	Events(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	CollaborationVersion(context.Context, string, int64) (CollaborationVersion, error)
	Change(context.Context, string) (Change, error)
	Changes(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int) ([]Change, error)
	Versions(context.Context, string, int64, int) ([]Version, error)
	Version(context.Context, string, string) (Version, error)
	Evidence(context.Context, string, int64, int) ([]Evidence, error)
	EvidenceBySource(context.Context, string, string, string) (Evidence, error)
	LinkedSource(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (LinkedSource, error)
	LinkedSourceByURL(context.Context, tracker.ProjectID, string) (LinkedSource, error)
	Links(context.Context, string, tracker.NativeWorkItemID, int) ([]Link, error)
}
type Writer interface {
	PutReceipt(context.Context, Receipt) (Receipt, error)
	Reader
	AppendEvent(context.Context, Event) (Event, error)
	CreateCollaborationVersion(context.Context, CollaborationVersion) (CollaborationVersion, error)
	CreateChange(context.Context, Change) (Change, error)
	UpdateChange(context.Context, Change) (Change, error)
	CreateVersion(context.Context, Version) (Version, error)
	AppendEvidence(context.Context, Evidence) (Evidence, error)
	CreateLinkedSource(context.Context, LinkedSource) (LinkedSource, error)
	UpdateLinkedSource(context.Context, LinkedSource) (LinkedSource, error)
	LinkIssue(context.Context, Link) error
}
type Repository interface {
	ReadRecords(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteRecords(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}
