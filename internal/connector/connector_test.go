package connector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type testConnector struct{}

func (testConnector) Name() string {
	return "test"
}

func (testConnector) FetchCandidateIssues(context.Context) ([]Issue, error) {
	return nil, ErrNotImplemented
}

func (testConnector) FetchIssuesByStates(context.Context, []string) ([]Issue, error) {
	return nil, ErrNotImplemented
}

func (testConnector) FetchIssueStatesByIDs(context.Context, []string) ([]Issue, error) {
	return nil, ErrNotImplemented
}

func (testConnector) CreateComment(context.Context, string, string) error {
	return ErrNotImplemented
}

func (testConnector) UpdateIssueState(context.Context, string, string) error {
	return ErrNotImplemented
}

func TestConnectorInterface(t *testing.T) {
	t.Parallel()

	var c Connector = testConnector{}

	if c.Name() != "test" {
		t.Fatalf("Name() = %q, want test", c.Name())
	}
	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrNotImplemented", err)
	}
}

func TestBackendString(t *testing.T) {
	t.Parallel()

	if got := BackendGitHub.String(); got != "github" {
		t.Fatalf("BackendGitHub.String() = %q, want github", got)
	}
}

func TestIssueDefaults(t *testing.T) {
	t.Parallel()

	issue := NewIssue()

	if !issue.AssignedToWorker {
		t.Fatal("AssignedToWorker = false, want true")
	}
	if issue.ModelOverride != "" {
		t.Fatalf("ModelOverride = %q, want empty string", issue.ModelOverride)
	}
	if len(issue.BlockedBy) != 0 {
		t.Fatalf("BlockedBy len = %d, want 0", len(issue.BlockedBy))
	}
	if len(issue.Labels) != 0 {
		t.Fatalf("Labels len = %d, want 0", len(issue.Labels))
	}
	if len(issue.Assignees) != 0 {
		t.Fatalf("Assignees len = %d, want 0", len(issue.Assignees))
	}
	if len(issue.Fields) != 0 {
		t.Fatalf("Fields len = %d, want 0", len(issue.Fields))
	}
}

func TestIssueJSONUsesElixirFieldNames(t *testing.T) {
	t.Parallel()

	priority := 2
	createdAt := time.Date(2026, 5, 30, 17, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	issue := Issue{
		ID:               "issue-1",
		Identifier:       "DIG-1",
		Title:            "Port connector",
		Description:      "Build the connector seam",
		Priority:         &priority,
		State:            "Todo",
		BranchName:       "detent/dig-1",
		URL:              "https://example.com/issues/1",
		AuthorID:         "author-1",
		AssigneeID:       "user-1",
		Assignees:        []string{"user-1", "user-2"},
		BlockedBy:        []BlockedRef{{ID: "issue-0", Identifier: "DIG-0", State: "Done"}},
		Labels:           []string{"backend", "stage:s1"},
		Fields:           map[string]string{"Status": "Todo"},
		AssignedToWorker: true,
		CreatedAt:        &createdAt,
		UpdatedAt:        &updatedAt,
		ModelOverride:    "gpt-5-codex-high",
	}

	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"id",
		"identifier",
		"title",
		"description",
		"priority",
		"state",
		"branch_name",
		"url",
		"author_id",
		"assignee_id",
		"assignees",
		"blocked_by",
		"labels",
		"fields",
		"assigned_to_worker",
		"created_at",
		"updated_at",
		"model_override",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON missing key %q in %s", key, raw)
		}
	}
}

func TestBlockedRefJSONUsesElixirFieldNames(t *testing.T) {
	t.Parallel()

	ref := BlockedRef{ID: "issue-2", Identifier: "digitaldrywood/detent#2", State: "Done"}

	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got["id"] != "issue-2" {
		t.Fatalf("id = %q, want issue-2", got["id"])
	}
	if got["identifier"] != "digitaldrywood/detent#2" {
		t.Fatalf("identifier = %q, want digitaldrywood/detent#2", got["identifier"])
	}
	if got["state"] != "Done" {
		t.Fatalf("state = %q, want Done", got["state"])
	}
}
