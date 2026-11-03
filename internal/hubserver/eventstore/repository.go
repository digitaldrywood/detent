package eventstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Event struct {
	PushKnown            bool
	PushChangeID         string
	PushVersionID        string
	PushHeadSHA          string
	PushExternalEffect   string
	PushEffectState      string
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
	LandingWaiting       bool
	LandingChangeID      string
	LandingVersionID     string
	LandingHeadSHA       string
	ProjectSequence      int64
	Report               *reportstore.EventProjection
}

type ProjectChange struct {
	WorkItemID     tracker.NativeWorkItemID
	Revision       int64
	LastActivityAt time.Time
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

type CollaborationVersionKey struct {
	RecordID string
	Revision int64
}

type CollaborationVersionsReader interface {
	CollaborationVersions(context.Context, []CollaborationVersionKey) ([]CollaborationVersion, error)
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
	LandedVersionID     *string
	LandedAt            *time.Time
	LandedHeadSHA       *string
	LandedMergeSHA      *string
	LandedBaseRef       *string
}

type ReviewPolicy struct {
	OrganizationID      tracker.OrganizationID
	ProjectID           tracker.ProjectID
	PolicyRevisionID    string
	ReviewPolicyID      string
	Revision            int64
	UpdatedBy           string
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
	CreatedAt      time.Time
}

type Version struct {
	OrganizationID        tracker.OrganizationID
	ID                    string
	ChangeID              string
	Number                int64
	HeadSHA               string
	BaseSHA               string
	MergeBaseSHA          string
	Repository            string
	PolicyRevisionID      string
	AttemptID             string
	ActorKind             string
	ActorPrincipalID      string
	ExternalProvider      string
	ExternalID            string
	ExternalURL           string
	CreatedAt             time.Time
	PayloadObjectKey      string
	PayloadSHA256         string
	PayloadBytes          int64
	PayloadContentBytes   int64
	PayloadFormatVersion  int64
	SourceObjectKey       string
	SourceSHA256          string
	SourceBytes           int64
	SourceContentBytes    int64
	ImplementingSessionID int64
	ImplementingMachineID string
}

type LandingReceipt struct {
	OrganizationID      tracker.OrganizationID
	AttemptID           string
	VersionID           string
	MergeSHA            string
	Repository          string
	BaseRef             string
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type NativePRPublication struct {
	OrganizationID    tracker.OrganizationID
	VersionID         string
	ChangeID          string
	Repository        string
	BaseRef           string
	Branch            string
	HeadSHA           string
	PolicyID          string
	SourceAttemptID   string
	RecorderAttemptID string
	External          tracker.ChangeExternalReference
}

type ReleaseDelivery struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	RepositoryID   *int64
	tracker.ReleaseDelivery
}

type PolicyLanding struct {
	ChangeID       string
	PullRequestURL string
	Actor          string
}

type ViewedFile struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ChangeID       string
	VersionID      string
	PrincipalID    string
	ManifestSHA256 string
	FileSHA256     string
	Viewed         bool
	UpdatedAt      time.Time
}

type Evidence struct {
	ReviewKind           string
	ReviewLanding        bool
	ReviewSessionKnown   bool
	ReviewSessionID      int64
	ReviewSessionMachine tracker.MachineID
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
	ReportEscape         *reportstore.EscapeProjection
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

type Receipt = receiptstore.Record

type Reader interface {
	ReviewPolicy(context.Context, tracker.ProjectID) (ReviewPolicy, error)
	LandingReceipt(context.Context, string) (LandingReceipt, error)
	LatestLandingReceipt(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (LandingReceipt, error)
	ViewedFiles(context.Context, string, string, int) ([]ViewedFile, error)
	Receipt(context.Context, string, string, string) (Receipt, error)
	Events(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	EventsBefore(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	ProjectEventSequence(context.Context, tracker.ProjectID) (int64, error)
	ProjectChanges(context.Context, tracker.ProjectID, int64, int64, time.Time, int) ([]ProjectChange, error)
	LatestRunEvent(context.Context, tracker.ProjectID, string) (Event, error)
	LatestCheckpointEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, []string) ([]Event, error)
	CollaborationVersion(context.Context, string, int64) (CollaborationVersion, error)
	Change(context.Context, string) (Change, error)
	LatestChange(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (Change, error)
	ProjectPolicyLanding(context.Context, tracker.ProjectID, string, string, string) (PolicyLanding, error)
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
	LockChange(context.Context, string) (Change, error)
	SetReviewPolicy(context.Context, ReviewPolicy, string) error
	CreateLandingReceipt(context.Context, LandingReceipt) (LandingReceipt, error)
	SetViewedFile(context.Context, ViewedFile) (ViewedFile, error)
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

type ChangeRecoveryReader interface {
	ChangeSummaryEvidence(context.Context, string, string, int64, []string) ([]Evidence, bool, error)
	ChangePublicationCheckpoints(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.NativeChangeReference) ([]Event, error)
	ChangePublicationProvenance(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.NativeRunData) (bool, error)
}

type ValidatorReviewReader interface {
	ValidatorReviewEvidence(context.Context, string, string, int64, tracker.MachineID, int64) ([]Evidence, bool, error)
}
