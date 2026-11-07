package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GitHubWebhook struct {
	DeliveryID, Repository, EventType, Action, Digest, Status string
	AfterProject                                              tracker.ProjectID
	Payload                                                   eventstore.Receipt
	ReceivedAt, ExpiresAt, NextAttemptAt                      time.Time
	Attempts                                                  int64
}

type GitHubIntakeReader interface {
	NextGitHubIntake(context.Context, time.Time) (tracker.ProjectID, error)
	GitHubWebhook(context.Context, string) (GitHubWebhook, error)
	NextGitHubWebhook(context.Context, time.Time) (GitHubWebhook, error)
	GitHubWebhookProjects(context.Context, string, tracker.ProjectID, int) ([]tracker.ProjectID, error)
}

type GitHubIntakeWriter interface {
	GitHubIntakeReader
	CreateGitHubWebhook(context.Context, GitHubWebhook) (bool, error)
	UpdateGitHubWebhook(context.Context, GitHubWebhook, tracker.ProjectID, int64) error
	PurgeGitHubWebhookPayloads(context.Context, time.Time, int) error
	DeferGitHubIntake(context.Context, tracker.ProjectID, time.Time) error
}

type GitHubOutbound struct {
	ID                                                   int64
	ProjectID                                            tracker.ProjectID
	WorkItemID                                           tracker.NativeWorkItemID
	Key, Repository, Kind, TargetNodeID, Status, LeaseID string
	IssueNumber, Attempts                                int64
	CreatedAt, NextAttemptAt                             time.Time
	Payload                                              eventstore.Receipt
}

type GitHubOutboxWriter interface {
	EnqueueGitHubOutbound(context.Context, GitHubOutbound) error
	ClaimGitHubOutbound(context.Context, time.Time, time.Time, string) (GitHubOutbound, error)
	FinishGitHubOutbound(context.Context, GitHubOutbound) error
}

type GitHubReconciliation struct {
	ProjectID          tracker.ProjectID
	Repository, NodeID string
	AfterWorkItem      tracker.NativeWorkItemID
	Revision, Attempts int64
	NextAttemptAt      time.Time
}

type GitHubReconciliationReader interface {
	NextGitHubReconciliation(context.Context, time.Time) (GitHubReconciliation, error)
	NextGitHubLinkedSource(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (eventstore.LinkedSource, error)
}

type GitHubReconciliationWriter interface {
	GitHubReconciliationReader
	SaveGitHubReconciliation(context.Context, GitHubReconciliation) error
}
