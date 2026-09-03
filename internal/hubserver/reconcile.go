package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	checkpointWebhook     = "webhook"
	checkpointIncremental = "incremental"
	checkpointFullRepair  = "full_repair"
)

type ReconcileMode string

const (
	ReconcileIncremental ReconcileMode = "incremental"
	ReconcileFullRepair  ReconcileMode = "full_repair"
)

type RepositoryTarget struct {
	ID         int64
	NodeID     string
	DatabaseID *int64
	Owner      string
	Name       string
}

type ReconcileRequest struct {
	Repository RepositoryTarget
	Mode       ReconcileMode
	Since      *time.Time
	Through    time.Time
	Hydrations []HydrationRequest
}

type HydrationRequest struct {
	ID           int64
	ObjectKind   string
	ObjectKey    string
	GitHubNodeID string
	GitHubNumber int
	HeadSHA      string
	RequestCount int
}

type RepositorySource struct {
	NodeID     string
	DatabaseID *int64
	Owner      string
	Name       string
	UpdatedAt  time.Time
}

type IssueSource struct {
	NodeID     string
	DatabaseID *int64
	Number     int
	Title      string
	Body       string
	URL        string
	State      string
	AuthorID   string
	Labels     []string
	Assignees  []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type PullRequestSource struct {
	NodeID     string
	DatabaseID *int64
	Number     int
	Title      string
	URL        string
	State      string
	Draft      bool
	HeadRef    string
	HeadSHA    string
	BaseRef    string
	BaseSHA    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type PullRequestDetailSource struct {
	Number         int
	HeadSHA        string
	MergeableState *string
	Checks         *tracker.CheckSummary
	Reviews        *tracker.ReviewSummary
}

type ReconcileSnapshot struct {
	Repository         RepositorySource
	Issues             []IssueSource
	PullRequests       []PullRequestSource
	PullRequestDetails []PullRequestDetailSource
}

type ReconcileBackend interface {
	Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error)
}

type reconcileTarget struct {
	RepositoryTarget
	Cursor         *time.Time
	HydrationSince *time.Time
	Hydrations     []HydrationRequest
}

func (s *Service) runGitHubReconciliation(ctx context.Context) {
	defer close(s.reconcileDone)
	if s.config.ReconcileBackend == nil {
		return
	}
	s.reconcileAllRepositories(ctx, ReconcileFullRepair)
	incrementalTicker := time.NewTicker(s.config.ReconcileInterval)
	fullTicker := time.NewTicker(s.config.FullRepairInterval)
	defer incrementalTicker.Stop()
	defer fullTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-incrementalTicker.C:
			s.reconcileAllRepositories(ctx, ReconcileIncremental)
		case <-fullTicker.C:
			s.reconcileAllRepositories(ctx, ReconcileFullRepair)
		}
	}
}

func (s *Service) reconcileAllRepositories(ctx context.Context, mode ReconcileMode) {
	targets, err := s.database.reconcileTargets(ctx)
	if err != nil {
		s.config.Logger.Error("list GitHub repositories for reconciliation", "mode", mode, "error", err)
		return
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := s.reconcileRepository(ctx, target, mode); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.Error("reconcile GitHub repository", "repository", target.Owner+"/"+target.Name, "mode", mode, "error", err)
		}
	}
}

func (s *Service) reconcileRepository(ctx context.Context, target reconcileTarget, mode ReconcileMode) error {
	startedAt := s.config.now().UTC()
	checkpoint := string(mode)
	if err := s.database.recordCheckpointStarted(ctx, target.ID, checkpoint, startedAt); err != nil {
		return err
	}
	request := ReconcileRequest{
		Repository: target.RepositoryTarget,
		Mode:       mode,
		Through:    startedAt,
		Hydrations: append([]HydrationRequest(nil), target.Hydrations...),
	}
	if mode == ReconcileIncremental {
		request.Since = earliestTime(target.Cursor, target.HydrationSince)
	}
	snapshot, err := s.config.ReconcileBackend.Reconcile(ctx, request)
	if err != nil {
		failureErr := s.database.recordCheckpointFailure(ctx, target.ID, checkpoint, s.config.now().UTC(), err)
		return errors.Join(err, failureErr)
	}
	completedAt := s.config.now().UTC()
	if err := s.database.applyReconcileSnapshot(ctx, target, mode, request.Through, completedAt, snapshot); err != nil {
		failureErr := s.database.recordCheckpointFailure(ctx, target.ID, checkpoint, completedAt, err)
		return errors.Join(err, failureErr)
	}
	return nil
}

func (s *Service) stopGitHubReconciliation() error {
	if s == nil || s.reconcileCancel == nil || s.reconcileDone == nil {
		return nil
	}
	s.reconcileStopOnce.Do(func() {
		s.reconcileCancel()
		<-s.reconcileDone
	})
	return nil
}

func (d *database) reconcileTargets(ctx context.Context) ([]reconcileTarget, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, github_node_id, github_database_id, github_owner, github_name, reconcile_cursor
		FROM repositories
		ORDER BY lower(github_owner), lower(github_name), id
	`)
	if err != nil {
		return nil, fmt.Errorf("list GitHub reconciliation targets: %w", err)
	}
	defer rows.Close()
	var targets []reconcileTarget
	for rows.Next() {
		var target reconcileTarget
		var databaseID sql.NullInt64
		var cursor sql.NullString
		if err := rows.Scan(&target.ID, &target.NodeID, &databaseID, &target.Owner, &target.Name, &cursor); err != nil {
			return nil, fmt.Errorf("scan GitHub reconciliation target: %w", err)
		}
		if databaseID.Valid {
			target.DatabaseID = &databaseID.Int64
		}
		var parseErr error
		cursorValue, cursorValid, parseErr := parseCheckpointTime(cursor)
		if parseErr != nil {
			return nil, fmt.Errorf("parse GitHub reconciliation cursor for %s/%s: %w", target.Owner, target.Name, parseErr)
		}
		if cursorValid {
			target.Cursor = &cursorValue
		}
		targets = append(targets, target)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close GitHub reconciliation targets: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate GitHub reconciliation targets: %w", err)
	}
	for index := range targets {
		hydrations, since, err := d.pendingHydrationRequests(ctx, targets[index].ID)
		if err != nil {
			return nil, fmt.Errorf("list GitHub hydration requests for %s/%s: %w", targets[index].Owner, targets[index].Name, err)
		}
		targets[index].Hydrations = hydrations
		targets[index].HydrationSince = since
	}
	return targets, nil
}

func (d *database) pendingHydrationRequests(ctx context.Context, repositoryID int64) ([]HydrationRequest, *time.Time, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, object_kind, object_key, github_node_id, github_number, head_sha, request_count, requested_source_updated_at
		FROM github_hydration_requests
		WHERE repository_id = ? AND status = 'pending'
		ORDER BY id
	`, repositoryID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var requests []HydrationRequest
	var since *time.Time
	for rows.Next() {
		var request HydrationRequest
		var githubNodeID sql.NullString
		var githubNumber sql.NullInt64
		var headSHA sql.NullString
		var requestedAt sql.NullString
		if err := rows.Scan(&request.ID, &request.ObjectKind, &request.ObjectKey, &githubNodeID, &githubNumber, &headSHA, &request.RequestCount, &requestedAt); err != nil {
			return nil, nil, err
		}
		if githubNodeID.Valid {
			request.GitHubNodeID = githubNodeID.String
		}
		if githubNumber.Valid {
			request.GitHubNumber = int(githubNumber.Int64)
		}
		if headSHA.Valid {
			request.HeadSHA = headSHA.String
		}
		value, valid, err := parseCheckpointTime(requestedAt)
		if err != nil {
			return nil, nil, err
		}
		if valid {
			since = earliestTime(since, &value)
		}
		requests = append(requests, request)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return requests, since, nil
}

func (d *database) applyReconcileSnapshot(ctx context.Context, target reconcileTarget, mode ReconcileMode, cursor time.Time, completedAt time.Time, snapshot ReconcileSnapshot) error {
	if err := validateReconcileSnapshot(snapshot); err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin GitHub reconciliation: %w", err)
	}
	defer tx.Rollback()
	repository := normalizedRepository{
		NodeID: snapshot.Repository.NodeID, DatabaseID: snapshot.Repository.DatabaseID,
		Owner: snapshot.Repository.Owner, Name: snapshot.Repository.Name,
	}
	repositoryStamp, err := newSourceStamp(snapshot.Repository.UpdatedAt, repository)
	if err != nil {
		return err
	}
	repositoryID, _, err := applyRepositoryProjection(ctx, tx, repository, repositoryStamp, completedAt, false, true)
	if err != nil {
		return err
	}
	issueNodeIDs := make(map[string]struct{}, len(snapshot.Issues))
	for _, source := range snapshot.Issues {
		issue := normalizedIssue(source)
		if issue.Labels == nil {
			issue.Labels = []string{}
		}
		if issue.Assignees == nil {
			issue.Assignees = []string{}
		}
		stamp, err := newSourceStamp(issue.UpdatedAt, issue)
		if err != nil {
			return err
		}
		if _, err := applyIssueProjection(ctx, tx, repositoryID, issue, stamp, completedAt, true); err != nil {
			return err
		}
		issueNodeIDs[issue.NodeID] = struct{}{}
	}
	pullRequestNodeIDs := make(map[string]struct{}, len(snapshot.PullRequests))
	for _, source := range snapshot.PullRequests {
		pullRequest := normalizedPullRequest(source)
		stamp, err := newSourceStamp(pullRequest.UpdatedAt, pullRequest)
		if err != nil {
			return err
		}
		if _, err := applyPullRequestProjection(ctx, tx, repositoryID, pullRequest, stamp, completedAt, true); err != nil {
			return err
		}
		pullRequestNodeIDs[pullRequest.NodeID] = struct{}{}
	}
	for _, details := range snapshot.PullRequestDetails {
		if err := applyPullRequestDetails(ctx, tx, repositoryID, details, completedAt); err != nil {
			return err
		}
	}
	if mode == ReconcileFullRepair {
		if err := markMissingProjectionRows(ctx, tx, "issues", `
			SELECT id, github_node_id FROM issues
			WHERE repository_id = ? AND github_state <> 'deleted'
		`, `
			UPDATE issues SET github_state = 'deleted', synchronized_at = ?, updated_at = ? WHERE id = ?
		`, repositoryID, issueNodeIDs, completedAt); err != nil {
			return err
		}
		if err := markMissingProjectionRows(ctx, tx, "pull_requests", `
			SELECT id, github_node_id FROM pull_requests
			WHERE repository_id = ? AND github_state <> 'deleted'
		`, `
			UPDATE pull_requests SET github_state = 'deleted', synchronized_at = ?, updated_at = ? WHERE id = ?
		`, repositoryID, pullRequestNodeIDs, completedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE repositories
		SET reconcile_cursor = ?, last_reconciled_at = ?, synchronized_at = ?, updated_at = ?
		WHERE id = ?
	`, formatWebhookTime(cursor), formatWebhookTime(completedAt), formatWebhookTime(completedAt), formatWebhookTime(completedAt), repositoryID); err != nil {
		return fmt.Errorf("update GitHub reconciliation cursor: %w", err)
	}
	if err := recordCheckpointSuccessTx(ctx, tx, repositoryID, string(mode), cursor, completedAt); err != nil {
		return err
	}
	for _, hydration := range target.Hydrations {
		if _, err := tx.ExecContext(ctx, `
			UPDATE github_hydration_requests
			SET status = 'completed', completed_at = ?, last_error = NULL, updated_at = ?
			WHERE id = ? AND repository_id = ? AND status = 'pending' AND request_count = ?
		`, formatWebhookTime(completedAt), formatWebhookTime(completedAt), hydration.ID, repositoryID, hydration.RequestCount); err != nil {
			return fmt.Errorf("complete GitHub hydration request %d: %w", hydration.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit GitHub reconciliation: %w", err)
	}
	return nil
}

func applyPullRequestDetails(ctx context.Context, tx *sql.Tx, repositoryID int64, details PullRequestDetailSource, now time.Time) error {
	if details.Number <= 0 && strings.TrimSpace(details.HeadSHA) == "" {
		return errors.New("GitHub reconciliation returned pull request details without an identity")
	}
	var pullRequestID int64
	var state string
	var draft bool
	var mergeableState string
	var checksJSON string
	var reviewsJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT id, github_state, draft, mergeable_state, checks_summary_json, reviews_summary_json
		FROM pull_requests
		WHERE repository_id = ? AND (github_number = ? OR (? <> '' AND head_sha = ?))
		ORDER BY CASE WHEN github_number = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, repositoryID, details.Number, details.HeadSHA, details.HeadSHA, details.Number).Scan(
		&pullRequestID, &state, &draft, &mergeableState, &checksJSON, &reviewsJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read GitHub pull request details: %w", err)
	}
	var checks tracker.CheckSummary
	if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
		return fmt.Errorf("decode GitHub pull request checks summary: %w", err)
	}
	var reviews tracker.ReviewSummary
	if err := json.Unmarshal([]byte(reviewsJSON), &reviews); err != nil {
		return fmt.Errorf("decode GitHub pull request reviews summary: %w", err)
	}
	if details.Checks != nil {
		checks = *details.Checks
	}
	if details.Reviews != nil {
		reviews = *details.Reviews
	}
	if details.MergeableState != nil {
		mergeableState = strings.ToLower(strings.TrimSpace(*details.MergeableState))
	}
	checksBytes, err := json.Marshal(checks)
	if err != nil {
		return fmt.Errorf("encode GitHub pull request checks summary: %w", err)
	}
	reviewsBytes, err := json.Marshal(reviews)
	if err != nil {
		return fmt.Errorf("encode GitHub pull request reviews summary: %w", err)
	}
	mergeReady := strings.EqualFold(state, "open") && !draft && mergeableState == "clean" &&
		checks.Total > 0 && checks.Pending == 0 && checks.Failed == 0 && strings.EqualFold(checks.Conclusion, "success") &&
		reviews.ChangesRequested == 0 && !strings.EqualFold(reviews.Decision, "changes_requested")
	if _, err := tx.ExecContext(ctx, `
		UPDATE pull_requests
		SET mergeable_state = ?, checks_summary_json = ?, reviews_summary_json = ?,
			merge_ready = ?, merge_readiness_refreshed_at = ?, synchronized_at = ?, updated_at = ?
		WHERE id = ?
	`, mergeableState, string(checksBytes), string(reviewsBytes), mergeReady,
		formatWebhookTime(now), formatWebhookTime(now), formatWebhookTime(now), pullRequestID); err != nil {
		return fmt.Errorf("update GitHub pull request details: %w", err)
	}
	return nil
}

func validateReconcileSnapshot(snapshot ReconcileSnapshot) error {
	repository := snapshot.Repository
	if strings.TrimSpace(repository.NodeID) == "" || strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Name) == "" || repository.UpdatedAt.IsZero() {
		return errors.New("GitHub reconciliation returned an incomplete repository")
	}
	for _, issue := range snapshot.Issues {
		if strings.TrimSpace(issue.NodeID) == "" || issue.Number <= 0 || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.URL) == "" || strings.TrimSpace(issue.State) == "" || issue.CreatedAt.IsZero() || issue.UpdatedAt.IsZero() {
			return fmt.Errorf("GitHub reconciliation returned incomplete issue %d", issue.Number)
		}
	}
	for _, pullRequest := range snapshot.PullRequests {
		if strings.TrimSpace(pullRequest.NodeID) == "" || pullRequest.Number <= 0 || strings.TrimSpace(pullRequest.Title) == "" || strings.TrimSpace(pullRequest.URL) == "" || strings.TrimSpace(pullRequest.State) == "" || strings.TrimSpace(pullRequest.HeadRef) == "" || strings.TrimSpace(pullRequest.HeadSHA) == "" || strings.TrimSpace(pullRequest.BaseRef) == "" || strings.TrimSpace(pullRequest.BaseSHA) == "" || pullRequest.CreatedAt.IsZero() || pullRequest.UpdatedAt.IsZero() {
			return fmt.Errorf("GitHub reconciliation returned incomplete pull request %d", pullRequest.Number)
		}
	}
	return nil
}

func markMissingProjectionRows(ctx context.Context, tx *sql.Tx, label string, selectStatement string, updateStatement string, repositoryID int64, present map[string]struct{}, now time.Time) error {
	rows, err := tx.QueryContext(ctx, selectStatement, repositoryID)
	if err != nil {
		return fmt.Errorf("list %s for GitHub full repair: %w", label, err)
	}
	defer rows.Close()
	var missing []int64
	for rows.Next() {
		var id int64
		var nodeID string
		if err := rows.Scan(&id, &nodeID); err != nil {
			return fmt.Errorf("scan %s for GitHub full repair: %w", label, err)
		}
		if _, ok := present[nodeID]; !ok {
			missing = append(missing, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s GitHub full repair rows: %w", label, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s for GitHub full repair: %w", label, err)
	}
	for _, id := range missing {
		if _, err := tx.ExecContext(ctx, updateStatement, formatWebhookTime(now), formatWebhookTime(now), id); err != nil {
			return fmt.Errorf("mark missing %s row: %w", label, err)
		}
	}
	return nil
}

func (d *database) recordCheckpointStarted(ctx context.Context, repositoryID int64, name string, now time.Time) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO sync_checkpoints (repository_id, checkpoint_name, last_started_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repository_id, checkpoint_name) DO UPDATE SET
			last_started_at = excluded.last_started_at,
			updated_at = excluded.updated_at
	`, repositoryID, name, formatWebhookTime(now), formatWebhookTime(now))
	if err != nil {
		return fmt.Errorf("record GitHub %s start: %w", name, err)
	}
	return nil
}

func (d *database) recordCheckpointFailure(ctx context.Context, repositoryID int64, name string, now time.Time, failure error) error {
	state := fmt.Sprintf(`{"last_error_at":%q}`, formatWebhookTime(now))
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO sync_checkpoints (repository_id, checkpoint_name, state_json, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repository_id, checkpoint_name) DO UPDATE SET
			state_json = excluded.state_json,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
	`, repositoryID, name, state, failure.Error(), formatWebhookTime(now))
	if err != nil {
		return fmt.Errorf("record GitHub %s failure: %w", name, err)
	}
	return nil
}

func recordCheckpointSuccessTx(ctx context.Context, tx *sql.Tx, repositoryID int64, name string, cursor time.Time, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sync_checkpoints (repository_id, checkpoint_name, cursor, last_successful_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repository_id, checkpoint_name) DO UPDATE SET
			cursor = excluded.cursor,
			last_successful_at = excluded.last_successful_at,
			updated_at = excluded.updated_at
	`, repositoryID, name, formatWebhookTime(cursor), formatWebhookTime(now), formatWebhookTime(now))
	if err != nil {
		return fmt.Errorf("record GitHub %s success: %w", name, err)
	}
	return nil
}

func parseCheckpointTime(value sql.NullString) (time.Time, bool, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false, err
	}
	parsed = parsed.UTC()
	return parsed, true, nil
}

func earliestTime(values ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, value := range values {
		if value == nil || value.IsZero() {
			continue
		}
		if earliest == nil || value.Before(*earliest) {
			copy := value.UTC()
			earliest = &copy
		}
	}
	return earliest
}
