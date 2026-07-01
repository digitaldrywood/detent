package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	defaultProjectID = "default"

	eventKindComment       = "comment"
	eventKindStateUpdate   = "state_update"
	eventKindFieldUpdate   = "field_update"
	eventKindProjectRemove = "project_remove"
)

type Config struct {
	Path           string
	ProjectID      string
	Issues         []connector.Issue
	ActiveStates   []string
	ObservedStates []string
	TerminalStates []string
	Now            func() time.Time
}

type Connector struct {
	db             *sql.DB
	projectID      string
	activeStates   []string
	observedStates []string
	terminalStates []string
	now            func() time.Time
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.CandidateIssuesByStatesFetcher = (*Connector)(nil)
var _ connector.IssuesByStatesLimiter = (*Connector)(nil)
var _ connector.IssueCommentReader = (*Connector)(nil)
var _ connector.IssueStateProber = (*Connector)(nil)
var _ connector.ProjectRemover = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, errors.New("local sqlite path is required")
	}
	var err error
	path, err = expandPath(path)
	if err != nil {
		return nil, err
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create local sqlite parent: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open local sqlite: %w", err)
	}
	conn := &Connector{
		db:             db,
		projectID:      localProjectID(cfg.ProjectID),
		activeStates:   cloneStrings(cfg.ActiveStates),
		observedStates: cloneStrings(cfg.ObservedStates),
		terminalStates: cloneStrings(cfg.TerminalStates),
		now:            cfg.Now,
	}
	if conn.now == nil {
		conn.now = time.Now
	}
	if err := conn.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := conn.seed(context.Background(), cfg.Issues); err != nil {
		_ = db.Close()
		return nil, err
	}
	return conn, nil
}

func (c *Connector) Name() string {
	return connector.BackendLocalSQLite.String()
}

func (c *Connector) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Connector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	if len(c.activeStates) == 0 {
		return c.fetchIssues(ctx, nil, 0)
	}
	return c.FetchIssuesByStates(ctx, c.activeStates)
}

func (c *Connector) FetchCandidateIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	return c.FetchIssuesByStates(ctx, states)
}

func (c *Connector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	return c.fetchIssues(ctx, states, 0)
}

func (c *Connector) FetchIssuesByStatesLimit(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	return c.fetchIssues(ctx, states, limit)
}

func (c *Connector) FetchIssueStateProbe(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	return c.FetchIssuesByStatesLimit(ctx, states, limit)
}

func (c *Connector) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]connector.Issue, error) {
	ids := normalizedSet(issueIDs)
	if len(ids) == 0 {
		return []connector.Issue{}, nil
	}
	issues, err := c.fetchIssues(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	out := make([]connector.Issue, 0, len(issueIDs))
	for _, issue := range issues {
		if _, ok := ids[strings.TrimSpace(issue.ID)]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (c *Connector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	rows, err := c.db.QueryContext(ctx, `
select body from detent_work_item_events
where project_id = ? and item_id = ? and event_kind = ?
order by id asc`, c.projectID, strings.TrimSpace(issue.ID), eventKindComment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	comments := []connector.IssueComment{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		comments = append(comments, connector.IssueComment{Body: body})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return comments, nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindComment, "", strings.TrimSpace(body), nil)
}

func (c *Connector) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	issueID = strings.TrimSpace(issueID)
	stateName = strings.TrimSpace(stateName)
	now := c.now().UTC()
	result, err := c.db.ExecContext(ctx, `
update detent_work_items
set state = ?, stage_updated_at = ?, updated_at = ?
where project_id = ? and id = ?`, stateName, formatTime(now), formatTime(now), c.projectID, issueID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return c.recordEvent(ctx, issueID, eventKindStateUpdate, stateName, "", nil)
}

func (c *Connector) SetAssignee(ctx context.Context, issueID string, login string) error {
	return c.SetField(ctx, issueID, "assignee", login)
}

func (c *Connector) SetField(ctx context.Context, issueID string, fieldName string, value string) error {
	issue, err := c.issueByID(ctx, issueID)
	if err != nil {
		return err
	}
	if issue.Fields == nil {
		issue.Fields = map[string]string{}
	}
	fieldName = strings.TrimSpace(fieldName)
	issue.Fields[fieldName] = strings.TrimSpace(value)
	fieldsJSON, err := marshalStringMap(issue.Fields)
	if err != nil {
		return err
	}
	now := c.now().UTC()
	_, err = c.db.ExecContext(ctx, `
update detent_work_items
set fields_json = ?, updated_at = ?
where project_id = ? and id = ?`, fieldsJSON, formatTime(now), c.projectID, strings.TrimSpace(issueID))
	if err != nil {
		return err
	}
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindFieldUpdate, "", "", map[string]string{fieldName: value})
}

func (c *Connector) RemoveIssueFromProject(ctx context.Context, issueID string) error {
	issueID = strings.TrimSpace(issueID)
	result, err := c.db.ExecContext(ctx, `
delete from detent_work_items
where project_id = ? and id = ?`, c.projectID, issueID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return c.recordEvent(ctx, issueID, eventKindProjectRemove, "", "", nil)
}

func (c *Connector) migrate(ctx context.Context) error {
	statements := []string{
		`create table if not exists detent_work_items (
project_id text not null,
id text not null,
identifier text not null,
title text not null default '',
description text not null default '',
priority integer,
state text not null default '',
url text not null default '',
author_id text not null default '',
assignee_id text not null default '',
assignees_json text not null default '[]',
labels_json text not null default '[]',
fields_json text not null default '{}',
metadata_json text not null default '{}',
deliverable_kind text not null default '',
deliverable_path text not null default '',
deliverable_review_url text not null default '',
deliverable_validation_status text not null default '',
deliverable_external_id text not null default '',
deliverable_metadata_json text not null default '{}',
assigned_to_worker integer not null default 1,
created_at text not null default '',
updated_at text not null default '',
stage_updated_at text not null default '',
model_override text not null default '',
primary key (project_id, id)
)`,
		`create table if not exists detent_work_item_events (
id integer primary key autoincrement,
project_id text not null,
item_id text not null,
event_kind text not null,
state text not null default '',
body text not null default '',
payload_json text not null default '{}',
created_at text not null
)`,
		`create index if not exists idx_detent_work_items_project_state on detent_work_items(project_id, state)`,
		`create index if not exists idx_detent_work_item_events_project_item on detent_work_item_events(project_id, item_id, id)`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate local sqlite connector: %w", err)
		}
	}
	return nil
}

func (c *Connector) seed(ctx context.Context, issues []connector.Issue) error {
	for _, issue := range issues {
		if err := c.insertSeedIssue(ctx, issue); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) insertSeedIssue(ctx context.Context, issue connector.Issue) error {
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = strings.TrimSpace(issue.Identifier)
	}
	if id == "" {
		return errors.New("local sqlite seed issue id or identifier is required")
	}
	identifier := strings.TrimSpace(issue.Identifier)
	if identifier == "" {
		identifier = id
	}
	now := c.now().UTC()
	createdAt := timeOrDefault(issue.CreatedAt, now)
	updatedAt := timeOrDefault(issue.UpdatedAt, createdAt)
	stageUpdatedAt := timeOrDefault(issue.StageUpdatedAt, updatedAt)
	assigneesJSON, err := marshalStringSlice(issue.Assignees)
	if err != nil {
		return err
	}
	labelsJSON, err := marshalStringSlice(issue.Labels)
	if err != nil {
		return err
	}
	fieldsJSON, err := marshalStringMap(issue.Fields)
	if err != nil {
		return err
	}
	metadataJSON, err := marshalStringMap(issue.Metadata)
	if err != nil {
		return err
	}
	deliverableMetadataJSON := "{}"
	deliverable := connector.Deliverable{}
	if issue.Deliverable != nil {
		deliverable = *issue.Deliverable
		deliverableMetadataJSON, err = marshalStringMap(issue.Deliverable.Metadata)
		if err != nil {
			return err
		}
	}
	assigned := 0
	if issue.AssignedToWorker {
		assigned = 1
	}
	_, err = c.db.ExecContext(ctx, `
insert into detent_work_items (
project_id, id, identifier, title, description, priority, state, url,
author_id, assignee_id, assignees_json, labels_json, fields_json, metadata_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, assigned_to_worker,
created_at, updated_at, stage_updated_at, model_override
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(project_id, id) do nothing`,
		c.projectID, id, identifier, issue.Title, issue.Description, nullableInt(issue.Priority), issue.State, issue.URL,
		issue.AuthorID, issue.AssigneeID, assigneesJSON, labelsJSON, fieldsJSON, metadataJSON,
		deliverable.Kind, deliverable.Path, deliverable.ReviewURL, deliverable.ValidationStatus,
		deliverable.ExternalID, deliverableMetadataJSON, assigned,
		formatTime(createdAt), formatTime(updatedAt), formatTime(stageUpdatedAt), issue.ModelOverride)
	return err
}

func (c *Connector) fetchIssues(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	query := `select project_id, id, identifier, title, description, priority, state, url,
author_id, assignee_id, assignees_json, labels_json, fields_json, metadata_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, assigned_to_worker,
created_at, updated_at, stage_updated_at, model_override
from detent_work_items
where project_id = ?`
	args := []any{c.projectID}
	if len(normalizedSet(states)) > 0 {
		placeholders := make([]string, 0, len(states))
		for _, state := range states {
			state = strings.TrimSpace(state)
			if state == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, state)
		}
		query += " and state in (" + strings.Join(placeholders, ",") + ")"
	}
	query += " order by updated_at desc, id asc"
	if limit > 0 {
		query += " limit ?"
		args = append(args, limit)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	issues := []connector.Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		comments, err := c.FetchIssueComments(ctx, issue)
		if err != nil {
			return nil, err
		}
		issue.Comments = comments
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return issues, nil
}

func (c *Connector) issueByID(ctx context.Context, issueID string) (connector.Issue, error) {
	issues, err := c.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return connector.Issue{}, err
	}
	if len(issues) == 0 {
		return connector.Issue{}, sql.ErrNoRows
	}
	return issues[0], nil
}

func (c *Connector) recordEvent(ctx context.Context, issueID string, kind string, state string, body string, payload map[string]string) error {
	payloadJSON, err := marshalStringMap(payload)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `
insert into detent_work_item_events(project_id, item_id, event_kind, state, body, payload_json, created_at)
values (?, ?, ?, ?, ?, ?, ?)`,
		c.projectID, issueID, kind, strings.TrimSpace(state), strings.TrimSpace(body), payloadJSON, formatTime(c.now().UTC()))
	return err
}

type issueScanner interface {
	Scan(dest ...any) error
}

func scanIssue(scanner issueScanner) (connector.Issue, error) {
	var issue connector.Issue
	var projectID string
	var priority sql.NullInt64
	var assigneesJSON, labelsJSON, fieldsJSON, metadataJSON string
	var deliverable connector.Deliverable
	var deliverableMetadataJSON string
	var assigned int
	var createdAt, updatedAt, stageUpdatedAt string
	err := scanner.Scan(
		&projectID, &issue.ID, &issue.Identifier, &issue.Title, &issue.Description, &priority, &issue.State, &issue.URL,
		&issue.AuthorID, &issue.AssigneeID, &assigneesJSON, &labelsJSON, &fieldsJSON, &metadataJSON,
		&deliverable.Kind, &deliverable.Path, &deliverable.ReviewURL, &deliverable.ValidationStatus,
		&deliverable.ExternalID, &deliverableMetadataJSON, &assigned,
		&createdAt, &updatedAt, &stageUpdatedAt, &issue.ModelOverride,
	)
	if err != nil {
		return connector.Issue{}, err
	}
	if priority.Valid {
		value := int(priority.Int64)
		issue.Priority = &value
	}
	issue.Assignees = unmarshalStringSlice(assigneesJSON)
	issue.Labels = unmarshalStringSlice(labelsJSON)
	issue.Fields = unmarshalStringMap(fieldsJSON)
	issue.Metadata = unmarshalStringMap(metadataJSON)
	issue.AssignedToWorker = assigned != 0
	issue.CreatedAt = parseTimePointer(createdAt)
	issue.UpdatedAt = parseTimePointer(updatedAt)
	issue.StageUpdatedAt = parseTimePointer(stageUpdatedAt)
	deliverable.Metadata = unmarshalStringMap(deliverableMetadataJSON)
	if deliverableHasContent(deliverable) {
		issue.Deliverable = &deliverable
	}
	return issue, nil
}

func deliverableHasContent(deliverable connector.Deliverable) bool {
	return strings.TrimSpace(deliverable.Kind) != "" ||
		strings.TrimSpace(deliverable.Path) != "" ||
		strings.TrimSpace(deliverable.ReviewURL) != "" ||
		strings.TrimSpace(deliverable.ValidationStatus) != "" ||
		strings.TrimSpace(deliverable.ExternalID) != "" ||
		len(deliverable.Metadata) > 0
}

func localProjectID(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return defaultProjectID
	}
	return projectID
}

func normalizedSet(values []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

func expandPath(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func timeOrDefault(value *time.Time, fallback time.Time) time.Time {
	if value == nil || value.IsZero() {
		return fallback
	}
	return value.UTC()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTimePointer(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func marshalStringSlice(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalStringMap(values map[string]string) (string, error) {
	if values == nil {
		values = map[string]string{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalStringSlice(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &values); err != nil {
		return []string{}
	}
	if values == nil {
		return []string{}
	}
	return values
}

func unmarshalStringMap(raw string) map[string]string {
	var values map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &values); err != nil {
		return map[string]string{}
	}
	if values == nil {
		return map[string]string{}
	}
	return values
}
