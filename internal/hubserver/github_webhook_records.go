package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) receiveGitHubWebhookRecord(c echo.Context, verifiedByEntry bool) error {
	if s.config.TenantRepository == nil || !verifiedByEntry && len(s.config.GitHubWebhookSecret) == 0 {
		return c.JSON(http.StatusServiceUnavailable, webhookErrorResponse{Code: "webhook_unavailable", Message: "GitHub webhook is not configured"})
	}
	ctx := s.runDetailContext(c.Request().Context())
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, githubWebhookMaxBodyBytes+1))
	if err != nil {
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{Code: "invalid_payload", Message: "Cannot read webhook payload"})
	}
	if len(raw) > githubWebhookMaxBodyBytes {
		return c.NoContent(http.StatusRequestEntityTooLarge)
	}
	if !verifiedByEntry {
		signature, err := hex.DecodeString(strings.TrimPrefix(c.Request().Header.Get("X-Hub-Signature-256"), "sha256="))
		mac := hmac.New(sha256.New, s.config.GitHubWebhookSecret)
		if _, err := mac.Write(raw); err != nil {
			return c.NoContent(http.StatusInternalServerError)
		}
		if err != nil || !strings.HasPrefix(c.Request().Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
			return c.NoContent(http.StatusUnauthorized)
		}
	}
	id, event := c.Request().Header.Get("X-GitHub-Delivery"), c.Request().Header.Get("X-GitHub-Event")
	if strings.TrimSpace(id) == "" || len(id) > 256 || strings.TrimSpace(event) == "" || len(event) > 100 {
		return c.NoContent(http.StatusBadRequest)
	}
	var payload githubWebhookPayload
	var envelope struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &payload) != nil || json.Unmarshal(raw, &envelope) != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	repository := webhookRepositoryFullName(payload.Repository)
	if repository == "" && event != "ping" {
		return c.NoContent(http.StatusBadRequest)
	}
	organization := s.database.hostedOrganization
	digest := sha256.Sum256(raw)
	ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	now := s.config.now().UTC()
	row := projectstore.GitHubWebhook{DeliveryID: id, Repository: strings.ToLower(repository), EventType: event, Action: envelope.Action, Digest: hex.EncodeToString(digest[:]), Payload: ref, ReceivedAt: now, ExpiresAt: now.Add(s.config.WebhookPayloadRetention), NextAttemptAt: now, Status: "pending"}
	created := false
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		var err error
		created, err = owner.CreateGitHubWebhook(ctx, row)
		if err != nil {
			return err
		}
		if created {
			return nil
		}
		prior, err := owner.GitHubWebhook(ctx, id)
		if err != nil {
			return err
		}
		if prior.Digest != row.Digest || prior.EventType != event || prior.Repository != row.Repository {
			return ErrWebhookDeliveryConflict
		}
		row = prior
		return nil
	})
	if errors.Is(err, ErrWebhookDeliveryConflict) {
		return c.NoContent(http.StatusConflict)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if row.Status == "pending" && !row.NextAttemptAt.After(now) {
		if err := s.processGitHubWebhookRecord(ctx, row); err != nil {
			s.config.Logger.WarnContext(ctx, "process GitHub webhook", "error", err)
		}
	}
	return c.JSON(http.StatusAccepted, webhookResponse{DeliveryID: id, Duplicate: !created})
}

func (s *Service) maintainGitHubWebhookRecords(ctx context.Context) error {
	if s.config.TenantRepository == nil {
		return nil
	}
	organization := s.database.hostedOrganization
	var row projectstore.GitHubWebhook
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		row, err = gitHubIntakeReader.NextGitHubWebhook(ctx, s.config.now())
		return err
	})
	if err == nil {
		err = s.processGitHubWebhookRecord(ctx, row)
	} else if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	pruneErr := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubIntakeWriter.PurgeGitHubWebhookPayloads(ctx, s.config.now(), 64)
	})
	return errors.Join(err, pruneErr)
}

func (s *Service) processGitHubWebhookRecord(ctx context.Context, row projectstore.GitHubWebhook) error {
	organization := row.Payload.OrganizationID
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row.Payload)
	if err != nil {
		return err
	}
	var payload githubWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	previous, attempts := row.AfterProject, row.Attempts
	var projects []tracker.ProjectID
	err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		projects, err = gitHubIntakeReader.GitHubWebhookProjects(ctx, row.Repository, row.AfterProject, 2)
		return err
	})
	if err != nil {
		return err
	}
	if len(projects) > 0 {
		scope, scopeErr := s.gitHubIntegrationScope(ctx, organization, projects[0])
		err = scopeErr
		if err == nil {
			scope.integrationRepository = row.Repository
			err = s.applyGitHubWebhookRecord(ctx, scope, row, payload)
		}
		if err == nil {
			row.AfterProject = projects[0]
		}
	}
	if err == nil && len(projects) < 2 {
		row.Status = "completed"
	}
	row.Attempts++
	row.NextAttemptAt = s.config.now()
	if err != nil {
		row.NextAttemptAt = row.NextAttemptAt.Add(time.Second * time.Duration(1<<min(row.Attempts, 6)))
	}
	writeErr := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubIntakeWriter.UpdateGitHubWebhook(ctx, row, previous, attempts)
	})
	return errors.Join(err, writeErr)
}

func (s *Service) applyGitHubWebhookRecord(ctx context.Context, scope nativeScope, row projectstore.GitHubWebhook, payload githubWebhookPayload) error {
	if row.EventType != "issues" && row.EventType != "issue_comment" {
		return nil
	}
	if payload.Issue == nil || len(payload.Issue.PullRequest) != 0 && string(payload.Issue.PullRequest) != "null" {
		return nil
	}
	source, _, complete := normalizeWebhookIssue(payload.Issue)
	if !complete {
		return errors.New("GitHub issue webhook is incomplete")
	}
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(source.URL)
	if err != nil || !strings.EqualFold(repository, row.Repository) || number != source.Number {
		return errors.New("GitHub webhook issue does not match repository")
	}
	var linked eventstore.LinkedSource
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		linked, err = r.LinkedSourceByURL(ctx, scope.project, canonical)
		return err
	})
	missing := errors.Is(err, corestore.ErrNotFound)
	if err != nil && !missing {
		return err
	}
	if missing && (row.EventType != "issues" || row.Action != "opened") {
		return nil
	}
	snapshot := linkedSnapshotFromSource(IssueSource{URL: canonical, NodeID: source.NodeID, Number: source.Number, Title: source.Title, Body: source.Body, AuthorID: source.AuthorID, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt}, s.config.now())
	if row.EventType == "issue_comment" {
		if row.Action != "created" || payload.Comment == nil || strings.Contains(payload.Comment.Body, "<!-- detent-summary:") {
			return nil
		}
		c := payload.Comment
		author := "unavailable"
		if c.User != nil && c.User.Login != nil {
			author = *c.User.Login
		}
		updated := c.UpdatedAt
		if updated.IsZero() {
			updated = c.CreatedAt
		}
		snapshot.Comments = []tracker.GitHubIssueComment{{Body: c.Body, Provenance: tracker.Provenance{Provider: "github", ExternalID: c.NodeID, AuthorID: author, CreatedAt: c.CreatedAt, UpdatedAt: updated, ObservedAt: s.config.now()}}}
	}
	key := "webhook:" + row.DeliveryID
	if missing {
		_, err = s.importGitHubSnapshotRecord(ctx, scope, key, tracker.GitHubIssuePreview{Number: number, ID: source.NodeID, URL: canonical}, snapshot, "Triage", nil)
		return err
	}
	scope.scheduledRepair = s.observeImportedScheduledRepair(ctx, scope, snapshot)
	_, err = s.applyLinkedIssueRecord(ctx, scope, linked.WorkItemID, "github.webhook", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key}, Snapshot: snapshot})
	return err
}
