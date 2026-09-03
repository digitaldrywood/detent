package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type webhookOutcome string

const (
	webhookOutcomeApplied            webhookOutcome = "applied"
	webhookOutcomeHydrationRequested webhookOutcome = "hydration_requested"
	webhookOutcomeIgnored            webhookOutcome = "ignored"
	webhookOutcomeSuperseded         webhookOutcome = "superseded"
	webhookOutcomeUnchanged          webhookOutcome = "unchanged"
)

type githubWebhookPayload struct {
	Repository  *githubWebhookRepository  `json:"repository"`
	Issue       *githubWebhookIssue       `json:"issue"`
	PullRequest *githubWebhookPullRequest `json:"pull_request"`
	Review      *githubWebhookReview      `json:"review"`
	CheckRun    *githubWebhookCheck       `json:"check_run"`
	CheckSuite  *githubWebhookCheck       `json:"check_suite"`
	SHA         *string                   `json:"sha"`
}

type githubWebhookRepository struct {
	DatabaseID *int64              `json:"id"`
	NodeID     *string             `json:"node_id"`
	Name       *string             `json:"name"`
	FullName   *string             `json:"full_name"`
	Owner      *githubWebhookActor `json:"owner"`
	UpdatedAt  json.RawMessage     `json:"updated_at"`
}

type githubWebhookActor struct {
	Login *string `json:"login"`
}

type githubWebhookIssue struct {
	DatabaseID *int64          `json:"id"`
	NodeID     *string         `json:"node_id"`
	Number     *int            `json:"number"`
	Title      *string         `json:"title"`
	Body       json.RawMessage `json:"body"`
	HTMLURL    *string         `json:"html_url"`
	State      *string         `json:"state"`
	Labels     json.RawMessage `json:"labels"`
	Assignees  json.RawMessage `json:"assignees"`
	CreatedAt  json.RawMessage `json:"created_at"`
	UpdatedAt  json.RawMessage `json:"updated_at"`
}

type githubWebhookPullRequest struct {
	DatabaseID *int64            `json:"id"`
	NodeID     *string           `json:"node_id"`
	Number     *int              `json:"number"`
	Title      *string           `json:"title"`
	HTMLURL    *string           `json:"html_url"`
	State      *string           `json:"state"`
	Draft      *bool             `json:"draft"`
	Head       *githubWebhookRef `json:"head"`
	Base       *githubWebhookRef `json:"base"`
	CreatedAt  json.RawMessage   `json:"created_at"`
	UpdatedAt  json.RawMessage   `json:"updated_at"`
}

type githubWebhookRef struct {
	Ref *string `json:"ref"`
	SHA *string `json:"sha"`
}

type githubWebhookReview struct {
	SubmittedAt json.RawMessage `json:"submitted_at"`
}

type githubWebhookCheck struct {
	HeadSHA      *string                    `json:"head_sha"`
	PullRequests []githubWebhookPullRequest `json:"pull_requests"`
	UpdatedAt    json.RawMessage            `json:"updated_at"`
	CompletedAt  json.RawMessage            `json:"completed_at"`
}

type normalizedRepository struct {
	NodeID     string `json:"node_id"`
	DatabaseID *int64 `json:"database_id,omitempty"`
	Owner      string `json:"owner"`
	Name       string `json:"name"`
}

type normalizedIssue struct {
	NodeID     string    `json:"node_id"`
	DatabaseID *int64    `json:"database_id,omitempty"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	Labels     []string  `json:"labels"`
	Assignees  []string  `json:"assignees"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type normalizedPullRequest struct {
	NodeID     string    `json:"node_id"`
	DatabaseID *int64    `json:"database_id,omitempty"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	Draft      bool      `json:"draft"`
	HeadRef    string    `json:"head_ref"`
	HeadSHA    string    `json:"head_sha"`
	BaseRef    string    `json:"base_ref"`
	BaseSHA    string    `json:"base_sha"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type sourceStamp struct {
	UpdatedAt time.Time
	Version   string
}

type hydrationTarget struct {
	RepositoryID       *int64
	RepositoryFullName string
	ObjectKind         string
	ObjectKey          string
	GitHubNodeID       string
	GitHubNumber       int
	HeadSHA            string
	Reason             string
	Source             sourceStamp
}

type projectionApplyResult struct {
	Changed  bool
	Stale    bool
	Conflict bool
}

func applyWebhook(ctx context.Context, tx *sql.Tx, delivery storedWebhook, now time.Time) (webhookProcessResult, error) {
	var payload githubWebhookPayload
	if err := json.Unmarshal(delivery.Payload, &payload); err != nil {
		return webhookProcessResult{}, fmt.Errorf("decode GitHub webhook payload: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(delivery.EventType)) {
	case "issues":
		return applyIssueWebhook(ctx, tx, delivery, payload, now)
	case "pull_request":
		return applyPullRequestWebhook(ctx, tx, delivery, payload, now, "")
	case "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		return applyPullRequestWebhook(ctx, tx, delivery, payload, now, "pull_request_reviews")
	case "check_run", "check_suite", "status":
		return applyChecksWebhook(ctx, tx, delivery, payload, now)
	default:
		return webhookProcessResult{Outcome: webhookOutcomeIgnored}, nil
	}
}

func applyIssueWebhook(ctx context.Context, tx *sql.Tx, delivery storedWebhook, payload githubWebhookPayload, now time.Time) (webhookProcessResult, error) {
	repositoryFullName := webhookRepositoryFullName(payload.Repository)
	partialStamp := sourceStampFromDelivery(delivery, webhookObjectTime(payload.Issue))
	target, hasTarget := issueHydrationTarget(repositoryFullName, payload.Issue, partialStamp, "partial_payload")
	issue, issueStamp, issueComplete := normalizeWebhookIssue(payload.Issue)
	deleted := strings.EqualFold(strings.TrimSpace(delivery.Action), "deleted")
	if deleted && issueComplete {
		issue.State = "closed"
		issue.Labels = []string{}
		issue.Assignees = []string{}
		var err error
		issueStamp, err = newSourceStampWithVersion(issue.UpdatedAt, "2", issue)
		if err != nil {
			return webhookProcessResult{}, err
		}
	}
	repository, repositoryStamp, repositoryComplete := normalizeWebhookRepository(payload.Repository, issueStamp.UpdatedAt)
	repositoryID, repositoryConflict, err := applyOrResolveWebhookRepository(ctx, tx, repositoryFullName, repository, repositoryStamp, repositoryComplete, now)
	if err != nil {
		return webhookProcessResult{}, err
	}
	if repositoryID != nil {
		target.RepositoryID = repositoryID
	}
	if !issueComplete || repositoryID == nil {
		if !hasTarget {
			return webhookProcessResult{Outcome: webhookOutcomeIgnored, RepositoryID: repositoryID}, nil
		}
		if err := enqueueHydration(ctx, tx, delivery.DeliveryID, target, now); err != nil {
			return webhookProcessResult{}, err
		}
		return webhookProcessResult{Outcome: webhookOutcomeHydrationRequested, RepositoryID: repositoryID}, nil
	}

	result, err := applyWebhookIssue(ctx, tx, *repositoryID, issue, issueStamp, now)
	if err != nil {
		return webhookProcessResult{}, err
	}
	if !deleted && (repositoryConflict || result.Conflict) {
		target, _ = issueHydrationTarget(repositoryFullName, payload.Issue, issueStamp, "source_version_conflict")
		target.RepositoryID = repositoryID
		if err := enqueueHydration(ctx, tx, delivery.DeliveryID, target, now); err != nil {
			return webhookProcessResult{}, err
		}
		return webhookProcessResult{Outcome: webhookOutcomeHydrationRequested, RepositoryID: repositoryID}, nil
	}
	return webhookProcessResult{Outcome: projectionOutcome(result), RepositoryID: repositoryID}, nil
}

func applyPullRequestWebhook(ctx context.Context, tx *sql.Tx, delivery storedWebhook, payload githubWebhookPayload, now time.Time, relatedHydrationKind string) (webhookProcessResult, error) {
	repositoryFullName := webhookRepositoryFullName(payload.Repository)
	partialStamp := sourceStampFromDelivery(delivery, webhookObjectTime(payload.PullRequest))
	target, hasTarget := pullRequestHydrationTarget(repositoryFullName, payload.PullRequest, partialStamp, "partial_payload", "pull_request")
	pullRequest, pullRequestStamp, pullRequestComplete := normalizeWebhookPullRequest(payload.PullRequest)
	repository, repositoryStamp, repositoryComplete := normalizeWebhookRepository(payload.Repository, pullRequestStamp.UpdatedAt)
	repositoryID, repositoryConflict, err := applyOrResolveWebhookRepository(ctx, tx, repositoryFullName, repository, repositoryStamp, repositoryComplete, now)
	if err != nil {
		return webhookProcessResult{}, err
	}
	if repositoryID != nil {
		target.RepositoryID = repositoryID
	}

	result := projectionApplyResult{}
	if pullRequestComplete && repositoryID != nil {
		result, err = applyWebhookPullRequest(ctx, tx, *repositoryID, pullRequest, pullRequestStamp, now)
		if err != nil {
			return webhookProcessResult{}, err
		}
	} else if hasTarget {
		if err := enqueueHydration(ctx, tx, delivery.DeliveryID, target, now); err != nil {
			return webhookProcessResult{}, err
		}
	} else {
		return webhookProcessResult{Outcome: webhookOutcomeIgnored, RepositoryID: repositoryID}, nil
	}

	hydrationRequested := !pullRequestComplete || repositoryID == nil
	if repositoryConflict || result.Conflict {
		conflictTarget, ok := pullRequestHydrationTarget(repositoryFullName, payload.PullRequest, pullRequestStamp, "source_version_conflict", "pull_request")
		if ok {
			conflictTarget.RepositoryID = repositoryID
			if err := enqueueHydration(ctx, tx, delivery.DeliveryID, conflictTarget, now); err != nil {
				return webhookProcessResult{}, err
			}
			hydrationRequested = true
		}
	}
	if relatedHydrationKind != "" {
		relatedStamp := sourceStampFromDelivery(delivery, webhookReviewTime(payload.Review))
		relatedTarget, ok := pullRequestHydrationTarget(repositoryFullName, payload.PullRequest, relatedStamp, "reviews_changed", relatedHydrationKind)
		if ok {
			relatedTarget.RepositoryID = repositoryID
			if err := enqueueHydration(ctx, tx, delivery.DeliveryID, relatedTarget, now); err != nil {
				return webhookProcessResult{}, err
			}
			hydrationRequested = true
		}
	}
	if hydrationRequested {
		return webhookProcessResult{Outcome: webhookOutcomeHydrationRequested, RepositoryID: repositoryID}, nil
	}
	return webhookProcessResult{Outcome: projectionOutcome(result), RepositoryID: repositoryID}, nil
}

func applyChecksWebhook(ctx context.Context, tx *sql.Tx, delivery storedWebhook, payload githubWebhookPayload, now time.Time) (webhookProcessResult, error) {
	repositoryFullName := webhookRepositoryFullName(payload.Repository)
	if repositoryFullName == "" {
		return webhookProcessResult{Outcome: webhookOutcomeIgnored}, nil
	}
	resolvedRepositoryID, found, err := resolveWebhookRepositoryID(ctx, tx, repositoryFullName)
	if err != nil {
		return webhookProcessResult{}, err
	}
	var repositoryID *int64
	if found {
		repositoryID = &resolvedRepositoryID
	}
	check := payload.CheckRun
	if check == nil {
		check = payload.CheckSuite
	}
	stamp := sourceStampFromDelivery(delivery, webhookCheckTime(check))
	targets := checkHydrationTargets(repositoryID, repositoryFullName, check, payload.SHA, stamp)
	if len(targets) == 0 {
		return webhookProcessResult{Outcome: webhookOutcomeIgnored, RepositoryID: repositoryID}, nil
	}
	for _, target := range targets {
		if err := enqueueHydration(ctx, tx, delivery.DeliveryID, target, now); err != nil {
			return webhookProcessResult{}, err
		}
	}
	return webhookProcessResult{Outcome: webhookOutcomeHydrationRequested, RepositoryID: repositoryID}, nil
}

func applyOrResolveWebhookRepository(ctx context.Context, tx *sql.Tx, fullName string, repository normalizedRepository, stamp sourceStamp, complete bool, now time.Time) (*int64, bool, error) {
	if complete {
		repositoryID, result, err := applyWebhookRepository(ctx, tx, repository, stamp, now)
		if err != nil {
			return nil, false, err
		}
		return &repositoryID, result.Conflict, nil
	}
	repositoryID, found, err := resolveWebhookRepositoryID(ctx, tx, fullName)
	if err != nil || !found {
		return nil, false, err
	}
	return &repositoryID, false, nil
}

func applyWebhookRepository(ctx context.Context, tx *sql.Tx, repository normalizedRepository, stamp sourceStamp, now time.Time) (int64, projectionApplyResult, error) {
	return applyRepositoryProjection(ctx, tx, repository, stamp, now, true, false)
}

func applyRepositoryProjection(ctx context.Context, tx *sql.Tx, repository normalizedRepository, stamp sourceStamp, now time.Time, webhook bool, authoritative bool) (int64, projectionApplyResult, error) {
	var repositoryID int64
	var currentVersion string
	var currentUpdatedAt sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT id, source_version, source_updated_at
		FROM repositories
		WHERE github_node_id = ? OR (lower(github_owner) = lower(?) AND lower(github_name) = lower(?))
		ORDER BY CASE WHEN github_node_id = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, repository.NodeID, repository.Owner, repository.Name, repository.NodeID).Scan(&repositoryID, &currentVersion, &currentUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO repositories (
				github_node_id, github_database_id, github_owner, github_name,
				last_webhook_at, source_version, source_updated_at, synchronized_at,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, repository.NodeID, optionalInt64(repository.DatabaseID), repository.Owner, repository.Name,
			optionalTime(webhook, now), stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
			formatWebhookTime(now), formatWebhookTime(now))
		if err != nil {
			return 0, projectionApplyResult{}, fmt.Errorf("insert GitHub webhook repository: %w", err)
		}
		repositoryID, err = result.LastInsertId()
		if err != nil {
			return 0, projectionApplyResult{}, fmt.Errorf("read GitHub webhook repository ID: %w", err)
		}
		return repositoryID, projectionApplyResult{Changed: true}, nil
	}
	if err != nil {
		return 0, projectionApplyResult{}, fmt.Errorf("read GitHub webhook repository: %w", err)
	}

	current, err := storedSourceStamp(currentVersion, currentUpdatedAt)
	if err != nil {
		return 0, projectionApplyResult{}, fmt.Errorf("read GitHub webhook repository source: %w", err)
	}
	comparison := compareSource(stamp, current)
	result := projectionApplyResult{
		Stale:    comparison < 0,
		Conflict: !current.UpdatedAt.IsZero() && stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version,
	}
	apply := comparison > 0 || authoritative && stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version
	if apply {
		if _, err := tx.ExecContext(ctx, `
			UPDATE repositories
			SET github_node_id = ?,
				github_database_id = COALESCE(?, github_database_id),
				github_owner = ?,
				github_name = ?,
				last_webhook_at = CASE WHEN ? THEN ? ELSE last_webhook_at END,
				source_version = ?,
				source_updated_at = ?,
				synchronized_at = ?,
				updated_at = ?
			WHERE id = ?
		`, repository.NodeID, optionalInt64(repository.DatabaseID), repository.Owner, repository.Name,
			webhook, formatWebhookTime(now), stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
			formatWebhookTime(now), repositoryID); err != nil {
			return 0, projectionApplyResult{}, fmt.Errorf("update GitHub webhook repository: %w", err)
		}
		result.Changed = true
	} else if webhook {
		if _, err := tx.ExecContext(ctx, `
			UPDATE repositories SET last_webhook_at = ? WHERE id = ?
		`, formatWebhookTime(now), repositoryID); err != nil {
			return 0, projectionApplyResult{}, fmt.Errorf("touch GitHub webhook repository: %w", err)
		}
	}
	return repositoryID, result, nil
}

func resolveWebhookRepositoryID(ctx context.Context, tx *sql.Tx, fullName string) (int64, bool, error) {
	owner, name, ok := splitRepositoryFullName(fullName)
	if !ok {
		return 0, false, nil
	}
	var repositoryID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM repositories
		WHERE lower(github_owner) = lower(?) AND lower(github_name) = lower(?)
	`, owner, name).Scan(&repositoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve GitHub webhook repository: %w", err)
	}
	return repositoryID, true, nil
}

func applyWebhookIssue(ctx context.Context, tx *sql.Tx, repositoryID int64, issue normalizedIssue, stamp sourceStamp, now time.Time) (projectionApplyResult, error) {
	return applyIssueProjection(ctx, tx, repositoryID, issue, stamp, now, false)
}

func applyIssueProjection(ctx context.Context, tx *sql.Tx, repositoryID int64, issue normalizedIssue, stamp sourceStamp, now time.Time, authoritative bool) (projectionApplyResult, error) {
	var issueID int64
	var currentVersion string
	var currentUpdatedAt string
	var currentState string
	queryErr := tx.QueryRowContext(ctx, `
		SELECT id, source_version, source_updated_at, github_state
		FROM issues
		WHERE github_node_id = ? OR (repository_id = ? AND github_number = ?)
		ORDER BY CASE WHEN github_node_id = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, issue.NodeID, repositoryID, issue.Number, issue.NodeID).Scan(&issueID, &currentVersion, &currentUpdatedAt, &currentState)
	if queryErr != nil && !errors.Is(queryErr, sql.ErrNoRows) {
		return projectionApplyResult{}, fmt.Errorf("read GitHub webhook issue: %w", queryErr)
	}
	labelsJSON, err := json.Marshal(issue.Labels)
	if err != nil {
		return projectionApplyResult{}, fmt.Errorf("encode GitHub webhook issue labels: %w", err)
	}
	assigneesJSON, err := json.Marshal(issue.Assignees)
	if err != nil {
		return projectionApplyResult{}, fmt.Errorf("encode GitHub webhook issue assignees: %w", err)
	}
	resolvedWorkflowStateID, hasWorkflowState, err := resolveWebhookWorkflowStateID(ctx, tx, repositoryID, issue.Labels)
	if err != nil {
		return projectionApplyResult{}, err
	}
	var workflowStateID *int64
	if hasWorkflowState {
		workflowStateID = &resolvedWorkflowStateID
	}
	if errors.Is(queryErr, sql.ErrNoRows) {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO issues (
				repository_id, workflow_state_id, github_node_id, github_database_id, github_number,
				title, body, url, github_state, labels_json, assignees_json,
				source_version, source_updated_at, synchronized_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, repositoryID, optionalInt64(workflowStateID), issue.NodeID, optionalInt64(issue.DatabaseID), issue.Number,
			issue.Title, issue.Body, issue.URL, issue.State, string(labelsJSON), string(assigneesJSON),
			stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
			formatWebhookTime(issue.CreatedAt), formatWebhookTime(issue.UpdatedAt))
		if err != nil {
			return projectionApplyResult{}, fmt.Errorf("insert GitHub webhook issue: %w", err)
		}
		return projectionApplyResult{Changed: true}, nil
	}
	currentTime, err := time.Parse(time.RFC3339Nano, currentUpdatedAt)
	if err != nil {
		return projectionApplyResult{}, fmt.Errorf("parse GitHub webhook issue source timestamp: %w", err)
	}
	current := sourceStamp{UpdatedAt: currentTime, Version: currentVersion}
	comparison := compareSource(stamp, current)
	result := projectionApplyResult{
		Stale:    comparison < 0,
		Conflict: stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version,
	}
	force := authoritative && stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version
	if !force && (comparison < 0 || comparison == 0 && !strings.EqualFold(currentState, "deleted")) {
		return result, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		SET repository_id = ?,
			workflow_state_id = ?,
			github_node_id = ?,
			github_database_id = COALESCE(?, github_database_id),
			github_number = ?,
			title = ?,
			body = ?,
			url = ?,
			github_state = ?,
			labels_json = ?,
			assignees_json = ?,
			source_version = ?,
			source_updated_at = ?,
			synchronized_at = ?,
			created_at = ?,
			updated_at = ?
		WHERE id = ?
	`, repositoryID, optionalInt64(workflowStateID), issue.NodeID, optionalInt64(issue.DatabaseID), issue.Number,
		issue.Title, issue.Body, issue.URL, issue.State, string(labelsJSON), string(assigneesJSON),
		stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
		formatWebhookTime(issue.CreatedAt), formatWebhookTime(issue.UpdatedAt), issueID); err != nil {
		return projectionApplyResult{}, fmt.Errorf("update GitHub webhook issue: %w", err)
	}
	result.Changed = true
	return result, nil
}

func resolveWebhookWorkflowStateID(ctx context.Context, tx *sql.Tx, repositoryID int64, labels []string) (int64, bool, error) {
	if len(labels) == 0 {
		return 0, false, nil
	}
	args := make([]any, 0, len(labels)+1)
	args = append(args, repositoryID)
	for _, label := range labels {
		args = append(args, strings.ToLower(strings.TrimSpace(label)))
	}
	var workflowStateID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM workflow_states
		WHERE repository_id = ? AND lower(source_name) IN (`+placeholders(len(labels))+`)
		ORDER BY id
		LIMIT 1
	`, args...).Scan(&workflowStateID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve GitHub webhook workflow state: %w", err)
	}
	return workflowStateID, true, nil
}

func applyWebhookPullRequest(ctx context.Context, tx *sql.Tx, repositoryID int64, pullRequest normalizedPullRequest, stamp sourceStamp, now time.Time) (projectionApplyResult, error) {
	return applyPullRequestProjection(ctx, tx, repositoryID, pullRequest, stamp, now, false)
}

func applyPullRequestProjection(ctx context.Context, tx *sql.Tx, repositoryID int64, pullRequest normalizedPullRequest, stamp sourceStamp, now time.Time, authoritative bool) (projectionApplyResult, error) {
	var pullRequestID int64
	var currentVersion string
	var currentUpdatedAt string
	var currentState string
	err := tx.QueryRowContext(ctx, `
		SELECT id, source_version, source_updated_at, github_state
		FROM pull_requests
		WHERE github_node_id = ? OR (repository_id = ? AND github_number = ?)
		ORDER BY CASE WHEN github_node_id = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, pullRequest.NodeID, repositoryID, pullRequest.Number, pullRequest.NodeID).Scan(&pullRequestID, &currentVersion, &currentUpdatedAt, &currentState)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pull_requests (
				repository_id, github_node_id, github_database_id, github_number,
				title, url, github_state, draft, head_ref, head_sha, base_ref, base_sha,
				source_version, source_updated_at, synchronized_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, repositoryID, pullRequest.NodeID, optionalInt64(pullRequest.DatabaseID), pullRequest.Number,
			pullRequest.Title, pullRequest.URL, pullRequest.State, pullRequest.Draft,
			pullRequest.HeadRef, pullRequest.HeadSHA, pullRequest.BaseRef, pullRequest.BaseSHA,
			stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
			formatWebhookTime(pullRequest.CreatedAt), formatWebhookTime(pullRequest.UpdatedAt))
		if err != nil {
			return projectionApplyResult{}, fmt.Errorf("insert GitHub webhook pull request: %w", err)
		}
		return projectionApplyResult{Changed: true}, nil
	}
	if err != nil {
		return projectionApplyResult{}, fmt.Errorf("read GitHub webhook pull request: %w", err)
	}
	currentTime, err := time.Parse(time.RFC3339Nano, currentUpdatedAt)
	if err != nil {
		return projectionApplyResult{}, fmt.Errorf("parse GitHub webhook pull request source timestamp: %w", err)
	}
	current := sourceStamp{UpdatedAt: currentTime, Version: currentVersion}
	comparison := compareSource(stamp, current)
	result := projectionApplyResult{
		Stale:    comparison < 0,
		Conflict: stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version,
	}
	force := authoritative && stamp.UpdatedAt.Equal(current.UpdatedAt) && stamp.Version != current.Version
	if !force && (comparison < 0 || comparison == 0 && !strings.EqualFold(currentState, "deleted")) {
		return result, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pull_requests
		SET repository_id = ?,
			github_node_id = ?,
			github_database_id = COALESCE(?, github_database_id),
			github_number = ?,
			title = ?,
			url = ?,
			github_state = ?,
			draft = ?,
			head_ref = ?,
			head_sha = ?,
			base_ref = ?,
			base_sha = ?,
			source_version = ?,
			source_updated_at = ?,
			synchronized_at = ?,
			created_at = ?,
			updated_at = ?
		WHERE id = ?
	`, repositoryID, pullRequest.NodeID, optionalInt64(pullRequest.DatabaseID), pullRequest.Number,
		pullRequest.Title, pullRequest.URL, pullRequest.State, pullRequest.Draft,
		pullRequest.HeadRef, pullRequest.HeadSHA, pullRequest.BaseRef, pullRequest.BaseSHA,
		stamp.Version, formatWebhookTime(stamp.UpdatedAt), formatWebhookTime(now),
		formatWebhookTime(pullRequest.CreatedAt), formatWebhookTime(pullRequest.UpdatedAt), pullRequestID); err != nil {
		return projectionApplyResult{}, fmt.Errorf("update GitHub webhook pull request: %w", err)
	}
	result.Changed = true
	return result, nil
}

func enqueueHydration(ctx context.Context, tx *sql.Tx, deliveryID string, target hydrationTarget, now time.Time) error {
	if target.RepositoryFullName == "" || target.ObjectKind == "" || target.ObjectKey == "" || target.Source.Version == "" {
		return errors.New("GitHub webhook hydration target is incomplete")
	}
	var sourceUpdatedAt any
	if !target.Source.UpdatedAt.IsZero() {
		sourceUpdatedAt = formatWebhookTime(target.Source.UpdatedAt)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key,
			github_node_id, github_number, head_sha, reason,
			requested_source_updated_at, requested_source_version,
			first_delivery_id, last_delivery_id, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(repository_full_name, object_kind, object_key) WHERE status = 'pending'
		DO UPDATE SET
			repository_id = COALESCE(excluded.repository_id, github_hydration_requests.repository_id),
			github_node_id = COALESCE(excluded.github_node_id, github_hydration_requests.github_node_id),
			github_number = COALESCE(excluded.github_number, github_hydration_requests.github_number),
			head_sha = COALESCE(excluded.head_sha, github_hydration_requests.head_sha),
			reason = max(excluded.reason, github_hydration_requests.reason),
			requested_source_updated_at = CASE
				WHEN
					(github_hydration_requests.requested_source_updated_at IS NULL AND excluded.requested_source_updated_at IS NOT NULL)
					OR (
						github_hydration_requests.requested_source_updated_at IS NULL
						AND excluded.requested_source_updated_at IS NULL
						AND excluded.requested_source_version > github_hydration_requests.requested_source_version
					)
					OR (
						excluded.requested_source_updated_at IS NOT NULL
						AND github_hydration_requests.requested_source_updated_at IS NOT NULL
						AND (
							excluded.requested_source_updated_at > github_hydration_requests.requested_source_updated_at
							OR (
								excluded.requested_source_updated_at = github_hydration_requests.requested_source_updated_at
								AND excluded.requested_source_version > github_hydration_requests.requested_source_version
							)
						)
					)
				THEN excluded.requested_source_updated_at
				ELSE github_hydration_requests.requested_source_updated_at
			END,
			requested_source_version = CASE
				WHEN
					(github_hydration_requests.requested_source_updated_at IS NULL AND excluded.requested_source_updated_at IS NOT NULL)
					OR (
						github_hydration_requests.requested_source_updated_at IS NULL
						AND excluded.requested_source_updated_at IS NULL
						AND excluded.requested_source_version > github_hydration_requests.requested_source_version
					)
					OR (
						excluded.requested_source_updated_at IS NOT NULL
						AND github_hydration_requests.requested_source_updated_at IS NOT NULL
						AND (
							excluded.requested_source_updated_at > github_hydration_requests.requested_source_updated_at
							OR (
								excluded.requested_source_updated_at = github_hydration_requests.requested_source_updated_at
								AND excluded.requested_source_version > github_hydration_requests.requested_source_version
							)
						)
					)
				THEN excluded.requested_source_version
				ELSE github_hydration_requests.requested_source_version
			END,
			last_delivery_id = excluded.last_delivery_id,
			request_count = github_hydration_requests.request_count + 1,
			updated_at = excluded.updated_at
	`, optionalInt64(target.RepositoryID), target.RepositoryFullName, target.ObjectKind, target.ObjectKey,
		optionalString(target.GitHubNodeID), optionalInt(target.GitHubNumber), optionalString(target.HeadSHA), target.Reason,
		sourceUpdatedAt, target.Source.Version, deliveryID, deliveryID, formatWebhookTime(now), formatWebhookTime(now))
	if err != nil {
		return fmt.Errorf("enqueue GitHub webhook hydration: %w", err)
	}
	return nil
}

func normalizeWebhookRepository(repository *githubWebhookRepository, fallbackUpdatedAt time.Time) (normalizedRepository, sourceStamp, bool) {
	if repository == nil {
		return normalizedRepository{}, sourceStamp{}, false
	}
	fullName := webhookRepositoryFullName(repository)
	owner, name, ok := splitRepositoryFullName(fullName)
	if !ok || repository.NodeID == nil || strings.TrimSpace(*repository.NodeID) == "" {
		return normalizedRepository{}, sourceStamp{}, false
	}
	updatedAt, validUpdatedAt := parseWebhookTime(repository.UpdatedAt)
	if !validUpdatedAt {
		updatedAt = fallbackUpdatedAt
	}
	if updatedAt.IsZero() {
		return normalizedRepository{}, sourceStamp{}, false
	}
	normalized := normalizedRepository{
		NodeID:     strings.TrimSpace(*repository.NodeID),
		DatabaseID: positiveInt64(repository.DatabaseID),
		Owner:      owner,
		Name:       name,
	}
	stamp, err := newSourceStamp(updatedAt, normalized)
	if err != nil {
		return normalizedRepository{}, sourceStamp{}, false
	}
	return normalized, stamp, true
}

func normalizeWebhookIssue(issue *githubWebhookIssue) (normalizedIssue, sourceStamp, bool) {
	if issue == nil || issue.NodeID == nil || issue.Number == nil || issue.Title == nil || issue.HTMLURL == nil || issue.State == nil {
		return normalizedIssue{}, sourceStamp{}, false
	}
	body, bodyOK := webhookNullableString(issue.Body)
	labels, labelsOK := webhookStringList(issue.Labels, "name")
	assignees, assigneesOK := webhookStringList(issue.Assignees, "login")
	createdAt, createdOK := parseWebhookTime(issue.CreatedAt)
	updatedAt, updatedOK := parseWebhookTime(issue.UpdatedAt)
	if strings.TrimSpace(*issue.NodeID) == "" || *issue.Number <= 0 || strings.TrimSpace(*issue.Title) == "" ||
		strings.TrimSpace(*issue.HTMLURL) == "" || strings.TrimSpace(*issue.State) == "" ||
		!bodyOK || !labelsOK || !assigneesOK || !createdOK || !updatedOK {
		return normalizedIssue{}, sourceStamp{}, false
	}
	normalized := normalizedIssue{
		NodeID:     strings.TrimSpace(*issue.NodeID),
		DatabaseID: positiveInt64(issue.DatabaseID),
		Number:     *issue.Number,
		Title:      *issue.Title,
		Body:       body,
		URL:        strings.TrimSpace(*issue.HTMLURL),
		State:      strings.ToLower(strings.TrimSpace(*issue.State)),
		Labels:     labels,
		Assignees:  assignees,
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
	stamp, err := newSourceStamp(updatedAt, normalized)
	if err != nil {
		return normalizedIssue{}, sourceStamp{}, false
	}
	return normalized, stamp, true
}

func normalizeWebhookPullRequest(pullRequest *githubWebhookPullRequest) (normalizedPullRequest, sourceStamp, bool) {
	if pullRequest == nil || pullRequest.NodeID == nil || pullRequest.Number == nil || pullRequest.Title == nil ||
		pullRequest.HTMLURL == nil || pullRequest.State == nil || pullRequest.Draft == nil || pullRequest.Head == nil || pullRequest.Base == nil ||
		pullRequest.Head.Ref == nil || pullRequest.Head.SHA == nil || pullRequest.Base.Ref == nil || pullRequest.Base.SHA == nil {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	createdAt, createdOK := parseWebhookTime(pullRequest.CreatedAt)
	updatedAt, updatedOK := parseWebhookTime(pullRequest.UpdatedAt)
	if strings.TrimSpace(*pullRequest.NodeID) == "" || *pullRequest.Number <= 0 || strings.TrimSpace(*pullRequest.Title) == "" ||
		strings.TrimSpace(*pullRequest.HTMLURL) == "" || strings.TrimSpace(*pullRequest.State) == "" ||
		strings.TrimSpace(*pullRequest.Head.Ref) == "" || strings.TrimSpace(*pullRequest.Head.SHA) == "" ||
		strings.TrimSpace(*pullRequest.Base.Ref) == "" || strings.TrimSpace(*pullRequest.Base.SHA) == "" || !createdOK || !updatedOK {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	normalized := normalizedPullRequest{
		NodeID:     strings.TrimSpace(*pullRequest.NodeID),
		DatabaseID: positiveInt64(pullRequest.DatabaseID),
		Number:     *pullRequest.Number,
		Title:      *pullRequest.Title,
		URL:        strings.TrimSpace(*pullRequest.HTMLURL),
		State:      strings.ToLower(strings.TrimSpace(*pullRequest.State)),
		Draft:      *pullRequest.Draft,
		HeadRef:    strings.TrimSpace(*pullRequest.Head.Ref),
		HeadSHA:    strings.TrimSpace(*pullRequest.Head.SHA),
		BaseRef:    strings.TrimSpace(*pullRequest.Base.Ref),
		BaseSHA:    strings.TrimSpace(*pullRequest.Base.SHA),
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
	stamp, err := newSourceStamp(updatedAt, normalized)
	if err != nil {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	return normalized, stamp, true
}

func issueHydrationTarget(repositoryFullName string, issue *githubWebhookIssue, stamp sourceStamp, reason string) (hydrationTarget, bool) {
	target := hydrationTarget{RepositoryFullName: repositoryFullName, ObjectKind: "issue", Reason: reason, Source: stamp}
	if issue == nil || repositoryFullName == "" {
		return target, false
	}
	if issue.Number != nil && *issue.Number > 0 {
		target.GitHubNumber = *issue.Number
		target.ObjectKey = strconv.Itoa(*issue.Number)
	}
	if issue.NodeID != nil {
		target.GitHubNodeID = strings.TrimSpace(*issue.NodeID)
		if target.ObjectKey == "" {
			target.ObjectKey = target.GitHubNodeID
		}
	}
	return target, target.ObjectKey != ""
}

func pullRequestHydrationTarget(repositoryFullName string, pullRequest *githubWebhookPullRequest, stamp sourceStamp, reason string, kind string) (hydrationTarget, bool) {
	target := hydrationTarget{RepositoryFullName: repositoryFullName, ObjectKind: kind, Reason: reason, Source: stamp}
	if pullRequest == nil || repositoryFullName == "" {
		return target, false
	}
	if pullRequest.Number != nil && *pullRequest.Number > 0 {
		target.GitHubNumber = *pullRequest.Number
		target.ObjectKey = strconv.Itoa(*pullRequest.Number)
	}
	if pullRequest.NodeID != nil {
		target.GitHubNodeID = strings.TrimSpace(*pullRequest.NodeID)
		if target.ObjectKey == "" {
			target.ObjectKey = target.GitHubNodeID
		}
	}
	return target, target.ObjectKey != ""
}

func checkHydrationTargets(repositoryID *int64, repositoryFullName string, check *githubWebhookCheck, fallbackSHA *string, stamp sourceStamp) []hydrationTarget {
	targets := make([]hydrationTarget, 0)
	seen := make(map[int]struct{})
	if check != nil {
		for _, pullRequest := range check.PullRequests {
			if pullRequest.Number == nil || *pullRequest.Number <= 0 {
				continue
			}
			number := *pullRequest.Number
			if _, ok := seen[number]; ok {
				continue
			}
			seen[number] = struct{}{}
			targets = append(targets, hydrationTarget{
				RepositoryID:       repositoryID,
				RepositoryFullName: repositoryFullName,
				ObjectKind:         "pull_request_checks",
				ObjectKey:          strconv.Itoa(number),
				GitHubNumber:       number,
				Reason:             "checks_changed",
				Source:             stamp,
			})
		}
	}
	if len(targets) > 0 {
		return targets
	}
	sha := ""
	if check != nil && check.HeadSHA != nil {
		sha = strings.TrimSpace(*check.HeadSHA)
	}
	if sha == "" && fallbackSHA != nil {
		sha = strings.TrimSpace(*fallbackSHA)
	}
	if sha == "" {
		return targets
	}
	return append(targets, hydrationTarget{
		RepositoryID:       repositoryID,
		RepositoryFullName: repositoryFullName,
		ObjectKind:         "commit_checks",
		ObjectKey:          sha,
		HeadSHA:            sha,
		Reason:             "checks_changed",
		Source:             stamp,
	})
}

func newSourceStamp(updatedAt time.Time, canonical any) (sourceStamp, error) {
	return newSourceStampWithVersion(updatedAt, "1", canonical)
}

func newSourceStampWithVersion(updatedAt time.Time, version string, canonical any) (sourceStamp, error) {
	value, err := json.Marshal(canonical)
	if err != nil {
		return sourceStamp{}, fmt.Errorf("encode GitHub webhook source version: %w", err)
	}
	digest := sha256.Sum256(value)
	return sourceStamp{UpdatedAt: updatedAt.UTC(), Version: version + ":" + hex.EncodeToString(digest[:])}, nil
}

func sourceStampFromDelivery(delivery storedWebhook, updatedAt time.Time) sourceStamp {
	version := delivery.PayloadSHA256
	if version == "" {
		digest := sha256.Sum256(delivery.Payload)
		version = hex.EncodeToString(digest[:])
	}
	return sourceStamp{UpdatedAt: updatedAt.UTC(), Version: "1:" + version}
}

func storedSourceStamp(version string, updatedAt sql.NullString) (sourceStamp, error) {
	if !updatedAt.Valid || strings.TrimSpace(updatedAt.String) == "" {
		return sourceStamp{Version: version}, nil
	}
	value, err := time.Parse(time.RFC3339Nano, updatedAt.String)
	if err != nil {
		return sourceStamp{}, err
	}
	return sourceStamp{UpdatedAt: value.UTC(), Version: version}, nil
}

func compareSource(incoming sourceStamp, current sourceStamp) int {
	if incoming.UpdatedAt.After(current.UpdatedAt) {
		return 1
	}
	if incoming.UpdatedAt.Before(current.UpdatedAt) {
		return -1
	}
	return strings.Compare(incoming.Version, current.Version)
}

func projectionOutcome(result projectionApplyResult) webhookOutcome {
	if result.Changed {
		return webhookOutcomeApplied
	}
	if result.Stale {
		return webhookOutcomeSuperseded
	}
	return webhookOutcomeUnchanged
}

func webhookRepositoryFullName(repository *githubWebhookRepository) string {
	if repository == nil {
		return ""
	}
	if repository.FullName != nil && strings.TrimSpace(*repository.FullName) != "" {
		return strings.TrimSpace(*repository.FullName)
	}
	if repository.Owner == nil || repository.Owner.Login == nil || repository.Name == nil {
		return ""
	}
	owner := strings.TrimSpace(*repository.Owner.Login)
	name := strings.TrimSpace(*repository.Name)
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func splitRepositoryFullName(fullName string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(fullName), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func webhookObjectTime(object any) time.Time {
	switch value := object.(type) {
	case *githubWebhookIssue:
		if value != nil {
			updatedAt, _ := parseWebhookTime(value.UpdatedAt)
			return updatedAt
		}
	case *githubWebhookPullRequest:
		if value != nil {
			updatedAt, _ := parseWebhookTime(value.UpdatedAt)
			return updatedAt
		}
	}
	return time.Time{}
}

func webhookReviewTime(review *githubWebhookReview) time.Time {
	if review == nil {
		return time.Time{}
	}
	value, _ := parseWebhookTime(review.SubmittedAt)
	return value
}

func webhookCheckTime(check *githubWebhookCheck) time.Time {
	if check == nil {
		return time.Time{}
	}
	if value, ok := parseWebhookTime(check.UpdatedAt); ok {
		return value
	}
	value, _ := parseWebhookTime(check.CompletedAt)
	return value
}

func parseWebhookTime(raw json.RawMessage) (time.Time, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return time.Time{}, false
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
		return parsed.UTC(), err == nil
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if seconds > 1_000_000_000_000 {
		return time.UnixMilli(seconds).UTC(), true
	}
	return time.Unix(seconds, 0).UTC(), true
}

func webhookNullableString(raw json.RawMessage) (string, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return "", false
	}
	if bytes.Equal(value, []byte("null")) {
		return "", true
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", false
	}
	return text, true
}

func webhookStringList(raw json.RawMessage, objectField string) ([]string, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, false
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(value, &entries); err != nil {
		return nil, false
	}
	values := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var text string
		if err := json.Unmarshal(entry, &text); err != nil {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(entry, &object); err != nil {
				return nil, false
			}
			field, ok := object[objectField]
			if !ok || json.Unmarshal(field, &text) != nil {
				return nil, false
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, false
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		values = append(values, text)
	}
	sort.Strings(values)
	return values, true
}

func positiveInt64(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	copy := *value
	return &copy
}

func optionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func optionalTime(enabled bool, value time.Time) any {
	if !enabled {
		return nil
	}
	return formatWebhookTime(value)
}

func optionalString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
