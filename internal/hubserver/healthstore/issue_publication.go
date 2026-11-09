package healthstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type IssueWork struct {
	OrganizationID      tracker.OrganizationID
	FindingID           string
	ProjectID           tracker.ProjectID
	SourceRevision      int64
	PublicationRevision int64
	SourceDigest        string
	AfterFingerprint    string
	Pending             bool
	NextCheckAt         time.Time
	CheckedAt           *time.Time
}

type IssueReport struct {
	OrganizationID    tracker.OrganizationID
	FindingID         string
	ProjectID         tracker.ProjectID
	WorkItemID        tracker.NativeWorkItemID
	IntakeFingerprint string
	EvidenceDigest    string
	ResolvedAt        *time.Time
}

type IssueReader interface {
	NextHealthIssueWork(context.Context, time.Time) (IssueWork, error)
	HealthIssueReports(context.Context, string, tracker.ProjectID, string, int) ([]IssueReport, error)
	HealthIssueReport(context.Context, string, tracker.ProjectID, string) (IssueReport, error)
	LatestHealthIssueReport(context.Context, tracker.ProjectID, string) (IssueReport, error)
	AuthorizeHealthIssue(context.Context, string, int64, tracker.ProjectID, bool) error
	CanResolveHealthIssue(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) (bool, error)
	HealthIssueAutoCancelledBefore(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) (bool, error)
}

type IssueWriter interface {
	IssueReader
	EnqueueHealthIssueWork(context.Context, IssueWork) error
	AdvanceHealthIssueWork(context.Context, IssueWork, time.Time) (IssueWork, error)
	FinishHealthIssueWork(context.Context, IssueWork, bool, time.Time) error
	RecordHealthIssueReport(context.Context, IssueReport) error
}
