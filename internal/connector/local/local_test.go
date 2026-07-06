package local

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
		!comment.Local ||
		comment.TargetType != connector.IssueCommentTargetIssue {
		t.Fatalf("FetchIssueComments()[0] = %#v, want normalized local metadata", comment)
	}
	if comment.CreatedAt == nil || !comment.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", comment.CreatedAt, now)
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
