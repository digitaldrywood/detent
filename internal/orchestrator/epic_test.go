package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestEpicIssueDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue connector.Issue
		want  bool
	}{
		{
			name:  "epic label",
			issue: epicTestIssue("issue-label", "Todo", false, "Release", []string{"enhancement", "Epic"}, ""),
			want:  true,
		},
		{
			name:  "epic title prefix",
			issue: epicTestIssue("issue-title", "Todo", false, " epic: Release readiness ", nil, ""),
			want:  true,
		},
		{
			name:  "non epic",
			issue: epicTestIssue("issue-feature", "Todo", false, "Release readiness", []string{"enhancement"}, ""),
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := epicIssue(tt.issue); got != tt.want {
				t.Fatalf("epicIssue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEpicChildRefs(t *testing.T) {
	t.Parallel()

	issue := epicTestIssue("epic", "Todo", false, "Epic: Release", nil, strings.Join([]string{
		"- [ ] #251",
		"- [x] https://github.com/digitaldrywood/detent/issues/252",
		"Depends on: #253 digitaldrywood/detent#253",
	}, "\n"))
	issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#254"}}
	issue.ChildIssues = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#255"}}

	got := epicChildRefs(issue)
	want := []connector.BlockedRef{
		{Identifier: "digitaldrywood/detent#251"},
		{Identifier: "digitaldrywood/detent#252"},
		{Identifier: "digitaldrywood/detent#253"},
		{Identifier: "digitaldrywood/detent#254"},
		{Identifier: "digitaldrywood/detent#255"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("epicChildRefs() = %#v, want %#v", got, want)
	}
}

func TestTickFinalizesCompletedEpics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		candidates   []connector.Issue
		stateIssues  []connector.Issue
		resolved     []connector.Issue
		linked       map[string][]connector.BlockedRef
		wantUpdates  []epicStateUpdate
		wantClosed   []string
		wantComments []string
	}{
		{
			name: "completed active epic is commented closed and moved to done",
			candidates: []connector.Issue{
				epicTestIssue("epic-258", "Todo", false, "Epic: Release readiness", []string{"epic"}, "- [ ] #251\n- [ ] #252"),
			},
			stateIssues: []connector.Issue{
				epicTestIssue("child-251", "Done", false, "Child 251", nil, ""),
			},
			resolved: []connector.Issue{
				epicTestIssue("child-252", "Open", true, "Child 252", nil, ""),
			},
			wantUpdates:  []epicStateUpdate{{issueID: "epic-258", state: "Done"}},
			wantClosed:   []string{"epic-258"},
			wantComments: []string{"Auto-closing completed epic: 2 child issues are Done."},
		},
		{
			name: "partial epic is untouched",
			candidates: []connector.Issue{
				epicTestIssue("epic-258", "Todo", false, "Epic: Release readiness", []string{"epic"}, "- [ ] #251\n- [ ] #252"),
				epicTestIssue("child-252", "In Progress", false, "Child 252", nil, ""),
			},
			stateIssues: []connector.Issue{
				epicTestIssue("child-251", "Done", false, "Child 251", nil, ""),
			},
		},
		{
			name: "paginated linked child keeps epic open",
			candidates: []connector.Issue{
				func() connector.Issue {
					issue := epicTestIssue("epic-258", "Todo", false, "Epic: Release readiness", []string{"epic"}, "")
					issue.ChildIssues = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#251", State: "Done"}}
					return issue
				}(),
			},
			linked: map[string][]connector.BlockedRef{
				"epic-258": {
					{Identifier: "digitaldrywood/detent#251", State: "Done"},
					{Identifier: "digitaldrywood/detent#252", State: "In Progress"},
				},
			},
		},
		{
			name: "already closed done epic is idempotent",
			stateIssues: []connector.Issue{
				func() connector.Issue {
					issue := epicTestIssue("epic-258", "Done", true, "Epic: Release readiness", []string{"epic"}, "")
					issue.ChildIssues = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#251"}}
					return issue
				}(),
				epicTestIssue("child-251", "Done", false, "Child 251", nil, ""),
			},
		},
		{
			name: "open done epic is closed without status update",
			stateIssues: []connector.Issue{
				func() connector.Issue {
					issue := epicTestIssue("epic-258", "Done", false, "Epic: Release readiness", []string{"epic"}, "")
					issue.ChildIssues = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#251"}}
					return issue
				}(),
				epicTestIssue("child-251", "Done", false, "Child 251", nil, ""),
			},
			wantClosed:   []string{"epic-258"},
			wantComments: []string{"Auto-closing completed epic: 1 child issue is Done."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress"},
				TerminalStates:      []string{"Done", "Cancelled", "Closed"},
			})
			state := newState(cfg)
			state.Running["occupied"] = Running{Issue: connector.Issue{ID: "occupied", Identifier: "occupied", Title: "Occupied", State: "Todo"}}
			tracker := &epicConnector{
				candidates:  tt.candidates,
				stateIssues: tt.stateIssues,
				resolved:    tt.resolved,
				linked:      tt.linked,
			}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			if got := tracker.stateUpdates(); !reflect.DeepEqual(got, tt.wantUpdates) {
				t.Fatalf("state updates = %#v, want %#v", got, tt.wantUpdates)
			}
			if got := tracker.closedIssues(); !reflect.DeepEqual(got, tt.wantClosed) {
				t.Fatalf("closed issues = %#v, want %#v", got, tt.wantClosed)
			}
			if got := tracker.commentBodies(); !reflect.DeepEqual(got, tt.wantComments) {
				t.Fatalf("comments = %#v, want %#v", got, tt.wantComments)
			}
		})
	}
}

type epicConnector struct {
	mu          sync.Mutex
	candidates  []connector.Issue
	stateIssues []connector.Issue
	resolved    []connector.Issue
	linked      map[string][]connector.BlockedRef
	updates     []epicStateUpdate
	closed      []string
	comments    []string
}

type epicStateUpdate struct {
	issueID string
	state   string
}

func (c *epicConnector) Name() string {
	return "epic"
}

func (c *epicConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneIssues(c.candidates), nil
}

func (c *epicConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	wanted := stateNameSet(states)
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[normalizeState(issue.State)]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *epicConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *epicConnector) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	wanted := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		wanted[strings.ToLower(strings.TrimSpace(identifier))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.resolved))
	for _, issue := range c.resolved {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.Identifier))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *epicConnector) FetchIssueChildren(_ context.Context, issueID string) ([]connector.BlockedRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.linked != nil {
		return append([]connector.BlockedRef(nil), c.linked[issueID]...), nil
	}
	for _, issue := range append(append([]connector.Issue(nil), c.candidates...), c.stateIssues...) {
		if issue.ID == issueID {
			return append([]connector.BlockedRef(nil), issue.ChildIssues...), nil
		}
	}
	return []connector.BlockedRef{}, nil
}

func (c *epicConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments = append(c.comments, body)
	return nil
}

func (c *epicConnector) CloseIssue(_ context.Context, issueID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, issueID)
	return nil
}

func (c *epicConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, epicStateUpdate{issueID: issueID, state: state})
	return nil
}

func (c *epicConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *epicConnector) SetField(context.Context, string, string, string) error {
	return nil
}

func (c *epicConnector) stateUpdates() []epicStateUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]epicStateUpdate(nil), c.updates...)
}

func (c *epicConnector) closedIssues() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closed...)
}

func (c *epicConnector) commentBodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.comments...)
}

func epicTestIssue(id string, state string, closed bool, title string, labels []string, description string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#" + strings.TrimPrefix(id, "child-")
	if strings.HasPrefix(id, "epic-") {
		issue.Identifier = "digitaldrywood/detent#" + strings.TrimPrefix(id, "epic-")
	}
	issue.Title = title
	issue.State = state
	issue.Closed = closed
	issue.Description = description
	issue.Labels = append([]string(nil), labels...)
	issue.URL = "https://github.com/" + strings.ReplaceAll(issue.Identifier, "#", "/issues/")
	return issue
}
