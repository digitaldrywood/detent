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
