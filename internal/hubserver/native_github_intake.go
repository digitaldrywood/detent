package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type linkedSourceURLKey struct{}

func nativeTriageStates(states []tracker.NativeState) []tracker.NativeState {
	triage := tracker.NativeState{Name: "Triage", Transitions: []string{}}
	result := make([]tracker.NativeState, 0, len(states)+1)
	for _, state := range states {
		if slices.Contains([]string{"Backlog", "Todo", "Cancelled"}, state.Name) {
			triage.Transitions = append(triage.Transitions, state.Name)
		}
		if state.Name != "Triage" {
			result = append(result, state)
		}
	}
	return append([]tracker.NativeState{triage}, result...)
}

func ensureNativeTriage(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if err := applyNativeProjectStates(ctx, tx, scope, nativeTriageStates(project.States), now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE projects SET github_intake = 'manual' WHERE id = ?", scope.project)
	return err
}

func readLinkedNativeIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, id string) (tracker.NativeIssue, error) {
	issue, _, err := readNativeIssue(ctx, tx, scope, id)
	return issue, err
}

func linkedSourceNumber(source string) int {
	_, _, number, err := tracker.ParseGitHubIssueURL(source)
	if err != nil {
		return 0
	}
	return number
}

func (s *Service) linkedSourceContext(ctx context.Context) context.Context {
	base := ""
	if s.config.Hosted != nil {
		base = s.config.Hosted.PublicURL + s.hostedBase()
	}
	return context.WithValue(ctx, linkedSourceURLKey{}, base)
}

func enqueueLinkedSourceSummary(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, key string, closeSource bool, now time.Time) error {
	if issue.LinkedSource == nil {
		return nil
	}
	base, ok := ctx.Value(linkedSourceURLKey{}).(string)
	if !ok || base == "" {
		return nil
	}
	target := base + "/work/i/" + url.PathEscape(string(issue.WorkItemID))
	body, reason := "Tracked in Detent: "+target, ""
	if closeSource {
		body, reason = "Cancelled in Detent: "+target, "not_planned"
	}
	return enqueueNativeSourceSummary(ctx, tx, scope, string(issue.WorkItemID), key, body, closeSource, reason, now)
}

func linkedSnapshotFromSource(source IssueSource, now time.Time) tracker.GitHubIssueSnapshot {
	author := source.AuthorID
	if author == "" {
		author = "unavailable"
	}
	return tracker.GitHubIssueSnapshot{URL: source.URL, Title: source.Title, Body: source.Body,
		Provenance: tracker.Provenance{Provider: "github", ExternalID: source.NodeID, AuthorID: author, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt, ObservedAt: now}}
}

func (s *Service) fetchLinkedSnapshot(ctx context.Context, scope nativeScope, sourceURL string) (*tracker.GitHubIssueSnapshot, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(sourceURL)
	if err != nil {
		return nil, nativeInvalid(err.Error())
	}
	integration, err := readProjectIntegration(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	if err := validateLinkedIssueRepository(integration, repository); err != nil {
		return nil, err
	}
	if s.config.ImportBackend == nil {
		return nil, nativeInvalid("GitHub App transport is unavailable for linked issue intake")
	}
	request := GitHubImportRequest{Profile: "native", Repository: repository, IssueNumber: number, Stage: "issue"}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, request)
	if err != nil {
		return nil, err
	}
	if page.Issue == nil {
		return nil, nativeInvalid("GitHub intake returned no issue")
	}
	fetchedURL, _, _, err := tracker.ParseGitHubIssueURL(page.Issue.URL)
	if err != nil || fetchedURL != canonical {
		return nil, nativeInvalid("GitHub intake returned a different issue")
	}
	snapshot := linkedSnapshotFromSource(*page.Issue, s.config.now())
	snapshot.URL = canonical
	request.Stage = "comments"
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages == 500 {
			return nil, nativeInvalid("Linked issue discussion exceeds the intake page bound")
		}
		page, err = s.config.ImportBackend.FetchImportPage(ctx, request)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			if record.Kind == "comment" && !strings.Contains(record.Body, "<!-- detent-summary:") && !seen[record.Provenance.ExternalID] {
				snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: record.Body, Provenance: record.Provenance})
				seen[record.Provenance.ExternalID] = true
			}
		}
		if page.NextCursor == "" {
			break
		}
		if len(snapshot.Comments) > 1000 || seen["cursor:"+page.NextCursor] {
			return nil, nativeInvalid("Linked issue discussion exceeds the intake bound")
		}
		seen["cursor:"+page.NextCursor] = true
		request.Cursor = page.NextCursor
	}
	return &snapshot, nil
}

func applyNativeIssueWebhook(ctx context.Context, tx *sql.Tx, delivery storedWebhook, payload githubWebhookPayload, now time.Time) (result webhookProcessResult, handled bool, resultErr error) {
	if delivery.EventType != "issues" && delivery.EventType != "issue_comment" {
		return webhookProcessResult{}, false, nil
	}
	fullName := webhookRepositoryFullName(payload.Repository)
	rows, err := tx.QueryContext(ctx, `SELECT p.organization_id, p.id FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id
 WHERE p.profile = 'native' AND (lower(r.github_owner || '/' || r.github_name) = lower(?) OR lower(p.checkout_repository) = lower(?))`, fullName, fullName)
	if err != nil {
		return webhookProcessResult{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	var scopes []nativeScope
	defer rows.Close()
	for rows.Next() {
		var scope nativeScope
		scope.credential.Scope = apiScopeWorker
		scope.sourceActor = nativeIntegrationActor("github")
		if err := rows.Scan(&scope.organization, &scope.project); err != nil {
			return webhookProcessResult{}, false, err
		}
		scopes = append(scopes, scope)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return webhookProcessResult{}, false, err
	}
	if len(scopes) == 0 {
		return webhookProcessResult{}, false, nil
	}
	result = webhookProcessResult{Outcome: webhookOutcomeIgnored}
	if payload.Issue == nil || len(payload.Issue.PullRequest) != 0 && string(payload.Issue.PullRequest) != "null" {
		return result, true, nil
	}
	source, _, complete := normalizeWebhookIssue(payload.Issue)
	if !complete {
		return result, true, errors.New("native GitHub issue webhook is incomplete")
	}
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(source.URL)
	if err != nil || !strings.EqualFold(repository, fullName) || number != source.Number {
		return result, true, errors.New("native GitHub issue webhook source does not match its repository")
	}
	repositoryID, err := ensureWebhookSourceRepository(ctx, tx, payload.Repository, now)
	if err != nil {
		return result, true, err
	}
	result.RepositoryID = &repositoryID
	for _, scope := range scopes {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE project_id = ? AND (native_source_key = ? OR (repository_id = ? AND github_number = ?))`, scope.project, "github:"+canonical, repositoryID, source.Number).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if delivery.EventType != "issues" || delivery.Action != "opened" {
				continue
			}
			issue, err := createLinkedIssueTx(ctx, tx, scope, tracker.CreateIssue{GitHubIssueURL: canonical}, now)
			if err != nil {
				return result, true, err
			}
			id = string(issue.WorkItemID)
		} else if err != nil {
			return result, true, err
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, id)
		if err != nil {
			return result, true, err
		}
		if issue.LinkedSource == nil {
			continue
		}
		snapshot := linkedSnapshotFromSource(IssueSource{NodeID: source.NodeID, Number: source.Number, URL: canonical, Title: source.Title, Body: source.Body, AuthorID: source.AuthorID, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt}, now)
		if delivery.EventType == "issue_comment" {
			if delivery.Action != "created" || payload.Comment == nil || strings.Contains(payload.Comment.Body, "<!-- detent-summary:") {
				continue
			}
			comment := payload.Comment
			provenance := tracker.Provenance{Provider: "github", ExternalID: comment.NodeID, AuthorID: "unavailable", CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt, ObservedAt: now}
			if comment.User != nil && comment.User.Login != nil {
				provenance.AuthorID = *comment.User.Login
			}
			if provenance.UpdatedAt.IsZero() {
				provenance.UpdatedAt = provenance.CreatedAt
			}
			if err := validateImportProvenance(&provenance); err != nil {
				return result, true, err
			}
			if len(comment.Body) > 64<<10 {
				return result, true, nativeInvalid("GitHub comment is oversized")
			}
			snapshot.Comments = []tracker.GitHubIssueComment{{Body: comment.Body, Provenance: provenance}}
		}
		if _, err := intakeLinkedIssueTx(ctx, tx, scope, id, snapshot, now); err != nil {
			return result, true, err
		}
		if delivery.EventType == "issues" && (delivery.Action == "closed" || delivery.Action == "reopened") {
			provenance := snapshot.Provenance
			provenance.ExternalID = "state:" + source.NodeID + ":" + delivery.Action + ":" + source.UpdatedAt.Format(time.RFC3339Nano)
			if payload.Sender != nil && payload.Sender.Login != nil {
				provenance.AuthorID = *payload.Sender.Login
			}
			if err := importGitHubComment(ctx, tx, scope, id, GitHubImportRecord{Body: fmt.Sprintf("GitHub issue %s: %s", delivery.Action, canonical), Provenance: provenance}, now); err != nil {
				return result, true, err
			}
		}
		result.Outcome = webhookOutcomeApplied
	}
	return result, true, nil
}

func ensureWebhookSourceRepository(ctx context.Context, tx *sql.Tx, repository *githubWebhookRepository, now time.Time) (int64, error) {
	fullName := webhookRepositoryFullName(repository)
	if id, found, err := resolveWebhookRepositoryID(ctx, tx, fullName); found || err != nil {
		return id, err
	}
	normalized, stamp, complete := normalizeWebhookRepository(repository, now)
	if !complete {
		return 0, errors.New("GitHub webhook repository is incomplete")
	}
	id, _, err := applyRepositoryProjection(ctx, tx, normalized, stamp, now, false, true)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM projects WHERE repository_id = ? AND profile = 'github_compatible'", id)
	return id, err
}
