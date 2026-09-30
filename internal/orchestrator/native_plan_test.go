package orchestrator

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

// These cases catch accepting a plan without review evidence, losing the P1
// rework route, and dispatching a planner into an incompatible native workflow.
func TestNativePlanWorkflowHandoff(t *testing.T) {
	t.Parallel()
	workflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Rework"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review", "Blocked"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Human Review"}},
		{Name: "Human Review"}, {Name: "Blocked"},
	}
	approved := "Plan.\n\n## Detent Plan Review\n\n- state: approved"
	for _, test := range []struct {
		name        string
		output      string
		review      string
		states      []connector.WorkflowState
		stop        string
		humanReview bool
		labels      []string
		want        []string
		invalid     bool
	}{
		{name: "automated approval without Plan Review lane", output: approved, want: []string{"In Progress"}},
		{name: "P1 requests another planner", output: "## Detent Plan Review\n\n- state: P1\n\nMissing tests.", want: []string{"Rework"}},
		{name: "missing review remains under review", output: "Plan only.", humanReview: true, want: []string{"In Progress", "Human Review"}},
		{name: "fenced approval example cannot approve", output: "```markdown\n## Detent Plan Review\n- state: approved\n```", want: []string{"In Progress", "Blocked"}},
		{name: "human policy does not consume automated approval", output: approved, review: gate.PlanReviewHuman, humanReview: true, want: []string{"In Progress", "Human Review"}},
		{name: "both permits automated approval", output: approved, review: gate.PlanReviewBoth, want: []string{"In Progress"}},
		{name: "human approval consumes configured label", review: gate.PlanReviewHuman, labels: []string{"plan-approved"}, want: []string{"In Progress"}},
		{name: "missing review destination rejects before dispatch", states: workflow[:3], output: approved, invalid: true},
		{name: "implementation must accept dispatch", states: []connector.WorkflowState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}}, {Name: "In Progress", Transitions: []string{"Blocked"}}, {Name: "Blocked"}}, invalid: true},
		{name: "review stop cannot dispatch unapproved work", stop: "In Progress", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			states := test.states
			if states == nil {
				states = workflow
			}
			cfg := normalizeConfig(Config{Plan: gate.PlanConfig{Enabled: true, Review: test.review}, ActiveStates: []string{"Todo", "In Progress", "Rework"}})
			if test.review == "" {
				cfg.Plan.Review = gate.PlanReviewAutomated
			}
			if test.stop != "" {
				cfg.Plan.Stop = test.stop
			}
			cfg.AutoPromote.HumanReview = &test.humanReview
			cfg.AutoPromote.SourceState = "Human Review"
			tracker := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, states: states}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			issue := completionTransitionIssue("Todo", "")
			issue.Labels = test.labels
			if _, err := orch.nativePlanLanes(t.Context(), issue); (err != nil) != test.invalid {
				t.Fatalf("workflow validation = %v", err)
			}
			if test.invalid {
				return
			}
			targets, err := orch.nativePlanTarget(t.Context(), issue, test.output)
			if err != nil || !slices.Equal(targets, test.want) {
				t.Fatalf("handoff = %v, %v, want %v", targets, err, test.want)
			}
		})
	}
}
