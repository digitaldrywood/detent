package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestTickReconcilesRunningIssueTrackerState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	prNumber := 226
	prior := connector.Issue{
		ID:         "issue-running",
		Identifier: "digitaldrywood/detent#225",
		Title:      "Dispatch title",
		State:      "Todo",
		URL:        "https://github.com/digitaldrywood/detent/issues/225",
		Labels:     []string{"bug"},
		PRNumber:   &prNumber,
		PullRequest: &connector.PullRequest{
			Number:           prNumber,
			URL:              "https://github.com/digitaldrywood/detent/pull/226",
			BranchName:       "detent/digitaldrywood_detent_225",
			State:            "OPEN",
			CIStatus:         "success",
			CodexReviewState: "COMMENTED",
		},
	}

	tests := []struct {
		name       string
		tracker    []connector.Issue
		err        error
		wantState  string
		wantTitle  string
		wantURL    string
		wantLabels []string
	}{
		{
			name: "updates running issue from tracker",
			tracker: []connector.Issue{
				{
					ID:         prior.ID,
					Identifier: prior.Identifier,
					Title:      "Live title",
					State:      "In Progress",
					URL:        "https://github.com/digitaldrywood/detent/issues/225#live",
					Labels:     []string{"bug", "live"},
				},
			},
			wantState:  "In Progress",
			wantTitle:  "Live title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225#live",
			wantLabels: []string{"bug", "live"},
		},
		{
			name:       "retains previous running issue on fetch error",
			err:        errors.New("tracker unavailable"),
			wantState:  "Todo",
			wantTitle:  "Dispatch title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225",
			wantLabels: []string{"bug"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			state.Running[prior.ID] = Running{Issue: cloneIssue(prior)}
			state.Claimed[prior.ID] = Claimed{Issue: cloneIssue(prior)}

			tracker := &runningStateConnector{
				issues: tt.tracker,
				err:    tt.err,
			}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			snapshot := state.Snapshot(now)
			if len(snapshot.Running) != 1 {
				t.Fatalf("Running len = %d, want 1", len(snapshot.Running))
			}
			got := snapshot.Running[0].Issue
			if got.State != tt.wantState {
				t.Fatalf("running snapshot state = %q, want %q", got.State, tt.wantState)
			}
			if got.Title != tt.wantTitle {
				t.Fatalf("running snapshot title = %q, want %q", got.Title, tt.wantTitle)
			}
			if got.URL != tt.wantURL {
				t.Fatalf("running snapshot URL = %q, want %q", got.URL, tt.wantURL)
			}
			if !slices.Equal(got.Labels, tt.wantLabels) {
				t.Fatalf("running snapshot labels = %#v, want %#v", got.Labels, tt.wantLabels)
			}
			if got.PullRequest == nil {
				t.Fatal("running snapshot pull request = nil, want preserved metadata")
			}
			if got.PullRequest.URL != "https://github.com/digitaldrywood/detent/pull/226" {
				t.Fatalf("running snapshot pull request URL = %q, want preserved metadata", got.PullRequest.URL)
			}
			if got.PullRequest.CIStatus != "success" || got.PullRequest.CodexReviewState != "COMMENTED" {
				t.Fatalf("running snapshot pull request status = %#v, want preserved metadata", got.PullRequest)
			}
			if !slices.Equal(tracker.requestedIDs, []string{prior.ID}) {
				t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want [%s]", tracker.requestedIDs, prior.ID)
			}
		})
	}
}

func TestMergeIssueTrackerFieldsDistinguishesMissingAndEmptyMetadata(t *testing.T) {
	t.Parallel()

	current := connector.Issue{
		ID:        "issue-running",
		Assignees: []string{"worker-1"},
		Fields:    map[string]string{"Status": "Todo"},
	}

	tests := []struct {
		name          string
		refreshed     connector.Issue
		wantAssignees []string
		wantFields    map[string]string
	}{
		{
			name:          "missing metadata preserves current values",
			refreshed:     connector.Issue{ID: current.ID},
			wantAssignees: []string{"worker-1"},
			wantFields:    map[string]string{"Status": "Todo"},
		},
		{
			name:          "explicit empty metadata clears current values",
			refreshed:     connector.Issue{ID: current.ID, Assignees: []string{}, Fields: map[string]string{}},
			wantAssignees: []string{},
			wantFields:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mergeIssueTrackerFields(current, tt.refreshed)
			if !slices.Equal(got.Assignees, tt.wantAssignees) {
				t.Fatalf("Assignees = %#v, want %#v", got.Assignees, tt.wantAssignees)
			}
			if len(got.Fields) != len(tt.wantFields) {
				t.Fatalf("Fields = %#v, want %#v", got.Fields, tt.wantFields)
			}
			for key, value := range tt.wantFields {
				if got.Fields[key] != value {
					t.Fatalf("Fields[%q] = %q, want %q", key, got.Fields[key], value)
				}
			}
		})
	}
}

type runningStateConnector struct {
	issues       []connector.Issue
	err          error
	requestedIDs []string
}

func (c *runningStateConnector) Name() string {
	return "running-state"
}

func (c *runningStateConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *runningStateConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *runningStateConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	c.requestedIDs = append([]string(nil), ids...)
	if c.err != nil {
		return nil, c.err
	}
	return cloneIssues(c.issues), nil
}

func (c *runningStateConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *runningStateConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}
