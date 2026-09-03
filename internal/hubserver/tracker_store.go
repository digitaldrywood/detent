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

const workItemColumns = `
SELECT
  i.id,
  r.id,
  r.github_node_id,
  r.github_owner,
  r.github_name,
  i.github_node_id,
  i.github_database_id,
  i.github_number,
  i.title,
  i.body,
  i.url,
  i.github_state,
  ws.id,
  ws.source_name,
  ws.detent_state,
  ws.terminal,
  ws.dispatchable,
  q.scope,
  q.state,
  q.rank,
  q.priority_override,
  i.author_login,
  i.labels_json,
  i.assignees_json,
  i.source_updated_at,
  i.synchronized_at,
  i.created_at,
  i.updated_at
FROM issues i
JOIN repositories r ON r.id = i.repository_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (
  SELECT candidate.id
  FROM queue_entries candidate
  WHERE candidate.issue_id = i.id
    AND (? = '' OR candidate.scope = ?)
  ORDER BY CASE WHEN candidate.scope = ? THEN 0 ELSE 1 END, candidate.scope, candidate.id
  LIMIT 1
)
`

const candidateOrder = `
ORDER BY
  CASE q.priority_override WHEN 0 THEN 0 WHEN 1 THEN 1 WHEN 2 THEN 2 WHEN 3 THEN 3 ELSE 4 END,
  CASE WHEN q.rank IS NULL OR trim(q.rank) = '' THEN 1 ELSE 0 END,
  trim(q.rank),
  i.created_at,
  lower(trim(r.github_owner)),
  lower(trim(r.github_name)),
  i.github_number
LIMIT ?`

type databaseQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

var _ tracker.Store = (*database)(nil)

func (d *database) ListCandidateRecords(ctx context.Context, query tracker.CandidateQuery) ([]tracker.Record, error) {
	if query.Limit < 0 {
		return nil, fmt.Errorf("%w: limit must not be negative", tracker.ErrInvalidCandidateQuery)
	}
	query.Scope = strings.TrimSpace(query.Scope)
	workflowStates := normalizedQueryStrings(query.WorkflowStates)
	if len(query.WorkflowStates) > 0 && len(workflowStates) == 0 {
		return nil, fmt.Errorf("%w: workflow states must include a non-empty value", tracker.ErrInvalidCandidateQuery)
	}

	clauses := []string{
		"lower(trim(i.github_state)) = 'open'",
		"ws.id IS NOT NULL",
		"ws.terminal = 0",
		"lower(trim(ws.detent_state)) <> 'cancelled'",
		"ws.dispatchable = 1",
	}
	args := []any{query.Scope, query.Scope, query.Scope}
	if len(workflowStates) > 0 {
		clauses = append(clauses, "lower(ws.detent_state) IN ("+placeholders(len(workflowStates))+")")
		for _, state := range workflowStates {
			args = append(args, state)
		}
	}
	if query.Scope != "" {
		clauses = append(clauses, "q.id IS NOT NULL")
	}
	repositoryIDs, err := normalizedRepositoryIDs(query.RepositoryIDs)
	if err != nil {
		return nil, err
	}
	if len(repositoryIDs) > 0 {
		clauses = append(clauses, "r.id IN ("+placeholders(len(repositoryIDs))+")")
		for _, id := range repositoryIDs {
			args = append(args, id)
		}
	}
	limit := query.Limit
	if limit == 0 {
		limit = tracker.DefaultCandidateLimit
	}
	args = append(args, limit)
	statement := workItemColumns + "WHERE " + strings.Join(clauses, " AND ") + candidateOrder

	return d.readWorkItemRecords(ctx, statement, args)
}

func (d *database) GetWorkItemRecords(ctx context.Context, ids []tracker.WorkItemID) ([]tracker.Record, error) {
	ids, err := normalizedWorkItemIDs(ids)
	if err != nil || len(ids) == 0 {
		return []tracker.Record{}, err
	}
	args := []any{"", "", ""}
	for _, id := range ids {
		args = append(args, id)
	}
	statement := workItemColumns + "WHERE i.id IN (" + placeholders(len(ids)) + ") ORDER BY i.id"
	return d.readWorkItemRecords(ctx, statement, args)
}

func (d *database) readWorkItemRecords(ctx context.Context, statement string, args []any) (records []tracker.Record, resultErr error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin hub tracker read: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()

	records, err = queryWorkItemRecords(ctx, tx, statement, args)
	if err != nil {
		return nil, err
	}
	if err := enrichWorkItemRecords(ctx, tx, records); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit hub tracker read: %w", err)
	}
	return records, nil
}

func queryWorkItemRecords(ctx context.Context, queryer databaseQueryer, statement string, args []any) ([]tracker.Record, error) {
	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query hub work items: %w", err)
	}
	defer rows.Close()

	records := make([]tracker.Record, 0)
	for rows.Next() {
		record, err := scanWorkItemRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hub work items: %w", err)
	}
	return records, nil
}

func scanWorkItemRecord(row interface{ Scan(...any) error }) (tracker.Record, error) {
	var record tracker.Record
	var githubDatabaseID sql.NullInt64
	var workflowID sql.NullInt64
	var workflowSourceName sql.NullString
	var workflowName sql.NullString
	var workflowTerminal sql.NullBool
	var workflowDispatchable sql.NullBool
	var queueScope sql.NullString
	var queueState sql.NullString
	var queueRank sql.NullString
	var priorityRank sql.NullInt64
	var authorLogin string
	var labelsJSON string
	var assigneesJSON string
	var sourceUpdatedAt string
	var sourceSyncedAt string
	var createdAt string
	var updatedAt string
	err := row.Scan(
		&record.ID,
		&record.Repository.ID,
		&record.Repository.GitHubNodeID,
		&record.Repository.Owner,
		&record.Repository.Name,
		&record.GitHub.NodeID,
		&githubDatabaseID,
		&record.GitHub.Number,
		&record.Title,
		&record.Body,
		&record.URL,
		&record.SourceState,
		&workflowID,
		&workflowSourceName,
		&workflowName,
		&workflowTerminal,
		&workflowDispatchable,
		&queueScope,
		&queueState,
		&queueRank,
		&priorityRank,
		&authorLogin,
		&labelsJSON,
		&assigneesJSON,
		&sourceUpdatedAt,
		&sourceSyncedAt,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return tracker.Record{}, fmt.Errorf("scan hub work item: %w", err)
	}
	if githubDatabaseID.Valid {
		record.GitHub.DatabaseID = &githubDatabaseID.Int64
	}
	if workflowID.Valid {
		record.WorkflowState = &tracker.WorkflowState{
			ID:           workflowID.Int64,
			SourceName:   workflowSourceName.String,
			Name:         workflowName.String,
			Terminal:     workflowTerminal.Bool,
			Dispatchable: workflowDispatchable.Bool,
		}
	}
	if queueScope.Valid {
		record.Queue = &tracker.QueueSummary{
			Scope: queueScope.String,
			State: queueState.String,
			Rank:  queueRank.String,
		}
		if priorityRank.Valid {
			priority := int(priorityRank.Int64)
			record.Queue.PriorityRank = &priority
		}
	}
	record.AuthorID = authorLogin
	if err := json.Unmarshal([]byte(labelsJSON), &record.Labels); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item labels: %w", err)
	}
	if err := json.Unmarshal([]byte(assigneesJSON), &record.Assignees); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item assignees: %w", err)
	}
	if record.SourceUpdatedAt, err = parsedTime(sourceUpdatedAt); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item source updated timestamp: %w", err)
	}
	if record.SourceSyncedAt, err = parsedTime(sourceSyncedAt); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item synchronized timestamp: %w", err)
	}
	if record.CreatedAt, err = parsedTime(createdAt); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item created timestamp: %w", err)
	}
	if record.UpdatedAt, err = parsedTime(updatedAt); err != nil {
		return tracker.Record{}, fmt.Errorf("decode hub work item updated timestamp: %w", err)
	}
	record.SyncStatus = tracker.SyncStatusSynced
	record.ObservedAt = time.Now().UTC()
	return record, nil
}

func enrichWorkItemRecords(ctx context.Context, queryer databaseQueryer, records []tracker.Record) error {
	if len(records) == 0 {
		return nil
	}
	index := make(map[tracker.WorkItemID]*tracker.Record, len(records))
	ids := make([]tracker.WorkItemID, 0, len(records))
	for i := range records {
		index[records[i].ID] = &records[i]
		ids = append(ids, records[i].ID)
	}
	if err := loadWorkItemRelations(ctx, queryer, index, ids); err != nil {
		return err
	}
	if err := loadWorkItemLeases(ctx, queryer, index, ids); err != nil {
		return err
	}
	if err := loadWorkItemPullRequests(ctx, queryer, index, ids); err != nil {
		return err
	}
	return loadWorkItemSyncStatus(ctx, queryer, index, ids)
}

func loadWorkItemRelations(ctx context.Context, queryer databaseQueryer, index map[tracker.WorkItemID]*tracker.Record, ids []tracker.WorkItemID) error {
	marks := placeholders(len(ids))
	statement := `
SELECT d.dependent_issue_id, 'blocker', related.id, r.github_owner, r.github_name, related.github_number, related.title, related.url, related.github_state, ws.id, ws.source_name, ws.detent_state, ws.terminal, ws.dispatchable
FROM issue_dependencies d
JOIN issues related ON related.id = d.blocker_issue_id
JOIN repositories r ON r.id = related.repository_id
LEFT JOIN workflow_states ws ON ws.id = related.workflow_state_id
WHERE d.dependent_issue_id IN (` + marks + `)
UNION ALL
SELECT d.blocker_issue_id, 'dependent', related.id, r.github_owner, r.github_name, related.github_number, related.title, related.url, related.github_state, ws.id, ws.source_name, ws.detent_state, ws.terminal, ws.dispatchable
FROM issue_dependencies d
JOIN issues related ON related.id = d.dependent_issue_id
JOIN repositories r ON r.id = related.repository_id
LEFT JOIN workflow_states ws ON ws.id = related.workflow_state_id
WHERE d.blocker_issue_id IN (` + marks + `)
ORDER BY 1, 2, 3`
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("query hub work item relationships: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var parentID tracker.WorkItemID
		var relation string
		var reference tracker.WorkItemReference
		var owner string
		var name string
		var sourceState string
		var workflowID sql.NullInt64
		var workflowSourceName sql.NullString
		var workflowName sql.NullString
		var workflowTerminal sql.NullBool
		var workflowDispatchable sql.NullBool
		if err := rows.Scan(&parentID, &relation, &reference.ID, &owner, &name, &reference.IssueNumber, &reference.Title, &reference.URL, &sourceState, &workflowID, &workflowSourceName, &workflowName, &workflowTerminal, &workflowDispatchable); err != nil {
			return fmt.Errorf("scan hub work item relationship: %w", err)
		}
		reference.Repository = owner + "/" + name
		reference.SourceState = tracker.SourceState(strings.ToLower(strings.TrimSpace(sourceState)))
		if workflowID.Valid {
			reference.WorkflowState = &tracker.WorkflowState{ID: workflowID.Int64, SourceName: workflowSourceName.String, Name: workflowName.String, Terminal: workflowTerminal.Bool, Dispatchable: workflowDispatchable.Bool}
		}
		record, err := indexedWorkItem(index, parentID)
		if err != nil {
			return err
		}
		if relation == "blocker" {
			record.Blockers = append(record.Blockers, reference)
		} else {
			record.Dependents = append(record.Dependents, reference)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate hub work item relationships: %w", err)
	}
	return nil
}

func loadWorkItemLeases(ctx context.Context, queryer databaseQueryer, index map[tracker.WorkItemID]*tracker.Record, ids []tracker.WorkItemID) error {
	statement := `
SELECT l.issue_id, l.lease_id, l.fencing_token, l.machine_id, m.hostname, m.display_name, l.session_id, l.acquired_at, l.renewed_at, l.expires_at
FROM leases l
JOIN machines m ON m.id = l.machine_id
WHERE l.released_at IS NULL AND l.issue_id IN (` + placeholders(len(ids)) + `)
ORDER BY l.issue_id`
	rows, err := queryer.QueryContext(ctx, statement, workItemIDArgs(ids)...)
	if err != nil {
		return fmt.Errorf("query hub work item leases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issueID tracker.WorkItemID
		var lease tracker.LeaseSummary
		var acquiredAt string
		var renewedAt string
		var expiresAt string
		if err := rows.Scan(&issueID, &lease.ID, &lease.FencingToken, &lease.Machine.ID, &lease.Machine.Hostname, &lease.Machine.DisplayName, &lease.SessionID, &acquiredAt, &renewedAt, &expiresAt); err != nil {
			return fmt.Errorf("scan hub work item lease: %w", err)
		}
		var err error
		if lease.AcquiredAt, err = parseTimeValue(acquiredAt); err != nil {
			return fmt.Errorf("decode hub lease acquired timestamp: %w", err)
		}
		if lease.RenewedAt, err = parseTimeValue(renewedAt); err != nil {
			return fmt.Errorf("decode hub lease renewed timestamp: %w", err)
		}
		if lease.ExpiresAt, err = parseTimeValue(expiresAt); err != nil {
			return fmt.Errorf("decode hub lease expiry timestamp: %w", err)
		}
		record, err := indexedWorkItem(index, issueID)
		if err != nil {
			return err
		}
		record.Lease = &lease
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate hub work item leases: %w", err)
	}
	return nil
}

func loadWorkItemPullRequests(ctx context.Context, queryer databaseQueryer, index map[tracker.WorkItemID]*tracker.Record, ids []tracker.WorkItemID) error {
	statement := `
SELECT issue_id, id, github_node_id, github_number, title, url, github_state, draft, head_ref, head_sha, base_ref, base_sha, mergeable_state, checks_summary_json, reviews_summary_json, merge_ready, merge_readiness_refreshed_at
FROM pull_requests
WHERE issue_id IN (` + placeholders(len(ids)) + `)
ORDER BY issue_id, github_number`
	rows, err := queryer.QueryContext(ctx, statement, workItemIDArgs(ids)...)
	if err != nil {
		return fmt.Errorf("query hub work item pull requests: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issueID tracker.WorkItemID
		var pullRequest tracker.PullRequestSummary
		var checksJSON string
		var reviewsJSON string
		var refreshedAt sql.NullString
		if err := rows.Scan(&issueID, &pullRequest.ID, &pullRequest.GitHubNodeID, &pullRequest.Number, &pullRequest.Title, &pullRequest.URL, &pullRequest.State, &pullRequest.Draft, &pullRequest.HeadRef, &pullRequest.HeadSHA, &pullRequest.BaseRef, &pullRequest.BaseSHA, &pullRequest.Merge.State, &checksJSON, &reviewsJSON, &pullRequest.Merge.Ready, &refreshedAt); err != nil {
			return fmt.Errorf("scan hub work item pull request: %w", err)
		}
		if err := json.Unmarshal([]byte(checksJSON), &pullRequest.Checks); err != nil {
			return fmt.Errorf("decode hub pull request checks summary: %w", err)
		}
		if err := json.Unmarshal([]byte(reviewsJSON), &pullRequest.Reviews); err != nil {
			return fmt.Errorf("decode hub pull request reviews summary: %w", err)
		}
		if refreshedAt.Valid {
			parsed, err := parseTimeValue(refreshedAt.String)
			if err != nil {
				return fmt.Errorf("decode hub pull request merge timestamp: %w", err)
			}
			pullRequest.Merge.RefreshedAt = &parsed
		}
		record, err := indexedWorkItem(index, issueID)
		if err != nil {
			return err
		}
		record.PullRequests = append(record.PullRequests, pullRequest)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate hub work item pull requests: %w", err)
	}
	return nil
}

func loadWorkItemSyncStatus(ctx context.Context, queryer databaseQueryer, index map[tracker.WorkItemID]*tracker.Record, ids []tracker.WorkItemID) error {
	statement := `
SELECT issue_id, status, attempts, next_retry_at, terminal_error
FROM github_outbox
WHERE issue_id IN (` + placeholders(len(ids)) + `) AND completed_at IS NULL
ORDER BY issue_id, id`
	rows, err := queryer.QueryContext(ctx, statement, workItemIDArgs(ids)...)
	if err != nil {
		return fmt.Errorf("query hub work item sync status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issueID tracker.WorkItemID
		var status string
		var attempts int
		var nextRetryAt sql.NullString
		var terminalError sql.NullString
		if err := rows.Scan(&issueID, &status, &attempts, &nextRetryAt, &terminalError); err != nil {
			return fmt.Errorf("scan hub work item sync status: %w", err)
		}
		record, err := indexedWorkItem(index, issueID)
		if err != nil {
			return err
		}
		candidate := classifySyncStatus(status, attempts, nextRetryAt.Valid, terminalError.String)
		if syncStatusPriority(candidate) > syncStatusPriority(record.SyncStatus) {
			record.SyncStatus = candidate
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate hub work item sync status: %w", err)
	}
	return nil
}

func indexedWorkItem(index map[tracker.WorkItemID]*tracker.Record, id tracker.WorkItemID) (*tracker.Record, error) {
	record, ok := index[id]
	if !ok || record == nil {
		return nil, fmt.Errorf("hub work item enrichment references unknown item %d", id)
	}
	return record, nil
}

func classifySyncStatus(status string, attempts int, retryScheduled bool, terminalError string) tracker.SyncStatus {
	status = strings.ToLower(strings.TrimSpace(status))
	if strings.TrimSpace(terminalError) != "" || status == "error" || status == "failed" || status == "dead_letter" {
		return tracker.SyncStatusError
	}
	if status == "retrying" || attempts > 0 || retryScheduled {
		return tracker.SyncStatusRetrying
	}
	if status == "complete" || status == "completed" || status == "succeeded" || status == "synced" {
		return tracker.SyncStatusSynced
	}
	return tracker.SyncStatusPending
}

func syncStatusPriority(status tracker.SyncStatus) int {
	switch status {
	case tracker.SyncStatusError:
		return 4
	case tracker.SyncStatusStale:
		return 3
	case tracker.SyncStatusRetrying:
		return 2
	case tracker.SyncStatusPending:
		return 1
	default:
		return 0
	}
}

func normalizedWorkItemIDs(ids []tracker.WorkItemID) ([]tracker.WorkItemID, error) {
	result := make([]tracker.WorkItemID, 0, len(ids))
	seen := make(map[tracker.WorkItemID]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: %d", tracker.ErrInvalidWorkItemID, id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

func normalizedRepositoryIDs(ids []tracker.RepositoryID) ([]tracker.RepositoryID, error) {
	result := make([]tracker.RepositoryID, 0, len(ids))
	seen := make(map[tracker.RepositoryID]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: repository ID must be positive: %d", tracker.ErrInvalidCandidateQuery, id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

func normalizedQueryStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func workItemIDArgs(ids []tracker.WorkItemID) []any {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

func parsedTime(value string) (*time.Time, error) {
	parsed, err := parseTimeValue(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
