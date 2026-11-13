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
	SchedulerOutcome     string
	HistoryHidden        bool
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
	ActorEnvelopeKnown   bool
	ActorViaKey          string
	ActorDisplayName     string
	ActorAgent           string
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

type ProjectEventPageReader interface {
	ProjectEvents(context.Context, tracker.ProjectID, int64, int) ([]Event, error)
}

type ProgressEvidenceReader interface {
	ProgressEvidenceSequence(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (int64, error)
}

type HistoryReader interface {
	HistoryEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	HistoryEventsBefore(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
}

type ProjectChange struct {
	WorkItemID     tracker.NativeWorkItemID
	Revision       int64
	LastActivityAt time.Time
}

type CollaborationVersion struct {
	SourceKnown         bool
	SourceScheduled     bool
	SourceObservedAt    time.Time
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

type EventKey struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	ID         string
}

type EventsByKeyReader interface {
	EventsByKey(context.Context, []EventKey) (map[string]Event, error)
}

type Change struct {
	OrganizationID         tracker.OrganizationID
	ProjectID              tracker.ProjectID
	WorkItemID             tracker.NativeWorkItemID
	ID                     string
	Revision               int64
	Title                  string
	CurrentVersionID       *string
	EvidenceSequence       int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
	PayloadObjectKey       string
	PayloadSHA256          string
	PayloadBytes           int64
	PayloadContentBytes    int64
	LandedVersionID        *string
	LandedAt               *time.Time
	LandedHeadSHA          *string
	LandedMergeSHA         *string
	LandedBaseRef          *string
	ReviewSummarySequence  int64
	ReviewEvidenceRevision int64
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
	FrozenLanding         *VersionLanding `json:"-"`
	LandingHistoryKnown   bool            `json:"-"`
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
	Native         *VersionLanding
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
	RunEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (Event, error)
	LatestEventRecordedAt(context.Context) (*time.Time, error)
	HistoryReader
	PRPublication(context.Context, string) (NativePRPublication, error)
	ReleaseDelivery(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (ReleaseDelivery, error)
	ReviewPolicy(context.Context, tracker.ProjectID) (ReviewPolicy, error)
	LandingReceipt(context.Context, string) (LandingReceipt, error)
	LatestLandingReceipt(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (LandingReceipt, error)
	ViewedFiles(context.Context, string, string, int) ([]ViewedFile, error)
	Receipt(context.Context, string, string, string) (Receipt, error)
	ReplayReceipt(context.Context, string, string, string) (Receipt, error)
	Events(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	EventsBefore(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int64, int) ([]Event, error)
	ProjectEventSequence(context.Context, tracker.ProjectID) (int64, error)
	ProjectChanges(context.Context, tracker.ProjectID, int64, int64, time.Time, int) ([]ProjectChange, error)
	LatestRunEvent(context.Context, tracker.ProjectID, string) (Event, error)
	LatestCheckpointEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, []string) ([]Event, error)
	CollaborationVersion(context.Context, string, int64) (CollaborationVersion, error)
	Change(context.Context, string) (Change, error)
	LatestChange(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (Change, error)
	WorkItemPullRequestVersion(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (PullRequestVersion, error)
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
	PutPRPublication(context.Context, NativePRPublication) error
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
	RecordVersionLanding(context.Context, VersionLanding) error
	ConfirmVersionUnlanded(context.Context, string, string, string) error
	AppendEvidence(context.Context, Evidence) (Evidence, error)
	CreateLinkedSource(context.Context, LinkedSource) (LinkedSource, error)
	UpdateLinkedSource(context.Context, LinkedSource) (LinkedSource, error)
	LinkIssue(context.Context, Link) error
}
type Repository interface {
	ReadRecords(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteRecords(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type PullRequestVersion struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	ChangeID       string
	VersionID      string
	Number         int64
	HeadSHA        string
	Repository     string
	PolicyID       string
	AttemptID      string
	External       tracker.ChangeExternalReference
}

type ChangeRecoveryReader interface {
	ChangeSummaryEvidence(context.Context, string, string, int64, []string) ([]Evidence, bool, error)
	RecoveryChangeEvidence(context.Context, string, string, int64, []string) ([]Evidence, bool, error)
	ChangePublicationCheckpoints(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.NativeChangeReference) ([]Event, error)
	ChangePublicationProvenance(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.NativeRunData) (bool, error)
}

type ChangeGateEvidenceReader interface {
	CurrentVersionGateEvidence(context.Context, string, string, int64) ([]Evidence, error)
}

type ChangeReviewEvidenceReader interface {
	ChangeReviewEvidence(context.Context, string, string, int64, []string, string) ([]Evidence, bool, error)
}

type ValidatorReviewReader interface {
	ValidatorReviewEvidence(context.Context, string, string, int64, tracker.MachineID, int64) ([]Evidence, bool, error)
}

type ChangeLandingReader interface {
	LatestVersionLandingEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.NativeChangeReference) (LandingObservation, error)
}

type LandingObservation struct {
	FromState   string
	Event       Event
	TargetState string
}

type WorkflowTransitionReader interface {
	LatestWorkflowTransition(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (Event, error)
	LatestSchedulerDecision(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (Event, error)
}

type SchedulerDecisionRows struct {
	Available   bool
	Latest      *Event
	Exclusion   *Event
	Unprojected *Event
}
type SchedulerDecisionReader interface {
	SchedulerDecisions(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (SchedulerDecisionRows, error)
	SchedulerDecision(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (Event, error)
	LatestClaimedSchedulerDecision(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (Event, error)
}

type RunEventBatchReader interface {
	LatestRunEvents(context.Context, tracker.ProjectID, []string) ([]Event, error)
}

type AnalyticsSchedulerCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

type AnalyticsSchedulerQuery struct {
	ProjectID tracker.ProjectID
	Principal string
	From      time.Time
	To        time.Time
	Outcome   string
	After     AnalyticsSchedulerCursor
	Limit     int
}

type AnalyticsSchedulerPage struct {
	Events   []Event
	Prior    map[string]Event
	Examined int
	Next     AnalyticsSchedulerCursor
	Partial  bool
}

type AnalyticsSchedulerReader interface {
	ReadAnalyticsSchedulerPage(context.Context, AnalyticsSchedulerQuery) (AnalyticsSchedulerPage, error)
}

type VersionLanding struct {
	OrganizationID       tracker.OrganizationID
	ProjectID            tracker.ProjectID
	WorkItemID           tracker.NativeWorkItemID
	ChangeID             string
	VersionID            string
	At                   time.Time
	HeadSHA              string
	MergeSHA             string
	BaseRef              string
	PayloadObjectKey     string
	PayloadSHA256        string
	PayloadBytes         int64
	PayloadContentBytes  int64
	PayloadFormatVersion int16
}

type VersionLandingReader interface {
	VersionLanding(context.Context, string, string) (VersionLanding, error)
}

func VersionLandingFromChange(row Change) (VersionLanding, error) {
	if row.OrganizationID == "" || row.ProjectID == "" || row.WorkItemID == "" || row.ID == "" || row.LandedVersionID == nil || *row.LandedVersionID == "" || row.LandedAt == nil || row.LandedAt.IsZero() || row.LandedHeadSHA == nil || *row.LandedHeadSHA == "" || row.LandedMergeSHA == nil || *row.LandedMergeSHA == "" || row.LandedBaseRef == nil || *row.LandedBaseRef == "" {
		return VersionLanding{}, ErrInvalidRecord
	}
	return VersionLanding{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ChangeID: row.ID, VersionID: *row.LandedVersionID, At: *row.LandedAt, HeadSHA: *row.LandedHeadSHA, MergeSHA: *row.LandedMergeSHA, BaseRef: *row.LandedBaseRef, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes, PayloadFormatVersion: 1}, nil
}

type AnalyticsResidenceQuery struct {
	ProjectID           tracker.ProjectID
	Principal           string
	From, To            time.Time
	Limit               int
	Live                bool
	IssuesAfter         tracker.NativeWorkItemID
	EventsAfterItem     tracker.NativeWorkItemID
	EventsAfterSequence int64
}

type AnalyticsResidenceIssue struct {
	WorkItemID   tracker.NativeWorkItemID
	CreatedAt    time.Time
	InitialState string
}

type AnalyticsResidencePage struct {
	States                 []tracker.NativeState
	Issues                 []AnalyticsResidenceIssue
	Events                 []Event
	IssuesAfter            tracker.NativeWorkItemID
	EventsAfterItem        tracker.NativeWorkItemID
	EventsAfterSequence    int64
	IssuesMore, EventsMore bool
}

type AnalyticsResidenceReader interface {
	ReadAnalyticsResidencePage(context.Context, AnalyticsResidenceQuery) (AnalyticsResidencePage, error)
}

type AnalyticsLandingCursor struct {
	At                  time.Time
	ChangeID, VersionID string
}
type AnalyticsLandingQuery struct {
	ProjectID       tracker.ProjectID
	Principal       string
	From, To        time.Time
	Limit           int
	AllVersions     bool
	After           AnalyticsLandingCursor
	ReopenedItems   []tracker.NativeWorkItemID
	EscapedVersions []string
}
type AnalyticsLandingPage struct {
	Items                   []VersionLanding
	Next                    AnalyticsLandingCursor
	Partial, HistoryUnknown bool
}
type AnalyticsLandingReader interface {
	ReadAnalyticsLandingPage(context.Context, AnalyticsLandingQuery) (AnalyticsLandingPage, error)
}

type AnalyticsWorkflowCursor struct {
	At       time.Time
	Sequence int64
	ID       string
}
type AnalyticsWorkflowQuery struct {
	ProjectID tracker.ProjectID
	Principal string
	From, To  time.Time
	Limit     int
	After     AnalyticsWorkflowCursor
}
type AnalyticsWorkflowRow struct {
	Event      Event
	ToTerminal bool
}
type AnalyticsWorkflowPage struct {
	Items   []AnalyticsWorkflowRow
	Next    AnalyticsWorkflowCursor
	Partial bool
}
type AnalyticsWorkflowReader interface {
	ReadAnalyticsWorkflowPage(context.Context, AnalyticsWorkflowQuery) (AnalyticsWorkflowPage, error)
}

type AnalyticsEvidenceCursor struct {
	At                           time.Time
	Sequence                     int64
	ChangeID                     string
	VersionAfter, VersionThrough int64
}
type AnalyticsEvidenceQuery struct {
	ProjectID tracker.ProjectID
	Principal string
	From, To  time.Time
	Limit     int
	After     AnalyticsEvidenceCursor
}
type AnalyticsEvidenceRow struct {
	WorkItemID tracker.NativeWorkItemID
	Evidence   Evidence
	Escape     *reportstore.EscapeProjection
}
type AnalyticsEvidencePage struct {
	Items   []AnalyticsEvidenceRow
	Next    AnalyticsEvidenceCursor
	Partial bool
}
type AnalyticsEvidenceReader interface {
	ReadAnalyticsEvidencePage(context.Context, AnalyticsEvidenceQuery) (AnalyticsEvidencePage, error)
}

type QualityEscapeReader interface {
	QualityEscapeOccurrence(context.Context, string, string, string) (Evidence, error)
}

type ScheduledSourceReader interface {
	ScheduledSources(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) ([]CollaborationVersion, bool, error)
}

type AttemptEventPageReader interface {
	AttemptEvents(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int64, int) ([]Event, error)
}
