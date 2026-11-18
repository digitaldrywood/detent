package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) gitHubWebhookIntegration(scope hubscope.NativeScope) githubwebhook.Integration {
	return githubwebhook.Integration{Scope: s.authorization().Scope(scope), Repository: scope.IntegrationRepository,
		Import: func(ctx context.Context, repository, key string, preview tracker.GitHubIssuePreview, snapshot tracker.GitHubIssueSnapshot) error {
			scope := scope
			scope.IntegrationRepository = repository
			_, err := s.importGitHubSnapshotRecord(ctx, scope, key, preview, snapshot, "Triage", nil)
			return err
		},
		Apply: func(ctx context.Context, repository string, item tracker.NativeWorkItemID, key string, snapshot tracker.GitHubIssueSnapshot) error {
			scope := scope
			scope.IntegrationRepository = repository
			scope.ScheduledRepair = s.observeImportedScheduledRepair(ctx, scope, snapshot)
			_, err := s.applyLinkedIssueRecord(ctx, scope, item, "github.webhook", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key}, Snapshot: snapshot})
			return err
		},
	}
}
func (s *Service) githubWebhookService() *githubwebhook.Service {
	organization := tracker.OrganizationID("")
	if s.database != nil {
		organization = s.database.hostedOrganization
	}
	return s.githubWebhookServiceFor(organization)
}
func (s *Service) githubWebhookServiceFor(organization tracker.OrganizationID) *githubwebhook.Service {
	return githubwebhook.New(githubwebhook.Deps{
		Repository:       s.config.TenantRepository,
		Organization:     organization,
		Details:          s.runDetailStore(),
		Now:              s.config.Clock,
		Jitter:           s.config.deps.Jitter,
		Logger:           s.config.Logger,
		Secret:           s.config.GitHubWebhookSecret,
		Retention:        s.config.WebhookPayloadRetention,
		Unavailable:      hubapi.ErrPostgresFeatureUnavailable,
		DeliveryConflict: ErrWebhookDeliveryConflict,
		Context:          s.runDetailContext,
		Error:            s.nativeAPIError,
		ValidCommit:      hubapi.ValidCommitID,
		Integration: func(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (githubwebhook.Integration, error) {
			scope, err := s.gitHubIntegrationScope(ctx, organization, project)
			return s.gitHubWebhookIntegration(scope), err
		},
	})
}
func (s *Service) receiveGitHubWebhookRecord(c echo.Context, verified bool) error {
	return s.githubWebhookService().Receive(c, verified)
}
func (s *Service) maintainGitHubWebhookRecords(ctx context.Context) error {
	return s.githubWebhookService().Maintain(ctx)
}
func (s *Service) applyGitHubWebhookRecord(ctx context.Context, scope hubscope.NativeScope, row projectstore.GitHubWebhook, payload githubWebhookPayload) error {
	return s.githubWebhookService().Apply(ctx, s.gitHubWebhookIntegration(scope), row, payload)
}
func splitRepositoryFullName(value string) (string, string, bool) {
	return githubwebhook.SplitRepositoryFullName(value)
}

type githubWebhookPayload = githubwebhook.Payload
type githubWebhookRepository = githubwebhook.Repository
type githubWebhookActor = githubwebhook.Actor
type githubWebhookComment = githubwebhook.Comment
type githubWebhookIssue = githubwebhook.Issue

func linkedSnapshotFromSource(source IssueSource, now time.Time) tracker.GitHubIssueSnapshot {
	return githubwebhook.SnapshotFromSource(source, now)
}
