package local

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorPersistsWorkItemStateAndEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "work-items.db")
	issue := connector.NewIssue()
	issue.ID = "ad-1"
	issue.Identifier = "store/ad-1"
	issue.Title = "Produce summer sale ad"
	issue.Description = "Create a short video ad from approved assets."
	issue.State = "Todo"
	issue.Fields = map[string]string{"validation_status": "pending"}
	issue.Metadata = map[string]string{"store": "creswood"}
	issue.Deliverable = &connector.Deliverable{
		Kind:             "video_ad",
		Path:             "outputs/ad-1/manifest.json",
		ValidationStatus: "pending",
		ExternalID:       "creative-101",
		Metadata:         map[string]string{"aspect_ratio": "9:16"},
	}

	store, err := New(Config{
		Path:         path,
		ProjectID:    "video",
		Issues:       []connector.Issue{issue},
		ActiveStates: []string{"Todo"},
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	candidates, err := store.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(candidates))
	}
	got := candidates[0]
	if got.ID != "ad-1" || got.State != "Todo" || got.Fields["validation_status"] != "pending" || got.Metadata["store"] != "creswood" {
		t.Fatalf("candidate = %#v", got)
	}
	if got.Number != 1 {
		t.Fatalf("Number = %d, want 1", got.Number)
	}
	if got.Deliverable == nil || got.Deliverable.ExternalID != "creative-101" || got.Deliverable.Metadata["aspect_ratio"] != "9:16" {
		t.Fatalf("candidate deliverable = %#v", got.Deliverable)
	}

	if err := store.UpdateIssueState(ctx, "ad-1", "Review"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := store.SetField(ctx, "ad-1", "validation_status", "valid"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}
	if err := store.CreateComment(ctx, "ad-1", "Manifest is ready for external pickup."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := New(Config{
		Path:      path,
		ProjectID: "video",
	})
	if err != nil {
		t.Fatalf("reopen New() error = %v", err)
	}
	defer reopened.Close()

	issues, err := reopened.FetchIssuesByStates(ctx, []string{"Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(issues))
	}
	got = issues[0]
	if got.State != "Review" || got.Fields["validation_status"] != "valid" {
		t.Fatalf("persisted issue = %#v", got)
	}
	if got.Number != 1 {
		t.Fatalf("persisted Number = %d, want 1", got.Number)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "Manifest is ready for external pickup." {
		t.Fatalf("persisted comments = %#v", got.Comments)
	}

	if err := reopened.RemoveIssueFromProject(ctx, "ad-1"); err != nil {
		t.Fatalf("RemoveIssueFromProject() error = %v", err)
	}
	if _, err := reopened.issueByID(ctx, "ad-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("issueByID() error = %v, want sql.ErrNoRows", err)
	}
}

func TestConnectorFetchesMixedTypeWorkItemFields(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	issue := connector.NewIssue()
	issue.ID = "mock-1"
	issue.Identifier = "video/mock-1"
	issue.State = "Review"
	var logs bytes.Buffer

	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "mixed-fields.db"),
		ProjectID: "video",
		Issues:    []connector.Issue{issue},
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		})),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	const fieldsJSON = `{"gate":"mock","slug":"demo","render_status":"recut","review_round":6,"approved":true,"quality":null,"nested":{"stage":1},"cuts":[1,2]}`
	if _, err := store.db.ExecContext(ctx, `
update detent_work_items set fields_json = ? where project_id = ? and id = ?`,
		fieldsJSON, "video", issue.ID); err != nil {
		t.Fatalf("update fields_json error = %v", err)
	}

	issues, err := store.FetchIssuesByStates(ctx, []string{"Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(issues))
	}
	want := map[string]string{
		"gate":          "mock",
		"slug":          "demo",
		"render_status": "recut",
		"review_round":  "6",
		"approved":      "true",
		"quality":       "null",
	}
	if len(issues[0].Fields) != len(want) {
		t.Fatalf("Fields = %#v, want %#v", issues[0].Fields, want)
	}
	for field, value := range want {
		if issues[0].Fields[field] != value {
			t.Errorf("Fields[%q] = %q, want %q", field, issues[0].Fields[field], value)
		}
	}
	for _, field := range []string{"nested", "cuts"} {
		if value, ok := issues[0].Fields[field]; ok {
			t.Errorf("Fields[%q] = %q, want field omitted", field, value)
		}
	}
	if count := strings.Count(logs.String(), "local sqlite work item field has non-string JSON value"); count != 5 {
		t.Fatalf("field warning count = %d, want 5:\n%s", count, logs.String())
	}
	for _, warning := range []string{
		"field=review_round json_type=number action=stringified",
		"field=approved json_type=boolean action=stringified",
		"field=quality json_type=null action=stringified",
		"field=nested json_type=object action=skipped",
		"field=cuts json_type=array action=skipped",
	} {
		if !strings.Contains(logs.String(), warning) {
			t.Errorf("logs missing %q:\n%s", warning, logs.String())
		}
	}
}

func TestConnectorFetchIssueCommentsReturnsEventMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "ad-1"
	issue.Identifier = "store/ad-1"
	issue.State = "Todo"

	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "comments.db"),
		ProjectID: "video",
		Issues:    []connector.Issue{issue},
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	if err := store.CreateComment(ctx, "ad-1", "Ready for review."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	got, err := store.FetchIssueComments(ctx, issue)
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueComments() len = %d, want 1", len(got))
	}
	comment := got[0]
	if comment.ID != "1" ||
		comment.Backend != connector.BackendLocalSQLite.String() ||
		comment.Body != "Ready for review." ||
		comment.AuthorLogin != "detent" ||
		comment.AuthorDisplayName != "Detent" ||
		!comment.Local ||
		!comment.CanEdit ||
		!comment.CanDelete ||
		comment.TargetType != connector.IssueCommentTargetIssue {
		t.Fatalf("FetchIssueComments()[0] = %#v, want normalized local metadata", comment)
	}
	if comment.CreatedAt == nil || !comment.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", comment.CreatedAt, now)
	}
}

func TestConnectorFetchIssueEventsReturnsCompleteHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "ad-1"
	issue.Identifier = "wi-history"
	issue.State = "Todo"
	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "events.db"),
		ProjectID: "video",
		Issues:    []connector.Issue{issue},
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if err := store.CreateComment(ctx, issue.ID, "Ready for review."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := store.UpdateIssueState(ctx, issue.ID, "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := store.SetField(ctx, issue.ID, "render_status", "queued"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}

	got, err := store.FetchIssueEvents(ctx, issue)
	if err != nil {
		t.Fatalf("FetchIssueEvents() error = %v", err)
	}
	want := []struct {
		kind  string
		state string
		body  string
		field string
		value string
	}{
		{kind: eventKindComment, body: "Ready for review."},
		{kind: eventKindStateUpdate, state: "In Progress"},
		{kind: eventKindFieldUpdate, field: "render_status", value: "queued"},
	}
	if len(got) != len(want) {
		t.Fatalf("FetchIssueEvents() len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i, expected := range want {
		if got[i].Kind != expected.kind || got[i].State != expected.state || got[i].Body != expected.body || got[i].Fields[expected.field] != expected.value {
			t.Fatalf("event[%d] = %#v, want %#v", i, got[i], expected)
		}
		if got[i].ID != strconv.Itoa(i+1) || got[i].CreatedAt == nil || !got[i].CreatedAt.Equal(now) {
			t.Fatalf("event[%d] metadata = %#v, want sequential ID and %v", i, got[i], now)
		}
	}
}

func TestConnectorUpdatesAndDeletesLocalIssueCommentsWithAuditEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	timestamps := []time.Time{
		time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 6, 12, 5, 0, 0, time.UTC),
		time.Date(2026, 7, 6, 12, 10, 0, 0, time.UTC),
	}
	next := 0
	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "comment-mutations.db"),
		ProjectID: "video",
		Now: func() time.Time {
			if next >= len(timestamps) {
				return timestamps[len(timestamps)-1]
			}
			value := timestamps[next]
			next++
			return value
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	if err := store.CreateComment(ctx, "ad-1", "Draft body"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	comments, err := store.FetchIssueComments(ctx, connector.Issue{ID: "ad-1"})
	if err != nil {
		t.Fatalf("FetchIssueComments() after create error = %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments after create len = %d, want 1", len(comments))
	}
	commentID := comments[0].ID

	if err := store.UpdateIssueComment(ctx, "ad-1", commentID, "Edited body"); err != nil {
		t.Fatalf("UpdateIssueComment() error = %v", err)
	}
	comments, err = store.FetchIssueComments(ctx, connector.Issue{ID: "ad-1"})
	if err != nil {
		t.Fatalf("FetchIssueComments() after edit error = %v", err)
	}
	if len(comments) != 1 || comments[0].Body != "Edited body" {
		t.Fatalf("comments after edit = %#v, want edited body", comments)
	}
	if comments[0].UpdatedAt == nil || !comments[0].UpdatedAt.Equal(timestamps[1]) {
		t.Fatalf("UpdatedAt = %v, want %v", comments[0].UpdatedAt, timestamps[1])
	}

	if err := store.DeleteIssueComment(ctx, "ad-1", commentID); err != nil {
		t.Fatalf("DeleteIssueComment() error = %v", err)
	}
	comments, err = store.FetchIssueComments(ctx, connector.Issue{ID: "ad-1"})
	if err != nil {
		t.Fatalf("FetchIssueComments() after delete error = %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("comments after delete = %#v, want none", comments)
	}

	rows, err := store.db.QueryContext(ctx, `
select event_kind, body, payload_json from detent_work_item_events
where project_id = ? and item_id = ?
order by id asc`, "video", "ad-1")
	if err != nil {
		t.Fatalf("query events error = %v", err)
	}
	defer rows.Close()

	type eventRow struct {
		kind    string
		body    string
		payload map[string]string
	}
	got := []eventRow{}
	for rows.Next() {
		var row eventRow
		var payloadJSON string
		if err := rows.Scan(&row.kind, &row.body, &payloadJSON); err != nil {
			t.Fatalf("scan event error = %v", err)
		}
		row.payload = unmarshalStringMap(payloadJSON)
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("event rows len = %d, want 3: %#v", len(got), got)
	}
	if got[0].kind != eventKindComment || got[0].body != "Draft body" {
		t.Fatalf("create event = %#v, want comment draft", got[0])
	}
	if got[1].kind != eventKindCommentEdit || got[1].body != "Edited body" ||
		got[1].payload[commentPayloadCommentID] != commentID ||
		got[1].payload[commentPayloadPreviousBody] != "Draft body" {
		t.Fatalf("edit event = %#v, want comment id and previous body", got[1])
	}
	if got[2].kind != eventKindCommentDelete ||
		got[2].payload[commentPayloadCommentID] != commentID ||
		got[2].payload[commentPayloadPreviousBody] != "Edited body" {
		t.Fatalf("delete event = %#v, want comment id and previous body", got[2])
	}
}

func TestConnectorFetchIssueCommentsPreservesEventOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	timestamps := []time.Time{
		time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 6, 12, 0, 0, 100_000_000, time.UTC),
	}
	next := 0
	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "comment-order.db"),
		ProjectID: "video",
		Now: func() time.Time {
			if next >= len(timestamps) {
				return timestamps[len(timestamps)-1]
			}
			value := timestamps[next]
			next++
			return value
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	if err := store.CreateComment(ctx, "ad-1", "first exact-second comment"); err != nil {
		t.Fatalf("CreateComment() first error = %v", err)
	}
	if err := store.CreateComment(ctx, "ad-1", "second fractional comment"); err != nil {
		t.Fatalf("CreateComment() second error = %v", err)
	}

	got, err := store.FetchIssueComments(ctx, connector.Issue{ID: "ad-1"})
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	wantBodies := []string{"first exact-second comment", "second fractional comment"}
	if len(got) != len(wantBodies) {
		t.Fatalf("FetchIssueComments() len = %d, want %d", len(got), len(wantBodies))
	}
	for index, want := range wantBodies {
		if got[index].Body != want {
			t.Fatalf("comment[%d].Body = %q, want %q; comments = %#v", index, got[index].Body, want, got)
		}
	}
}

func TestConnectorFetchIssuesByStatesLimit(t *testing.T) {
	t.Parallel()

	first := connector.NewIssue()
	first.ID = "ad-1"
	first.Identifier = "store/ad-1"
	first.State = "Todo"
	second := connector.NewIssue()
	second.ID = "ad-2"
	second.Identifier = "store/ad-2"
	second.State = "Todo"

	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "limit.db"),
		ProjectID: "video",
		Issues:    []connector.Issue{first, second},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	issues, err := store.FetchIssuesByStatesLimit(context.Background(), []string{"Todo"}, 1)
	if err != nil {
		t.Fatalf("FetchIssuesByStatesLimit() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("FetchIssuesByStatesLimit() len = %d, want 1", len(issues))
	}
}

func TestConnectorUpsertGitHubIdentityAndLocalIssueFields(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := New(Config{
		Path:           filepath.Join(t.TempDir(), "github-local.db"),
		ProjectID:      "detent",
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	issue := connector.NewIssue()
	issue.ID = "github:123:779"
	issue.Identifier = "digitaldrywood/detent#779"
	issue.Title = "Add local status mode"
	issue.State = "Todo"
	issue.Metadata = map[string]string{
		MetadataGitHubNodeID:        "I_kwDOtest779",
		MetadataGitHubRepositoryID:  "123",
		MetadataGitHubIssueNumber:   "779",
		MetadataGitHubUpstreamState: "open",
	}
	if err := store.UpsertIssues(ctx, []connector.Issue{issue}); err != nil {
		t.Fatalf("UpsertIssues() error = %v", err)
	}
	if err := store.SetIssueField(ctx, "github:123:779", 42, "agent-1"); err != nil {
		t.Fatalf("SetIssueField() error = %v", err)
	}
	if err := store.ClearIssueField(ctx, "github:123:779", 42); err != nil {
		t.Fatalf("ClearIssueField() error = %v", err)
	}
	if err := store.CloseIssue(ctx, "github:123:779"); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}

	issues, err := store.FetchIssueStatesByIdentifiers(ctx, []string{"digitaldrywood/detent#779"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	got := issues[0]
	if got.State != "Done" {
		t.Fatalf("State = %q, want Done", got.State)
	}
	if got.Metadata[MetadataGitHubNodeID] != "I_kwDOtest779" ||
		got.Metadata[MetadataGitHubRepositoryID] != "123" ||
		got.Metadata[MetadataGitHubIssueNumber] != "779" {
		t.Fatalf("GitHub metadata = %#v", got.Metadata)
	}
	if value, ok := got.Fields[issueFieldKey(42)]; ok || value != "" {
		t.Fatalf("issue field persisted = %q, ok=%v; want cleared", value, ok)
	}
}

func TestConnectorBackfillsWorkItemNumbersByProjectCreationOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backfill.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `
create table detent_work_items (
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
github_node_id text not null default '',
github_repository_id integer not null default 0,
github_issue_number integer not null default 0,
github_upstream_state text not null default '',
github_orphaned integer not null default 0,
primary key (project_id, id)
)`)
	if err != nil {
		t.Fatalf("create legacy table error = %v", err)
	}
	created := []struct {
		projectID  string
		id         string
		identifier string
		createdAt  time.Time
	}{
		{"video", "ad-2", "wi-video-2", time.Date(2026, 7, 1, 12, 2, 0, 0, time.UTC)},
		{"video", "ad-1", "wi-video-1", time.Date(2026, 7, 1, 12, 1, 0, 0, time.UTC)},
		{"audio", "mix-1", "wi-audio-1", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
	}
	for _, row := range created {
		if _, err := db.ExecContext(ctx, `
insert into detent_work_items(project_id, id, identifier, title, state, created_at, updated_at, stage_updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.projectID, row.id, row.identifier, row.identifier, "Todo", formatTime(row.createdAt), formatTime(row.createdAt), formatTime(row.createdAt)); err != nil {
			t.Fatalf("insert legacy row error = %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("legacy Close() error = %v", err)
	}

	store, err := New(Config{Path: path, ProjectID: "video"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	issues, err := store.FetchIssuesByStates(ctx, []string{"Todo"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(video) error = %v", err)
	}
	numbers := map[string]int{}
	for _, issue := range issues {
		numbers[issue.ID] = issue.Number
	}
	if numbers["ad-1"] != 1 || numbers["ad-2"] != 2 {
		t.Fatalf("video numbers = %#v, want ad-1=1 ad-2=2", numbers)
	}

	next := connector.NewIssue()
	next.ID = "ad-3"
	next.Identifier = "wi-video-3"
	next.State = "Todo"
	if err := store.UpsertIssues(ctx, []connector.Issue{next}); err != nil {
		t.Fatalf("UpsertIssues(next) error = %v", err)
	}
	createdIssue, err := store.FetchIssueStatesByIdentifiers(ctx, []string{"#3"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers(#3) error = %v", err)
	}
	if len(createdIssue) != 1 || createdIssue[0].ID != "ad-3" || createdIssue[0].Number != 3 {
		t.Fatalf("created issue by #3 = %#v, want ad-3 number 3", createdIssue)
	}
	literal := connector.NewIssue()
	literal.ID = "literal-3"
	literal.Identifier = "3"
	literal.State = "Todo"
	if err := store.UpsertIssues(ctx, []connector.Issue{literal}); err != nil {
		t.Fatalf("UpsertIssues(literal) error = %v", err)
	}
	literalMatch, err := store.FetchIssueStatesByIdentifiers(ctx, []string{"3"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers(3) error = %v", err)
	}
	if len(literalMatch) != 1 || literalMatch[0].ID != "literal-3" {
		t.Fatalf("literal numeric identifier match = %#v, want literal-3 only", literalMatch)
	}
}

func TestConnectorAssignsUniqueConcurrentWorkItemNumbers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "concurrent.db"),
		ProjectID: "video",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	const count = 24
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			suffix := strconv.Itoa(i)
			issue := connector.NewIssue()
			issue.ID = "ad-" + suffix
			issue.Identifier = "wi-concurrent-" + suffix
			issue.State = "Todo"
			errs <- store.UpsertIssues(ctx, []connector.Issue{issue})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpsertIssues() error = %v", err)
		}
	}

	issues, err := store.FetchIssuesByStates(ctx, []string{"Todo"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != count {
		t.Fatalf("issues len = %d, want %d", len(issues), count)
	}
	seen := map[int]struct{}{}
	for _, issue := range issues {
		if issue.Number < 1 || issue.Number > count {
			t.Fatalf("issue %s number = %d, want 1..%d", issue.ID, issue.Number, count)
		}
		if _, ok := seen[issue.Number]; ok {
			t.Fatalf("duplicate number %d in %#v", issue.Number, issues)
		}
		seen[issue.Number] = struct{}{}
	}
}
