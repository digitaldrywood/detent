package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GitHubWebhook struct {
	DeliveryID, Repository, EventType, Action, Digest, Status string
	AfterProject, BindingProject                              tracker.ProjectID
	Payload                                                   eventstore.Receipt
	ReceivedAt, ExpiresAt, NextAttemptAt                      time.Time
	Attempts                                                  int64
}

type GitHubIntakeReader interface {
	GitHubWebhookAuthorized(context.Context, GitHubWebhook) (bool, error)
	LatestGitHubWebhookReceipt(context.Context) (time.Time, error)
	NextGitHubIntake(context.Context, time.Time) (tracker.ProjectID, error)
	GitHubWebhook(context.Context, string, tracker.ProjectID) (GitHubWebhook, error)
	NextGitHubWebhook(context.Context, time.Time) (GitHubWebhook, error)
	GitHubWebhookProjects(context.Context, GitHubWebhook, int) ([]tracker.ProjectID, error)
}

type GitHubIntakeWriter interface {
	GitHubIntakeReader
	CreateGitHubWebhook(context.Context, GitHubWebhook) (bool, error)
	ObserveGitHubWebhookReceipt(context.Context, GitHubWebhook) error
	UpdateGitHubWebhook(context.Context, GitHubWebhook, tracker.ProjectID, int64) error
	PurgeGitHubWebhookPayloads(context.Context, time.Time, int) error
	DeferGitHubIntake(context.Context, tracker.ProjectID, time.Time, float64) error
	RecordGitHubPullRequestSource(context.Context, tracker.ProjectID, GitHubWebhook, GitHubPullRequestObservation) error
}

type GitHubPullRequestRefreshKey struct {
	ProjectID  tracker.ProjectID
	ChangeID   string
	Repository string
	Number     int
}

type GitHubPullRequestRefresh struct {
	GitHubPullRequestRefreshKey
	WorkItemID           tracker.NativeWorkItemID
	RequestedVersion     string
	RequestedHeadSHA     string
	RequestCount         int64
	ObservedRequestCount int64
	RequestedAt          time.Time
	NextAttemptAt        time.Time
	Pending              bool
	Attempts             int64
	Observation          *GitHubPullRequestObservation
}

type GitHubPullRequestObservation struct {
	RepositoryDefaultBranch     *string
	MergeSHA, BaseRef, MergedBy *string
	MergedAt                    *time.Time
	Repository                  string
	Number                      int
	HeadSHA                     string
	SourceUpdatedAt             time.Time
	ObservedAt                  time.Time
	State                       string
	Draft                       bool
	Mergeable                   *bool
	Checks                      tracker.CheckSummary
	Reviews                     tracker.ReviewSummary
}

type GitHubPullRequestRefreshReader interface {
	GitHubPullRequestRefresh(context.Context, GitHubPullRequestRefreshKey) (GitHubPullRequestRefresh, error)
	NextGitHubPullRequestRefresh(context.Context, time.Time) (GitHubPullRequestRefresh, error)
}

type GitHubPullRequestRefreshWriter interface {
	GitHubPullRequestRefreshReader
	RequestGitHubPullRequests(context.Context, []GitHubPullRequestRefresh) error
	CompleteGitHubPullRequestRefresh(context.Context, GitHubPullRequestRefresh, GitHubPullRequestObservation) error
	RetryGitHubPullRequestRefresh(context.Context, GitHubPullRequestRefresh, time.Time) error
	CancelGitHubPullRequestRefresh(context.Context, GitHubPullRequestRefresh) error
	SupersedeGitHubPullRequestRefresh(context.Context, GitHubPullRequestRefresh) error
}

type GitHubOutbound struct {
	ID                                                   int64
	ProjectID                                            tracker.ProjectID
	WorkItemID                                           tracker.NativeWorkItemID
	Key, Repository, Kind, TargetNodeID, Status, LeaseID string
	CoalesceKey                                          string
	IssueNumber, Attempts                                int64
	CreatedAt, NextAttemptAt                             time.Time
	Payload                                              eventstore.Receipt
	Outcome                                              string
	Progress                                             GitHubOutboundProgress
	Blocked                                              bool
	Observations                                         []GitHubOutboundObservation
}

type GitHubOutboundProgress struct {
	Stage, Cursor                            string
	TargetID, FallbackID, Verification, Page int64
	SourceUpdatedAt                          time.Time
}

type GitHubOutboundObservation struct {
	Kind, Key string
	Sequence  int64
	Ready     bool
}

type GitHubOutboxWriter interface {
	SupersedeGitHubSummaries(context.Context, tracker.ProjectID, string, time.Time) error
	SupersedeGitHubCompatibilityWrites(context.Context, tracker.ProjectID, bool, time.Time) error
	GitHubOutbound(context.Context, string) (GitHubOutbound, error)
	EnqueueGitHubOutbound(context.Context, GitHubOutbound) error
	ClaimGitHubOutbound(context.Context, time.Time, time.Time, string) (GitHubOutbound, error)
	RenewGitHubOutbound(context.Context, int64, string, time.Time, time.Time, bool) error
	FinishGitHubOutbound(context.Context, GitHubOutbound, time.Time) error
}

type GitHubReconciliation struct {
	ProjectID             tracker.ProjectID
	Repository, NodeID    string
	RepositoryEnabled     bool
	AfterWorkItem         tracker.NativeWorkItemID
	Revision, Attempts    int64
	NextAttemptAt         time.Time
	LastSuccessfulSweepAt *time.Time
	LastFailureAt         *time.Time
	RepositoryPage        int64
	SweepStartedAt        *time.Time
}

type GitHubReconciliationReader interface {
	GitHubReconciliation(context.Context, tracker.ProjectID) (GitHubReconciliation, error)
	DueGitHubReconciliations(context.Context, time.Time, int, bool) ([]GitHubReconciliation, error)
	NextGitHubLinkedSource(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (eventstore.LinkedSource, error)
}

type GitHubPullRequestDrift struct {
	Number          int
	SourceUpdatedAt time.Time
}

type GitHubReconciliationWriter interface {
	GitHubReconciliationReader
	SaveGitHubReconciliation(context.Context, GitHubReconciliation) error
	RequestDriftedGitHubPullRequests(context.Context, tracker.ProjectID, string, []GitHubPullRequestDrift, time.Time) (int64, error)
}

type GitHubWorkpadReader interface {
	LatestGitHubWorkpad(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (GitHubOutbound, error)
}
