package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	defaultProjectID        = "default"
	sqliteBusyTimeoutMillis = 5000

	eventKindComment       = "comment"
	eventKindCommentEdit   = "comment_edit"
	eventKindCommentDelete = "comment_delete"
	eventKindStateUpdate   = "state_update"
	eventKindFieldUpdate   = "field_update"
	eventKindProjectRemove = "project_remove"
	eventKindClose         = "close"

	commentPayloadActor        = "actor"
	commentPayloadCommentID    = "comment_id"
	commentPayloadPreviousBody = "previous_body"

	MetadataGitHubNodeID        = "github_node_id"
	MetadataGitHubRepositoryID  = "github_repository_id"
	MetadataGitHubIssueNumber   = "github_issue_number"
	MetadataGitHubUpstreamState = "github_upstream_state"
	MetadataGitHubOrphaned      = "github_orphaned"
)

type Config struct {
	Path           string
	ProjectID      string
	Issues         []connector.Issue
	ActiveStates   []string
	ObservedStates []string
	TerminalStates []string
	Now            func() time.Time
	Logger         *slog.Logger
}

type Connector struct {
	db             *sql.DB
	projectID      string
	activeStates   []string
	observedStates []string
	terminalStates []string
	now            func() time.Time
	logger         *slog.Logger
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.CandidateIssuesByStatesFetcher = (*Connector)(nil)
var _ connector.IssuesByStatesLimiter = (*Connector)(nil)
var _ connector.IssueCloser = (*Connector)(nil)
var _ connector.IssueCommentDeleter = (*Connector)(nil)
var _ connector.IssueCommentReader = (*Connector)(nil)
var _ connector.IssueCommentUpdater = (*Connector)(nil)
var _ connector.IssueEventReader = (*Connector)(nil)
var _ connector.IssueFieldClearer = (*Connector)(nil)
var _ connector.IssueFieldSetter = (*Connector)(nil)
var _ connector.IssueReferenceResolver = (*Connector)(nil)
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
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open local sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", sqliteBusyTimeoutMillis)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure local sqlite busy timeout: %w", err)
	}
	conn := &Connector{
		db:             db,
		projectID:      localProjectID(cfg.ProjectID),
		activeStates:   cloneStrings(cfg.ActiveStates),
		observedStates: cloneStrings(cfg.ObservedStates),
		terminalStates: cloneStrings(cfg.TerminalStates),
		now:            cfg.Now,
		logger:         cfg.Logger,
	}
	if conn.now == nil {
		conn.now = time.Now
	}
	if conn.logger == nil {
		conn.logger = slog.Default()
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

// sqliteDSN opens the tracker database with a busy timeout and WAL enabled on
// every pooled connection. The database is shared with a running detent
// server and with operators editing it via the sqlite3 shell, so writes must
// wait for the lock instead of failing immediately with SQLITE_BUSY. The
// pragmas ride the DSN because ExecContext PRAGMAs would only configure the
// single pool connection that happened to run them.
func sqliteDSN(path string) string {
	if path == ":memory:" {
		return path
	}
	return "file:" + escapeSQLiteURIPath(sqliteURIPath(path)) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func sqliteURIPath(path string) string {
	cleaned := filepath.Clean(path)
	uriPath := filepath.ToSlash(cleaned)
	if windowsDrivePath(uriPath) {
		uriPath = strings.ReplaceAll(cleaned, `\`, "/")
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
	}
	return uriPath
}

func windowsDrivePath(path string) bool {
	return len(path) >= 2 && path[1] == ':' &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z')
}

func escapeSQLiteURIPath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
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

func (c *Connector) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) ([]connector.Issue, error) {
	wanted := normalizedIdentifierSet(identifiers)
	if len(wanted) == 0 && len(normalizedNumberSet(identifiers, nil)) == 0 {
		return []connector.Issue{}, nil
	}
	issues, err := c.fetchIssues(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	out := make([]connector.Issue, 0, len(identifiers))
	seen := map[string]struct{}{}
	matchedIdentifiers := map[string]struct{}{}
	for _, issue := range issues {
		key := strings.TrimSpace(issue.ID)
		identifier := strings.ToLower(strings.TrimSpace(issue.Identifier))
		if _, ok := wanted[identifier]; ok {
			out = append(out, issue)
			seen[key] = struct{}{}
			matchedIdentifiers[identifier] = struct{}{}
		}
	}
	wantedNumbers := normalizedNumberSet(identifiers, matchedIdentifiers)
	if len(wantedNumbers) == 0 {
		return out, nil
	}
	for _, issue := range issues {
		key := strings.TrimSpace(issue.ID)
		if issue.Number <= 0 {
			continue
		}
		if _, ok := wantedNumbers[issue.Number]; !ok {
			continue
		}
		if _, ok := seen[key]; ok && key != "" {
			continue
		}
		out = append(out, issue)
		seen[key] = struct{}{}
	}
	return out, nil
}

func (c *Connector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	rows, err := c.db.QueryContext(ctx, `
select id, event_kind, body, payload_json, created_at from detent_work_item_events
where project_id = ? and item_id = ? and event_kind in (?, ?, ?)
order by id asc`, c.projectID, strings.TrimSpace(issue.ID), eventKindComment, eventKindCommentEdit, eventKindCommentDelete)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	commentsByID := map[string]connector.IssueComment{}
	order := []string{}
	for rows.Next() {
		var id int64
		var kind string
		var body string
		var payloadJSON string
		var createdAt string
		if err := rows.Scan(&id, &kind, &body, &payloadJSON, &createdAt); err != nil {
			return nil, err
		}
		eventTime := parseTimePointer(createdAt)
		switch kind {
		case eventKindComment:
			commentID := strconv.FormatInt(id, 10)
			commentsByID[commentID] = connector.IssueComment{
				ID:                commentID,
				Backend:           connector.BackendLocalSQLite.String(),
				Body:              body,
				AuthorLogin:       "detent",
				AuthorDisplayName: "Detent",
				CreatedAt:         eventTime,
				Local:             true,
				CanEdit:           true,
				CanDelete:         true,
				TargetType:        connector.IssueCommentTargetIssue,
			}
			order = append(order, commentID)
		case eventKindCommentEdit:
			payload := unmarshalStringMap(payloadJSON)
			commentID := strings.TrimSpace(payload[commentPayloadCommentID])
			comment, ok := commentsByID[commentID]
			if !ok {
				continue
			}
			comment.Body = body
			comment.UpdatedAt = eventTime
			commentsByID[commentID] = comment
		case eventKindCommentDelete:
			payload := unmarshalStringMap(payloadJSON)
			commentID := strings.TrimSpace(payload[commentPayloadCommentID])
			delete(commentsByID, commentID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	comments := []connector.IssueComment{}
	for _, commentID := range order {
		comment, ok := commentsByID[commentID]
		if ok {
			comments = append(comments, comment)
		}
	}
	return comments, nil
}

func (c *Connector) FetchIssueEvents(ctx context.Context, issue connector.Issue) ([]connector.IssueEvent, error) {
	rows, err := c.db.QueryContext(ctx, `
select id, event_kind, state, body, payload_json, created_at from detent_work_item_events
where project_id = ? and item_id = ?
order by id asc`, c.projectID, strings.TrimSpace(issue.ID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []connector.IssueEvent{}
	for rows.Next() {
		var id int64
		var event connector.IssueEvent
		var payloadJSON string
		var createdAt string
		if err := rows.Scan(&id, &event.Kind, &event.State, &event.Body, &payloadJSON, &createdAt); err != nil {
			return nil, err
		}
		event.ID = strconv.FormatInt(id, 10)
		event.Fields = unmarshalStringMap(payloadJSON)
		event.CreatedAt = parseTimePointer(createdAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindComment, "", strings.TrimSpace(body), nil)
}

func (c *Connector) localIssueCommentByID(ctx context.Context, issueID string, commentID string) (connector.IssueComment, error) {
	issueID = strings.TrimSpace(issueID)
	commentID = strings.TrimSpace(commentID)
	if issueID == "" || commentID == "" {
		return connector.IssueComment{}, sql.ErrNoRows
	}
	comments, err := c.FetchIssueComments(ctx, connector.Issue{ID: issueID})
	if err != nil {
		return connector.IssueComment{}, err
	}
	for _, comment := range comments {
		if strings.TrimSpace(comment.ID) == commentID {
			return comment, nil
		}
	}
	return connector.IssueComment{}, sql.ErrNoRows
}

func (c *Connector) UpdateIssueComment(ctx context.Context, issueID string, commentID string, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("local comment body is required")
	}
	comment, err := c.localIssueCommentByID(ctx, issueID, commentID)
	if err != nil {
		return err
	}
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindCommentEdit, "", body, map[string]string{
		commentPayloadActor:        "detent",
		commentPayloadCommentID:    comment.ID,
		commentPayloadPreviousBody: comment.Body,
	})
}

func (c *Connector) DeleteIssueComment(ctx context.Context, issueID string, commentID string) error {
	comment, err := c.localIssueCommentByID(ctx, issueID, commentID)
	if err != nil {
		return err
	}
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindCommentDelete, "", "", map[string]string{
		commentPayloadActor:        "detent",
		commentPayloadCommentID:    comment.ID,
		commentPayloadPreviousBody: comment.Body,
	})
}

func (c *Connector) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	issueID = strings.TrimSpace(issueID)
	stateName = c.canonicalState(stateName)
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

func (c *Connector) SetIssueField(ctx context.Context, issueID string, fieldID int, value string) error {
	if fieldID <= 0 {
		return connector.ErrNotImplemented
	}
	return c.SetField(ctx, issueID, issueFieldKey(fieldID), value)
}

func (c *Connector) ClearIssueField(ctx context.Context, issueID string, fieldID int) error {
	if fieldID <= 0 {
		return connector.ErrNotImplemented
	}
	return c.clearField(ctx, issueID, issueFieldKey(fieldID))
}

func (c *Connector) CloseIssue(ctx context.Context, issueID string) error {
	state := c.closedState()
	issueID = strings.TrimSpace(issueID)
	now := c.now().UTC()
	result, err := c.db.ExecContext(ctx, `
update detent_work_items
set state = ?, stage_updated_at = ?, updated_at = ?
where project_id = ? and id = ?`, state, formatTime(now), formatTime(now), c.projectID, issueID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return c.recordEvent(ctx, issueID, eventKindClose, state, "", nil)
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
state text not null default '' collate nocase,
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
github_node_id text not null default '',
github_repository_id integer not null default 0,
github_issue_number integer not null default 0,
github_upstream_state text not null default '',
github_orphaned integer not null default 0,
number integer not null default 0,
primary key (project_id, id)
)`,
		`create table if not exists detent_work_item_counters (
project_id text primary key,
next_number integer not null
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
	if err := c.addMissingWorkItemColumns(ctx); err != nil {
		return fmt.Errorf("migrate local sqlite connector: %w", err)
	}
	if err := c.backfillWorkItemNumbers(ctx); err != nil {
		return fmt.Errorf("migrate local sqlite connector: %w", err)
	}
	if _, err := c.db.ExecContext(ctx, `create unique index if not exists idx_detent_work_items_project_number on detent_work_items(project_id, number) where number > 0`); err != nil {
		return fmt.Errorf("migrate local sqlite connector: %w", err)
	}
	if err := c.syncWorkItemCounters(ctx); err != nil {
		return fmt.Errorf("migrate local sqlite connector: %w", err)
	}
	return nil
}

func (c *Connector) addMissingWorkItemColumns(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `pragma table_info(detent_work_items)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "number", definition: "integer not null default 0"},
		{name: "github_node_id", definition: "text not null default ''"},
		{name: "github_repository_id", definition: "integer not null default 0"},
		{name: "github_issue_number", definition: "integer not null default 0"},
		{name: "github_upstream_state", definition: "text not null default ''"},
		{name: "github_orphaned", definition: "integer not null default 0"},
	}
	for _, column := range columns {
		if _, ok := existing[column.name]; ok {
			continue
		}
		if _, err := c.db.ExecContext(ctx, "alter table detent_work_items add column "+column.name+" "+column.definition); err != nil {
			return err
		}
	}
	return nil
}

type workItemNumberBackfillRow struct {
	projectID string
	id        string
}

func (c *Connector) backfillWorkItemNumbers(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(ctx, `
select project_id, id
from detent_work_items
where number <= 0
order by project_id asc, case when created_at = '' then 1 else 0 end asc, created_at asc, id asc`)
	if err != nil {
		return err
	}
	defer rows.Close()
	pending := []workItemNumberBackfillRow{}
	for rows.Next() {
		var row workItemNumberBackfillRow
		if err := rows.Scan(&row.projectID, &row.id); err != nil {
			closeErr := rows.Close()
			return errors.Join(err, closeErr)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		closeErr := rows.Close()
		return errors.Join(err, closeErr)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	nextByProject := map[string]int64{}
	for _, row := range pending {
		next, ok := nextByProject[row.projectID]
		if !ok {
			if err := tx.QueryRowContext(ctx, `
select coalesce(max(number), 0) + 1
from detent_work_items
where project_id = ? and number > 0`, row.projectID).Scan(&next); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
update detent_work_items
set number = ?
where project_id = ? and id = ?`, next, row.projectID, row.id); err != nil {
			return err
		}
		nextByProject[row.projectID] = next + 1
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (c *Connector) syncWorkItemCounters(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
insert into detent_work_item_counters(project_id, next_number)
select project_id, coalesce(max(number), 0) + 1
from detent_work_items
group by project_id
on conflict(project_id) do update set
next_number = max(detent_work_item_counters.next_number, excluded.next_number)`)
	return err
}

func (c *Connector) workItemNumberForUpsert(ctx context.Context, tx *sql.Tx, id string, requested int) (int64, error) {
	var existing int64
	err := tx.QueryRowContext(ctx, `
select number
from detent_work_items
where project_id = ? and id = ?`, c.projectID, id).Scan(&existing)
	if err == nil && existing > 0 {
		return existing, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if requested > 0 {
		number := int64(requested)
		if err := c.ensureWorkItemCounterAtLeast(ctx, tx, number+1); err != nil {
			return 0, err
		}
		return number, nil
	}
	return c.nextWorkItemNumber(ctx, tx)
}

func (c *Connector) nextWorkItemNumber(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
insert into detent_work_item_counters(project_id, next_number)
values (?, 1)
on conflict(project_id) do nothing`, c.projectID); err != nil {
		return 0, err
	}
	var number int64
	if err := tx.QueryRowContext(ctx, `
update detent_work_item_counters
set next_number = next_number + 1
where project_id = ?
returning next_number - 1`, c.projectID).Scan(&number); err != nil {
		return 0, err
	}
	return number, nil
}

func (c *Connector) ensureWorkItemCounterAtLeast(ctx context.Context, tx *sql.Tx, next int64) error {
	_, err := tx.ExecContext(ctx, `
insert into detent_work_item_counters(project_id, next_number)
values (?, ?)
on conflict(project_id) do update set
next_number = max(detent_work_item_counters.next_number, excluded.next_number)`, c.projectID, next)
	return err
}

func (c *Connector) seed(ctx context.Context, issues []connector.Issue) error {
	for _, issue := range issues {
		if err := c.insertSeedIssue(ctx, issue); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) UpsertIssues(ctx context.Context, issues []connector.Issue) error {
	for _, issue := range issues {
		if err := c.upsertIssue(ctx, issue); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) upsertIssue(ctx context.Context, issue connector.Issue) error {
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = strings.TrimSpace(issue.Identifier)
	}
	if id == "" {
		return errors.New("local sqlite upsert issue id or identifier is required")
	}
	identifier := strings.TrimSpace(issue.Identifier)
	if identifier == "" {
		identifier = id
	}
	issue.State = c.canonicalState(issue.State)
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
	githubIdentity := githubIdentityFromIssue(issue)
	assigned := 0
	if issue.AssignedToWorker {
		assigned = 1
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	number, err := c.workItemNumberForUpsert(ctx, tx, id, issue.Number)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
insert into detent_work_items (
project_id, id, identifier, number, title, description, priority, state, url,
author_id, assignee_id, assignees_json, labels_json, fields_json, metadata_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, assigned_to_worker,
created_at, updated_at, stage_updated_at, model_override,
github_node_id, github_repository_id, github_issue_number, github_upstream_state, github_orphaned
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(project_id, id) do update set
identifier = excluded.identifier,
number = case when detent_work_items.number > 0 then detent_work_items.number else excluded.number end,
title = excluded.title,
description = excluded.description,
state = case when excluded.state <> '' then excluded.state else detent_work_items.state end,
stage_updated_at = case when excluded.state <> '' and excluded.state <> detent_work_items.state then excluded.stage_updated_at else detent_work_items.stage_updated_at end,
url = excluded.url,
author_id = excluded.author_id,
assignee_id = excluded.assignee_id,
assignees_json = excluded.assignees_json,
labels_json = excluded.labels_json,
metadata_json = excluded.metadata_json,
assigned_to_worker = excluded.assigned_to_worker,
updated_at = excluded.updated_at,
model_override = excluded.model_override,
github_node_id = excluded.github_node_id,
github_repository_id = excluded.github_repository_id,
github_issue_number = excluded.github_issue_number,
github_upstream_state = excluded.github_upstream_state,
github_orphaned = excluded.github_orphaned`,
		c.projectID, id, identifier, number, issue.Title, issue.Description, nullableInt(issue.Priority), issue.State, issue.URL,
		issue.AuthorID, issue.AssigneeID, assigneesJSON, labelsJSON, fieldsJSON, metadataJSON,
		deliverable.Kind, deliverable.Path, deliverable.ReviewURL, deliverable.ValidationStatus,
		deliverable.ExternalID, deliverableMetadataJSON, assigned,
		formatTime(createdAt), formatTime(updatedAt), formatTime(stageUpdatedAt), issue.ModelOverride,
		githubIdentity.NodeID, githubIdentity.RepositoryID, githubIdentity.IssueNumber, githubIdentity.UpstreamState, boolInt(githubIdentity.Orphaned))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
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
	issue.State = c.canonicalState(issue.State)
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
	githubIdentity := githubIdentityFromIssue(issue)
	assigned := 0
	if issue.AssignedToWorker {
		assigned = 1
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	number, err := c.workItemNumberForUpsert(ctx, tx, id, issue.Number)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
insert into detent_work_items (
project_id, id, identifier, number, title, description, priority, state, url,
author_id, assignee_id, assignees_json, labels_json, fields_json, metadata_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, assigned_to_worker,
created_at, updated_at, stage_updated_at, model_override,
github_node_id, github_repository_id, github_issue_number, github_upstream_state, github_orphaned
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(project_id, id) do nothing`,
		c.projectID, id, identifier, number, issue.Title, issue.Description, nullableInt(issue.Priority), issue.State, issue.URL,
		issue.AuthorID, issue.AssigneeID, assigneesJSON, labelsJSON, fieldsJSON, metadataJSON,
		deliverable.Kind, deliverable.Path, deliverable.ReviewURL, deliverable.ValidationStatus,
		deliverable.ExternalID, deliverableMetadataJSON, assigned,
		formatTime(createdAt), formatTime(updatedAt), formatTime(stageUpdatedAt), issue.ModelOverride,
		githubIdentity.NodeID, githubIdentity.RepositoryID, githubIdentity.IssueNumber, githubIdentity.UpstreamState, boolInt(githubIdentity.Orphaned))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (c *Connector) fetchIssues(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	query := `select project_id, id, identifier, number, title, description, priority, state, url,
author_id, assignee_id, assignees_json, labels_json, fields_json, metadata_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, assigned_to_worker,
created_at, updated_at, stage_updated_at, model_override,
github_node_id, github_repository_id, github_issue_number, github_upstream_state, github_orphaned
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
		// Callers such as the orchestrator lowercase configured states while
		// rows may hold the template's capitalized spellings; compare
		// case-insensitively so items stay visible either way.
		query += " and state collate nocase in (" + strings.Join(placeholders, ",") + ")"
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
		issue, err := c.scanIssue(rows)
		if err != nil {
			closeErr := rows.Close()
			return nil, errors.Join(err, closeErr)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		closeErr := rows.Close()
		return nil, errors.Join(err, closeErr)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range issues {
		comments, err := c.FetchIssueComments(ctx, issues[i])
		if err != nil {
			return nil, err
		}
		issues[i].Comments = comments
		fieldUpdatedAt, err := c.fetchFieldUpdatedAt(ctx, issues[i].ID)
		if err != nil {
			return nil, err
		}
		issues[i].FieldUpdatedAt = fieldUpdatedAt
	}
	return issues, nil
}

func (c *Connector) fetchFieldUpdatedAt(ctx context.Context, issueID string) (map[string]time.Time, error) {
	rows, err := c.db.QueryContext(ctx, `
select payload_json, created_at from detent_work_item_events
where project_id = ? and item_id = ? and event_kind = ?
order by id asc`, c.projectID, strings.TrimSpace(issueID), eventKindFieldUpdate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	updatedAt := map[string]time.Time{}
	for rows.Next() {
		var payloadJSON string
		var createdAt string
		if err := rows.Scan(&payloadJSON, &createdAt); err != nil {
			return nil, err
		}
		timestamp := parseTimePointer(createdAt)
		if timestamp == nil || timestamp.IsZero() {
			continue
		}
		for field := range unmarshalStringMap(payloadJSON) {
			field = strings.TrimSpace(field)
			if field != "" {
				updatedAt[field] = *timestamp
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return updatedAt, nil
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

func (c *Connector) clearField(ctx context.Context, issueID string, fieldName string) error {
	issue, err := c.issueByID(ctx, issueID)
	if err != nil {
		return err
	}
	fieldName = strings.TrimSpace(fieldName)
	if fieldName == "" {
		return nil
	}
	delete(issue.Fields, fieldName)
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
	return c.recordEvent(ctx, strings.TrimSpace(issueID), eventKindFieldUpdate, "", "", map[string]string{fieldName: ""})
}

// canonicalState maps a state name to its configured spelling. The
// orchestrator lowercases configured states before writing them back while
// templates and humans use capitalized spellings; without this the state
// column accumulates both cases.
func (c *Connector) canonicalState(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return name
	}
	for _, states := range [][]string{c.activeStates, c.observedStates, c.terminalStates} {
		for _, state := range states {
			if state = strings.TrimSpace(state); strings.EqualFold(state, name) {
				return state
			}
		}
	}
	return name
}

func (c *Connector) closedState() string {
	for _, state := range c.terminalStates {
		if strings.EqualFold(strings.TrimSpace(state), "Done") {
			return strings.TrimSpace(state)
		}
	}
	for _, state := range c.terminalStates {
		if state = strings.TrimSpace(state); state != "" {
			return state
		}
	}
	return "Done"
}

type issueScanner interface {
	Scan(dest ...any) error
}

func (c *Connector) scanIssue(scanner issueScanner) (connector.Issue, error) {
	var issue connector.Issue
	var projectID string
	var priority sql.NullInt64
	var assigneesJSON, labelsJSON, fieldsJSON, metadataJSON string
	var deliverable connector.Deliverable
	var deliverableMetadataJSON string
	var assigned int
	var createdAt, updatedAt, stageUpdatedAt string
	var githubNodeID, githubUpstreamState string
	var githubRepositoryID, githubIssueNumber int64
	var githubOrphaned int
	err := scanner.Scan(
		&projectID, &issue.ID, &issue.Identifier, &issue.Number, &issue.Title, &issue.Description, &priority, &issue.State, &issue.URL,
		&issue.AuthorID, &issue.AssigneeID, &assigneesJSON, &labelsJSON, &fieldsJSON, &metadataJSON,
		&deliverable.Kind, &deliverable.Path, &deliverable.ReviewURL, &deliverable.ValidationStatus,
		&deliverable.ExternalID, &deliverableMetadataJSON, &assigned,
		&createdAt, &updatedAt, &stageUpdatedAt, &issue.ModelOverride,
		&githubNodeID, &githubRepositoryID, &githubIssueNumber, &githubUpstreamState, &githubOrphaned,
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
	issue.Fields = map[string]string{}
	fields, warnings, fieldsErr := unmarshalIssueFields(fieldsJSON)
	if fieldsErr != nil {
		c.logger.Warn("decode local sqlite work item fields failed",
			"project_id", projectID,
			"issue_id", issue.ID,
			"error", fieldsErr,
		)
	} else {
		issue.Fields = fields
		for _, warning := range warnings {
			c.logger.Warn("local sqlite work item field has non-string JSON value",
				"project_id", projectID,
				"issue_id", issue.ID,
				"field", warning.field,
				"json_type", warning.jsonType,
				"action", warning.action,
			)
		}
	}
	issue.Metadata = unmarshalStringMap(metadataJSON)
	applyGitHubIdentityMetadata(&issue, githubIdentity{
		NodeID:        githubNodeID,
		RepositoryID:  githubRepositoryID,
		IssueNumber:   githubIssueNumber,
		UpstreamState: githubUpstreamState,
		Orphaned:      githubOrphaned != 0,
	})
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

func normalizedIdentifierSet(values []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func normalizedNumberSet(values []string, skip map[string]struct{}) map[int]struct{} {
	out := map[int]struct{}{}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, ok := skip[normalized]; ok {
			continue
		}
		number, ok := parseLocalIssueNumber(value)
		if ok {
			out[number] = struct{}{}
		}
	}
	return out
}

func parseLocalIssueNumber(value string) (int, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "#"))
	if value == "" {
		return 0, false
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func issueFieldKey(fieldID int) string {
	return "issue_field:" + strconv.Itoa(fieldID)
}

type githubIdentity struct {
	NodeID        string
	RepositoryID  int64
	IssueNumber   int64
	UpstreamState string
	Orphaned      bool
}

func githubIdentityFromIssue(issue connector.Issue) githubIdentity {
	metadata := issue.Metadata
	return githubIdentity{
		NodeID:        strings.TrimSpace(metadata[MetadataGitHubNodeID]),
		RepositoryID:  int64Metadata(metadata, MetadataGitHubRepositoryID),
		IssueNumber:   int64Metadata(metadata, MetadataGitHubIssueNumber),
		UpstreamState: strings.TrimSpace(metadata[MetadataGitHubUpstreamState]),
		Orphaned:      boolMetadata(metadata, MetadataGitHubOrphaned),
	}
}

func applyGitHubIdentityMetadata(issue *connector.Issue, identity githubIdentity) {
	if issue.Metadata == nil {
		issue.Metadata = map[string]string{}
	}
	if identity.NodeID != "" {
		issue.Metadata[MetadataGitHubNodeID] = identity.NodeID
	}
	if identity.RepositoryID != 0 {
		issue.Metadata[MetadataGitHubRepositoryID] = strconv.FormatInt(identity.RepositoryID, 10)
	}
	if identity.IssueNumber != 0 {
		issue.Metadata[MetadataGitHubIssueNumber] = strconv.FormatInt(identity.IssueNumber, 10)
	}
	if identity.UpstreamState != "" {
		issue.Metadata[MetadataGitHubUpstreamState] = identity.UpstreamState
	}
	if identity.Orphaned {
		issue.Metadata[MetadataGitHubOrphaned] = "true"
	}
}

func int64Metadata(metadata map[string]string, key string) int64 {
	value := strings.TrimSpace(metadata[key])
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func boolMetadata(metadata map[string]string, key string) bool {
	switch strings.ToLower(strings.TrimSpace(metadata[key])) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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

// parseTimeLayouts tolerates the timestamp shapes external producers write
// into the tracker database (the sqlite shell's CURRENT_TIMESTAMP and
// datetime() emit space-separated UTC values), not just detent's own
// RFC3339Nano output.
var parseTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTimePointer(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, layout := range parseTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	return nil
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

type fieldHydrationWarning struct {
	field    string
	jsonType string
	action   string
}

func unmarshalIssueFields(raw string) (map[string]string, []fieldHydrationWarning, error) {
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &encoded); err != nil {
		return map[string]string{}, nil, err
	}

	fields := make(map[string]string, len(encoded))
	warnings := make([]fieldHydrationWarning, 0)
	for field, encodedValue := range encoded {
		value, jsonType, ok := issueFieldString(encodedValue)
		if jsonType != "string" {
			action := "skipped"
			if ok {
				action = "stringified"
			}
			warnings = append(warnings, fieldHydrationWarning{
				field:    field,
				jsonType: jsonType,
				action:   action,
			})
		}
		if ok {
			fields[field] = value
		}
	}
	return fields, warnings, nil
}

func issueFieldString(raw json.RawMessage) (string, string, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", "invalid", false
	}

	switch value[0] {
	case '"':
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", "invalid", false
		}
		return decoded, "string", true
	case 't', 'f':
		var decoded bool
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", "invalid", false
		}
		return strconv.FormatBool(decoded), "boolean", true
	case 'n':
		return "null", "null", true
	case '[':
		return "", "array", false
	case '{':
		return "", "object", false
	default:
		var decoded json.Number
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", "invalid", false
		}
		return decoded.String(), "number", true
	}
}
