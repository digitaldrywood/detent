package memory

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorFetchesConfiguredIssues(t *testing.T) {
	t.Parallel()

	priority := 2
	createdAt := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		{
			ID:               "issue-1",
			Identifier:       "MT-1",
			Title:            "Memory adapter",
			Description:      "Load issues from config",
			Priority:         &priority,
			State:            "Todo",
			BranchName:       "detent/mt-1",
			URL:              "https://example.com/issues/1",
			AuthorID:         "author-1",
			AssigneeID:       "worker-1",
			Assignees:        []string{"worker-1", "worker-2"},
			BlockedBy:        []connector.BlockedRef{{ID: "issue-0", Identifier: "MT-0", State: "Done"}},
			ChildIssues:      []connector.BlockedRef{{ID: "issue-10", Identifier: "MT-10", State: "Done"}},
			Labels:           []string{"stage:s1"},
			Fields:           map[string]string{"Status": "Todo", "Track": "foundation"},
			AssignedToWorker: true,
			CreatedAt:        &createdAt,
			UpdatedAt:        &createdAt,
			ModelOverride:    "gpt-5-codex-high",
		},
	}

	c := New(Config{Issues: issues})
	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}

	if !reflect.DeepEqual(got, issues) {
		t.Fatalf("FetchCandidateIssues() = %#v, want %#v", got, issues)
	}
}

func TestConnectorReturnsDefensiveIssueCopies(t *testing.T) {
	t.Parallel()

	priority := 1
	createdAt := time.Date(2026, 5, 31, 11, 0, 0, 0, time.UTC)
	c := New(Config{Issues: []connector.Issue{{
		ID:        "issue-1",
		Priority:  &priority,
		State:     "Todo",
		Assignees: []string{"worker-1"},
		BlockedBy: []connector.BlockedRef{{Identifier: "MT-0"}},
		ChildIssues: []connector.BlockedRef{{
			Identifier: "MT-10",
		}},
		Labels:    []string{"stage:s1"},
		Fields:    map[string]string{"Status": "Todo"},
		CreatedAt: &createdAt,
		UpdatedAt: &createdAt,
	}}})
	priority = 4
	createdAt = createdAt.Add(time.Hour)

	first, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() first error = %v", err)
	}
	*first[0].Priority = 3
	first[0].Assignees[0] = "changed"
	first[0].BlockedBy[0].Identifier = "changed"
	first[0].ChildIssues[0].Identifier = "changed"
	first[0].Labels[0] = "changed"
	first[0].Fields["Status"] = "changed"
	*first[0].CreatedAt = first[0].CreatedAt.Add(time.Hour)
	*first[0].UpdatedAt = first[0].UpdatedAt.Add(time.Hour)

	second, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() second error = %v", err)
	}

	if *second[0].Priority != 1 {
		t.Fatalf("Priority = %d, want 1", *second[0].Priority)
	}
	if second[0].Assignees[0] != "worker-1" {
		t.Fatalf("Assignees[0] = %q, want worker-1", second[0].Assignees[0])
	}
	if second[0].BlockedBy[0].Identifier != "MT-0" {
		t.Fatalf("BlockedBy[0].Identifier = %q, want MT-0", second[0].BlockedBy[0].Identifier)
	}
	if second[0].ChildIssues[0].Identifier != "MT-10" {
		t.Fatalf("ChildIssues[0].Identifier = %q, want MT-10", second[0].ChildIssues[0].Identifier)
	}
	if second[0].Labels[0] != "stage:s1" {
		t.Fatalf("Labels[0] = %q, want stage:s1", second[0].Labels[0])
	}
	if second[0].Fields["Status"] != "Todo" {
		t.Fatalf("Fields[Status] = %q, want Todo", second[0].Fields["Status"])
	}
	if !second[0].CreatedAt.Equal(time.Date(2026, 5, 31, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("CreatedAt = %v, want original time", second[0].CreatedAt)
	}
	if !second[0].UpdatedAt.Equal(time.Date(2026, 5, 31, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("UpdatedAt = %v, want original time", second[0].UpdatedAt)
	}
}

func TestConnectorFetchIssuesByStatesMatchesElixirMemoryAdapter(t *testing.T) {
	t.Parallel()

	c := New(Config{Issues: []connector.Issue{
		{ID: "issue-1", State: "Todo"},
		{ID: "issue-2", State: " in progress "},
		{ID: "issue-3", State: "Done"},
		{ID: "issue-4"},
	}})

	tests := []struct {
		name   string
		states []string
		want   []string
	}{
		{name: "matches state ignoring case and whitespace", states: []string{" todo ", "IN PROGRESS"}, want: []string{"issue-1", "issue-2"}},
		{name: "empty state list matches no issues", states: []string{}, want: []string{}},
		{name: "blank state matches blank issue state", states: []string{" "}, want: []string{"issue-4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.FetchIssuesByStates(context.Background(), tt.states)
			if err != nil {
				t.Fatalf("FetchIssuesByStates() error = %v", err)
			}

			if ids := issueIDs(got); !reflect.DeepEqual(ids, tt.want) {
				t.Fatalf("FetchIssuesByStates() ids = %#v, want %#v", ids, tt.want)
			}
		})
	}
}

func TestConnectorFetchIssueStatesByIDsMatchesElixirMemoryAdapter(t *testing.T) {
	t.Parallel()

	c := New(Config{Issues: []connector.Issue{
		{ID: "issue-1", State: "Todo"},
		{ID: "issue-2", State: "Done"},
		{ID: "", State: "No ID"},
	}})

	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{name: "matches exact IDs in configured order", ids: []string{"issue-2", "issue-1"}, want: []string{"issue-1", "issue-2"}},
		{name: "empty ID list matches no issues", ids: []string{}, want: []string{}},
		{name: "blank ID matches blank issue ID", ids: []string{""}, want: []string{""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.FetchIssueStatesByIDs(context.Background(), tt.ids)
			if err != nil {
				t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
			}

			if ids := issueIDs(got); !reflect.DeepEqual(ids, tt.want) {
				t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want %#v", ids, tt.want)
			}
		})
	}
}

func TestConnectorFetchIssueParents(t *testing.T) {
	t.Parallel()

	c := New(Config{Issues: []connector.Issue{
		{
			ID:         "epic-1",
			Identifier: "MT-1",
			ChildIssues: []connector.BlockedRef{{
				ID:         "child-1",
				Identifier: "MT-10",
			}},
		},
		{
			ID:         "epic-2",
			Identifier: "MT-2",
			BlockedBy:  []connector.BlockedRef{{Identifier: "MT-10"}},
		},
		{
			ID:         "child-1",
			Identifier: "MT-10",
			State:      "Done",
		},
		{
			ID:         "unrelated",
			Identifier: "MT-3",
			ChildIssues: []connector.BlockedRef{{
				ID:         "child-2",
				Identifier: "MT-11",
			}},
		},
	}})

	got, err := c.FetchIssueParents(context.Background(), "child-1")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if ids := issueIDs(got); !reflect.DeepEqual(ids, []string{"epic-1", "epic-2"}) {
		t.Fatalf("FetchIssueParents() ids = %#v, want [epic-1 epic-2]", ids)
	}
}

func TestConnectorMutationsMatchElixirMemoryAdapter(t *testing.T) {
	t.Parallel()

	var events []Event
	c := New(Config{
		Issues: []connector.Issue{{ID: "issue-1", State: "Todo"}},
		EventSink: func(event Event) {
			events = append(events, event)
		},
	})

	if err := c.CreateComment(context.Background(), "issue-1", "comment"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := c.CloseIssue(context.Background(), "issue-1"); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}
	if err := c.UpdateIssueState(context.Background(), "issue-1", "Done"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := c.SetAssignee(context.Background(), "issue-1", "worker-1"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}
	if err := c.SetField(context.Background(), "issue-1", "Owner", "worker-1"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}
	issues, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}

	wantEvents := []Event{
		{Kind: EventKindComment, IssueID: "issue-1", Body: "comment"},
		{Kind: EventKindClose, IssueID: "issue-1"},
		{Kind: EventKindStateUpdate, IssueID: "issue-1", State: "Done"},
		{Kind: EventKindAssigneeUpdate, IssueID: "issue-1", Login: "worker-1"},
		{Kind: EventKindFieldUpdate, IssueID: "issue-1", FieldName: "Owner", FieldValue: "worker-1"},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if issues[0].State != "Todo" {
		t.Fatalf("State after UpdateIssueState() = %q, want Todo", issues[0].State)
	}
	if len(issues[0].Assignees) != 0 {
		t.Fatalf("Assignees after SetAssignee() = %#v, want unchanged empty slice", issues[0].Assignees)
	}
	if len(issues[0].Fields) != 0 {
		t.Fatalf("Fields after SetField() = %#v, want unchanged empty map", issues[0].Fields)
	}
}

func TestConnectorMutationsNoopWithoutEventSink(t *testing.T) {
	t.Parallel()

	c := New(Config{})

	if err := c.CreateComment(context.Background(), "issue-1", "comment"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := c.CloseIssue(context.Background(), "issue-1"); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}
	if err := c.UpdateIssueState(context.Background(), "issue-1", "Done"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := c.SetAssignee(context.Background(), "issue-1", "worker-1"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}
	if err := c.SetField(context.Background(), "issue-1", "Owner", "worker-1"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}
}

func issueIDs(issues []connector.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}
