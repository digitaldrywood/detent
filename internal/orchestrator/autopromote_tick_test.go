package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestTickAutoPromoteHumanReviewIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	recentReview := now.Add(-30 * time.Second)

	tests := []struct {
		name                 string
		cfg                  AutoPromoteConfig
		issue                connector.Issue
		wantUpdates          []autoPromoteTickUpdate
		wantCommentFragments []string
	}{
		{
			name: "promotes ready issue to merging",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-ready", []string{"bug"}, &connector.PullRequest{
				Number:                 42,
				URL:                    "https://github.test/digitaldrywood/detent/pull/42",
				State:                  "OPEN",
				CIStatus:               "success",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-ready",
				state:   "Merging",
			}},
			wantCommentFragments: []string{
				"Auto-promoted this issue from Human Review to Merging.",
				"reason: ready",
				"https://github.test/digitaldrywood/detent/pull/42",
			},
		},
		{
			name: "routes P1 findings to rework",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-p1", []string{"bug"}, &connector.PullRequest{
				Number:                 43,
				URL:                    "https://github.test/digitaldrywood/detent/pull/43",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "P1",
				CodexReviewSubmittedAt: &oldReview,
				CodexReviewFindings: []connector.PullRequestFinding{{
					Body: "![P1 Badge](https://example.test/p1.svg) Unsafe migration.",
					URL:  "https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
				}},
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-p1",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework.",
				"reason: p1_findings",
				"Unsafe migration.",
				"https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
			},
		},
		{
			name: "waits for quiet period",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-recent", []string{"bug"}, &connector.PullRequest{
				Number:                 44,
				URL:                    "https://github.test/digitaldrywood/detent/pull/44",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &recentReview,
			}),
		},
		{
			name: "honors evaluator label filters",
			cfg: AutoPromoteConfig{
				Enabled:            true,
				QuietDuration:      10 * time.Minute,
				AllowedIssueLabels: []string{"release"},
			},
			issue: autoPromoteTickIssue("issue-label", []string{"bug"}, &connector.PullRequest{
				Number:                 45,
				URL:                    "https://github.test/digitaldrywood/detent/pull/45",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
		{
			name: "disabled config does not evaluate",
			cfg: AutoPromoteConfig{
				Enabled:       false,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-disabled", []string{"bug"}, &connector.PullRequest{
				Number:                 46,
				URL:                    "https://github.test/digitaldrywood/detent/pull/46",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				AutoPromote:         tt.cfg,
				ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{tt.issue}}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}

			orch.tick(context.Background(), &state, now)

			if !reflect.DeepEqual(tracker.updates, tt.wantUpdates) {
				t.Fatalf("updates = %#v, want %#v", tracker.updates, tt.wantUpdates)
			}
			if len(tracker.fetchByStatesRequests) != 1 {
				t.Fatalf("FetchIssuesByStates() calls = %d, want 1", len(tracker.fetchByStatesRequests))
			}
			if !autoPromoteTickStatesEqual(tracker.fetchByStatesRequests[0], []string{"Blocked", "Human Review", "Merging"}) {
				t.Fatalf("FetchIssuesByStates() states = %#v, want Blocked/Human Review/Merging", tracker.fetchByStatesRequests[0])
			}
			if len(tt.wantCommentFragments) == 0 {
				if len(tracker.comments) != 0 {
					t.Fatalf("comments = %#v, want none", tracker.comments)
				}
				return
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("comments = %#v, want one comment", tracker.comments)
			}
			for _, fragment := range tt.wantCommentFragments {
				if !strings.Contains(tracker.comments[0].body, fragment) {
					t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
				}
			}
		})
	}
}

func autoPromoteTickIssue(id string, labels []string, pullRequest *connector.PullRequest) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#42"
	issue.Title = "Auto promote test"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	issue.PullRequest = pullRequest
	return issue
}

func autoPromoteTickStatesEqual(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, state := range got {
		seen[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	for _, state := range want {
		if _, ok := seen[strings.ToLower(strings.TrimSpace(state))]; !ok {
			return false
		}
	}
	return true
}

type autoPromoteTickUpdate struct {
	issueID string
	state   string
}

type autoPromoteTickComment struct {
	issueID string
	body    string
}

type autoPromoteTickConnector struct {
	stateIssues           []connector.Issue
	fetchByStatesRequests [][]string
	updates               []autoPromoteTickUpdate
	comments              []autoPromoteTickComment
}

func (c *autoPromoteTickConnector) Name() string {
	return "auto-promote-tick"
}

func (c *autoPromoteTickConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *autoPromoteTickConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetchByStatesRequests = append(c.fetchByStatesRequests, append([]string(nil), states...))
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		wanted[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.State))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *autoPromoteTickConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, autoPromoteTickComment{issueID: issueID, body: body})
	return nil
}

func (c *autoPromoteTickConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, autoPromoteTickUpdate{issueID: issueID, state: state})
	return nil
}

func (c *autoPromoteTickConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *autoPromoteTickConnector) SetField(context.Context, string, string, string) error {
	return nil
}
