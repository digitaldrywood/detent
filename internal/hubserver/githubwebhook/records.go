package githubwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githuboutbound"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) Receive(c echo.Context, verifiedByEntry bool) error {
	if s.deps.Repository == nil || !verifiedByEntry && len(s.deps.Secret) == 0 {
		return c.JSON(http.StatusServiceUnavailable, webhookErrorResponse{Code: "webhook_unavailable", Message: "GitHub webhook is not configured"})
	}
	ctx := s.deps.Context(c.Request().Context())
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodyBytes+1))
	if err != nil {
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{Code: "invalid_payload", Message: "Cannot read webhook payload"})
	}
	if len(raw) > maxBodyBytes {
		return c.NoContent(http.StatusRequestEntityTooLarge)
	}
	if !verifiedByEntry {
		signature, err := hex.DecodeString(strings.TrimPrefix(c.Request().Header.Get("X-Hub-Signature-256"), "sha256="))
		mac := hmac.New(sha256.New, s.deps.Secret)
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
	var payload Payload
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
	organization := s.deps.Organization
	digest := sha256.Sum256(raw)
	ref, err := eventstore.NewContent(s.deps.Details).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization}, raw)
	if err != nil {
		return s.deps.Error(c, err)
	}
	now := s.deps.Now().UTC().Truncate(time.Microsecond)
	row := projectstore.GitHubWebhook{DeliveryID: id, Repository: strings.ToLower(repository), EventType: event, Action: envelope.Action, Digest: hex.EncodeToString(digest[:]), Payload: ref, ReceivedAt: now, ExpiresAt: now.Add(s.deps.Retention), NextAttemptAt: now, Status: "pending"}
	created := false
	err = s.deps.Repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return s.deps.Unavailable
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
			return s.deps.DeliveryConflict
		}
		if err := owner.ObserveGitHubWebhookReceipt(ctx, row); err != nil {
			return err
		}
		row = prior
		return nil
	})
	if errors.Is(err, s.deps.DeliveryConflict) {
		return c.NoContent(http.StatusConflict)
	}
	if err != nil {
		return s.deps.Error(c, err)
	}
	if row.Status == "pending" && !row.NextAttemptAt.After(now) {
		if err := s.Process(ctx, row); err != nil {
			s.deps.Logger.WarnContext(ctx, "process GitHub webhook", "error", err)
		}
	}
	return c.JSON(http.StatusAccepted, webhookResponse{DeliveryID: id, Duplicate: !created})
}

func (s *Service) Maintain(ctx context.Context) error {
	if s.deps.Repository == nil {
		return nil
	}
	organization := s.deps.Organization
	var row projectstore.GitHubWebhook
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return s.deps.Unavailable
		}
		row, err = gitHubIntakeReader.NextGitHubWebhook(ctx, s.deps.Now())
		return err
	})
	if err == nil {
		err = s.Process(ctx, row)
	} else if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	pruneErr := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return gitHubIntakeWriter.PurgeGitHubWebhookPayloads(ctx, s.deps.Now(), 64)
	})
	return errors.Join(err, pruneErr)
}

func (s *Service) Process(ctx context.Context, row projectstore.GitHubWebhook) error {
	organization := row.Payload.OrganizationID
	raw, err := eventstore.NewContent(s.deps.Details).Receipt(ctx, row.Payload)
	if err != nil {
		return err
	}
	var payload Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	previous, attempts := row.AfterProject, row.Attempts
	var projects []tracker.ProjectID
	err = s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return s.deps.Unavailable
		}
		projects, err = gitHubIntakeReader.GitHubWebhookProjects(ctx, row.Repository, row.AfterProject, 2)
		return err
	})
	if err != nil {
		return err
	}
	if len(projects) > 0 {
		scope, scopeErr := s.deps.Integration(ctx, organization, projects[0])
		err = scopeErr
		if err == nil {
			scope.Repository = row.Repository
			err = s.Apply(ctx, scope, row, payload)
		}
		if err == nil {
			row.AfterProject = projects[0]
		}
	}
	if err == nil && len(projects) < 2 {
		row.Status = "completed"
	}
	row.NextAttemptAt = s.deps.Now()
	if err != nil {
		row.Attempts++
		row.NextAttemptAt = row.NextAttemptAt.Add(githuboutbound.RetryDelay(row.Attempts, 2*time.Second, 64*time.Second, s.deps.Jitter()))
	} else {
		row.Attempts = 0
	}
	writeErr := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return gitHubIntakeWriter.UpdateGitHubWebhook(ctx, row, previous, attempts)
	})
	return errors.Join(err, writeErr)
}

func (s *Service) Apply(ctx context.Context, scope Integration, row projectstore.GitHubWebhook, payload Payload) error {
	if row.EventType == "pull_request" {
		return s.applyPullRequest(ctx, scope, row, payload)
	}
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
	err = s.deps.Repository.ReadTenant(ctx, scope.Scope.Organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		linked, err = r.LinkedSourceByURL(ctx, scope.Scope.Project, canonical)
		return err
	})
	missing := errors.Is(err, corestore.ErrNotFound)
	if err != nil && !missing {
		return err
	}
	if missing && (row.EventType != "issues" || row.Action != "opened") {
		return nil
	}
	snapshot := SnapshotFromSource(githubcontract.IssueSource{URL: canonical, NodeID: source.NodeID, Number: source.Number, Title: source.Title, Body: source.Body, AuthorID: source.AuthorID, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt}, row.ReceivedAt)
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
		snapshot.Comments = []tracker.GitHubIssueComment{{Body: c.Body, Provenance: tracker.Provenance{Provider: "github", ExternalID: c.NodeID, AuthorID: author, CreatedAt: c.CreatedAt, UpdatedAt: updated, ObservedAt: row.ReceivedAt}}}
	}
	identity, err := hubapi.CommandHash(struct {
		Project  tracker.ProjectID
		Delivery string
	}{scope.Scope.Project, row.DeliveryID})
	if err != nil {
		return err
	}
	key := "webhook:" + identity
	if missing {
		err = scope.Import(ctx, scope.Repository, key, tracker.GitHubIssuePreview{Number: number, ID: source.NodeID, URL: canonical}, snapshot)
		return err
	}
	if row.EventType == "issues" && (row.Action == "closed" || row.Action == "reopened") {
		provenance := snapshot.Provenance
		provenance.ExternalID = "state:" + source.NodeID + ":" + row.Action + ":" + source.UpdatedAt.Format(time.RFC3339Nano)
		if payload.Sender != nil && payload.Sender.Login != nil {
			provenance.AuthorID = *payload.Sender.Login
		}
		snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: "GitHub issue " + row.Action + ": " + canonical, Provenance: provenance})
	}
	err = scope.Apply(ctx, scope.Repository, linked.WorkItemID, key, snapshot)
	return err
}

func (s *Service) applyPullRequest(ctx context.Context, scope Integration, row projectstore.GitHubWebhook, payload Payload) error {
	source, _, complete := normalizeWebhookPullRequest(payload.PullRequest)
	if !complete {
		return errors.New("GitHub pull request webhook is incomplete")
	}
	canonical := "https://github.com/" + strings.ToLower(row.Repository) + "/pull/" + strconv.Itoa(source.Number)
	if payload.Repository == nil || payload.Repository.FullName == nil || !strings.EqualFold(*payload.Repository.FullName, row.Repository) || !strings.EqualFold(source.URL, canonical) || !s.deps.ValidCommit(source.HeadSHA) || source.MergedAt.IsZero() && source.State != "open" && source.State != "closed" || !source.MergedAt.IsZero() && (source.State != "closed" || !s.deps.ValidCommit(source.MergeSHA)) {
		return errors.New("GitHub pull request webhook identity is invalid")
	}
	observation := projectstore.GitHubPullRequestObservation{Repository: strings.ToLower(row.Repository), Number: source.Number, HeadSHA: source.HeadSHA, SourceUpdatedAt: source.UpdatedAt, ObservedAt: row.ReceivedAt, State: source.State, Draft: source.Draft, MergeSHA: &source.MergeSHA, BaseRef: &source.BaseRef, MergedBy: &source.MergedBy}
	if payload.Repository.DefaultBranch != nil && strings.TrimSpace(*payload.Repository.DefaultBranch) != "" {
		observation.RepositoryDefaultBranch = new(strings.TrimSpace(*payload.Repository.DefaultBranch))
	}
	if !source.MergedAt.IsZero() {
		observation.State = "merged"
		observation.MergedAt = new(source.MergedAt)
	}
	return s.deps.Repository.WriteTenant(ctx, scope.Scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := scope.Scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		project, err := w.ProjectMetadata(ctx, scope.Scope.Project)
		if err != nil {
			return err
		}
		if !strings.EqualFold(project.Settings.Repository, row.Repository) {
			return nil
		}
		writer, ok := w.(projectstore.GitHubIntakeWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return writer.RecordGitHubPullRequestSource(ctx, scope.Scope.Project, row, observation)
	})
}
