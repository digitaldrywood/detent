package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// workflowStateConnector is an autoPromoteTickConnector that also reports the
// project's own lanes, the way the native Hub connector does.
type workflowStateConnector struct {
	*autoPromoteTickConnector
	states []connector.WorkflowState
	err    error
	calls  int
}

func (c *workflowStateConnector) ListWorkflowStates(context.Context) ([]connector.WorkflowState, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return append([]connector.WorkflowState(nil), c.states...), nil
}

func threeStateWorkflow() []connector.WorkflowState {
	return []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true},
		{Name: "In Progress", Dispatchable: true},
		{Name: "Done", Terminal: true},
	}
}

func TestCompletedActiveReviewFallbackState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		states []connector.WorkflowState
		want   string
	}{
		{
			name:   "three state project falls back to its terminal lane",
			states: threeStateWorkflow(),
			want:   "Done",
		},
		{
			name: "a review lane is preferred over the terminal lane",
			states: []connector.WorkflowState{
				{Name: "Todo", Dispatchable: true},
				{Name: "In Progress", Dispatchable: true},
				{Name: "Code Review"},
				{Name: "Done", Terminal: true},
			},
			want: "Code Review",
		},
		{
			name: "a parking lane that is not a review lane is not used",
			states: []connector.WorkflowState{
				{Name: "Backlog"},
				{Name: "Todo", Dispatchable: true},
				{Name: "Blocked"},
				{Name: "Shipped", Terminal: true},
			},
			want: "Shipped",
		},
		{
			name: "an operator-only review lane is skipped",
			states: []connector.WorkflowState{
				{Name: "Todo", Dispatchable: true},
				{Name: "Manual Review", OperatorOnly: true},
				{Name: "Done", Terminal: true},
			},
			want: "Done",
		},
		{
			name: "a dispatchable lane named review is not a parking lane",
			states: []connector.WorkflowState{
				{Name: "Todo", Dispatchable: true},
				{Name: "Review Queue", Dispatchable: true},
				{Name: "Closed", Terminal: true},
			},
			want: "Closed",
		},
		{
			name: "the first terminal lane wins when there are several",
			states: []connector.WorkflowState{
				{Name: "Todo", Dispatchable: true},
				{Name: "Done", Terminal: true},
				{Name: "Cancelled", Terminal: true},
			},
			want: "Done",
		},
		{
			name: "a workflow with nowhere to park has no fallback",
			states: []connector.WorkflowState{
				{Name: "Todo", Dispatchable: true},
				{Name: "In Progress", Dispatchable: true},
			},
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := completedActiveReviewFallbackState(test.states); got != test.want {
				t.Fatalf("completedActiveReviewFallbackState() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveCompletedActiveReviewTarget(t *testing.T) {
	t.Parallel()

	humanReviewWorkflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true},
		{Name: "In Progress", Dispatchable: true},
		{Name: "Human Review"},
		{Name: "Merging"},
		{Name: "Done", Terminal: true},
	}

	tests := []struct {
		name            string
		states          []connector.WorkflowState
		known           bool
		configured      string
		want            string
		wantSubstituted bool
	}{
		{
			name:       "the configured review lane is used when the project has it",
			states:     humanReviewWorkflow,
			known:      true,
			configured: autoPromoteSourceState,
			want:       autoPromoteSourceState,
		},
		{
			name:       "a non-terminal review lane the config names is used",
			states:     []connector.WorkflowState{{Name: "Todo", Dispatchable: true}, {Name: "Needs Review"}, {Name: "Done", Terminal: true}},
			known:      true,
			configured: "Needs Review",
			want:       "Needs Review",
		},
		{
			name:            "a three state project promotes into its terminal lane",
			states:          threeStateWorkflow(),
			known:           true,
			configured:      autoPromoteSourceState,
			want:            "Done",
			wantSubstituted: true,
		},
		{
			name:            "a project with its own review lane promotes into that",
			states:          []connector.WorkflowState{{Name: "Todo", Dispatchable: true}, {Name: "Peer Review"}, {Name: "Done", Terminal: true}},
			known:           true,
			configured:      autoPromoteSourceState,
			want:            "Peer Review",
			wantSubstituted: true,
		},
		{
			name:       "an unknown workflow leaves the configured lane alone",
			states:     nil,
			known:      false,
			configured: autoPromoteSourceState,
			want:       autoPromoteSourceState,
		},
		{
			name:       "a workflow with nowhere to promote into yields no target",
			states:     []connector.WorkflowState{{Name: "Todo", Dispatchable: true}, {Name: "In Progress", Dispatchable: true}},
			known:      true,
			configured: autoPromoteSourceState,
			want:       "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			orch := &Orchestrator{}
			issue := completionTransitionIssue("In Progress", "OPEN")
			got, substituted := orch.resolveCompletedActiveReviewTarget(test.states, test.known, issue, test.configured)
			if got != test.want || substituted != test.wantSubstituted {
				t.Fatalf("resolveCompletedActiveReviewTarget() = (%q, %t), want (%q, %t)", got, substituted, test.want, test.wantSubstituted)
			}
		})
	}
}

// TestTransitionCompletedActiveIssuesPromotesIntoTerminalLaneWithoutReview is
// the dogfood loop: a three-state project, an attempt that succeeded, and an
// item that used to stay In Progress forever because the configured review
// lane does not exist (operations.md section 7).
func TestTransitionCompletedActiveIssuesPromotesIntoTerminalLaneWithoutReview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	tracker := &workflowStateConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
		states:                   threeStateWorkflow(),
	}
	cfg := normalizeConfig(Config{
		AutoPromote:    AutoPromoteConfig{},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want the completed issue", result.transitioned)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
		t.Fatalf("updates = %#v, want one move to Done", tracker.updates)
	}
	if tracker.calls != 1 {
		t.Fatalf("workflow state reads = %d, want one per tick", tracker.calls)
	}
}

// TestTransitionCompletedActiveIssuesKeepsConfiguredReviewLane proves the
// fallback is inert on a project that has the configured lane: the promotion
// is the one it always was, auto-promote included.
func TestTransitionCompletedActiveIssuesKeepsConfiguredReviewLane(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	tracker := &workflowStateConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
		states: []connector.WorkflowState{
			{Name: "Todo", Dispatchable: true},
			{Name: "In Progress", Dispatchable: true},
			{Name: autoPromoteSourceState},
			{Name: "Done", Terminal: true},
		},
	}
	cfg := normalizeConfig(Config{
		AutoPromote:    AutoPromoteConfig{},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if len(tracker.updates) != 1 || tracker.updates[0].state != autoPromoteSourceState {
		t.Fatalf("updates = %#v, want one move to %s", tracker.updates, autoPromoteSourceState)
	}
}

// TestTransitionCompletedActiveIssuesKeepsConfiguredLaneWhenWorkflowUnknown
// pins the behavior of every connector that cannot report a workflow: the
// configured lane is still the target, exactly as before.
func TestTransitionCompletedActiveIssuesKeepsConfiguredLaneWhenWorkflowUnknown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	cfg := normalizeConfig(Config{
		AutoPromote:    AutoPromoteConfig{},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})

	for _, test := range []struct {
		name      string
		connector connector.Connector
	}{
		{name: "connector reports no workflow", connector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}},
		{name: "workflow read fails", connector: &workflowStateConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
			err:                      errors.New("hub unavailable"),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			orch := &Orchestrator{cfg: cfg, connector: test.connector}
			state := newState(cfg)
			state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

			orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

			var updates []autoPromoteTickUpdate
			switch tracker := test.connector.(type) {
			case *autoPromoteTickConnector:
				updates = tracker.updates
			case *workflowStateConnector:
				updates = tracker.updates
			}
			if len(updates) != 1 || updates[0].state != autoPromoteSourceState {
				t.Fatalf("updates = %#v, want one move to %s", updates, autoPromoteSourceState)
			}
		})
	}
}

// TestLogCompletedActiveReviewFallbackOncePerItem pins "once per item": a
// project that simply has no such lane must not repeat the line every tick.
func TestLogCompletedActiveReviewFallbackOncePerItem(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	orch := &Orchestrator{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	issue := completionTransitionIssue("In Progress", "")
	const line = "completed issue promotion lane substituted"

	for range 3 {
		orch.logCompletedActiveReviewFallback(issue, autoPromoteSourceState, "Done")
	}
	if got := strings.Count(logs.String(), line); got != 1 {
		t.Fatalf("log records = %d, want one per item: %s", got, logs.String())
	}

	// A different destination for the same item is a different fact.
	orch.logCompletedActiveReviewFallback(issue, autoPromoteSourceState, "Cancelled")
	if got := strings.Count(logs.String(), line); got != 2 {
		t.Fatalf("log records = %d, want a second line for a changed target: %s", got, logs.String())
	}

	other := completionTransitionIssue("In Progress", "")
	other.ID = "issue-2"
	orch.logCompletedActiveReviewFallback(other, autoPromoteSourceState, "Done")
	if got := strings.Count(logs.String(), line); got != 3 {
		t.Fatalf("log records = %d, want one line per item: %s", got, logs.String())
	}

	// A project with nowhere to promote into says so, once, at warn.
	orch.logCompletedActiveReviewFallback(other, autoPromoteSourceState, "")
	orch.logCompletedActiveReviewFallback(other, autoPromoteSourceState, "")
	if got := strings.Count(logs.String(), "completed issue has no promotion lane"); got != 1 {
		t.Fatalf("missing lane records = %d, want one: %s", got, logs.String())
	}

	// An item that is no longer waiting to be promoted is forgotten, so it
	// neither leaks an entry nor silences its next promotion.
	state := newState(normalizeConfig(Config{}))
	state.Completed[issue.ID] = Completed{Issue: issue}
	orch.forgetPromotionFallbackLogs(&state)
	if _, kept := orch.promotionFallbackLogged[other.ID]; kept {
		t.Fatalf("promotionFallbackLogged kept %s after it left Completed", other.ID)
	}
	if _, kept := orch.promotionFallbackLogged[issue.ID]; !kept {
		t.Fatalf("promotionFallbackLogged dropped %s while it is still waiting", issue.ID)
	}
	orch.logCompletedActiveReviewFallback(other, autoPromoteSourceState, "Done")
	if got := strings.Count(logs.String(), line); got != 4 {
		t.Fatalf("log records = %d, want a fresh line for a forgotten item: %s", got, logs.String())
	}
}
