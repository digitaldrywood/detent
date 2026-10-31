package connector

import "testing"

func TestCompletionLane(t *testing.T) {
	t.Parallel()
	hosted := []WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	review := []WorkflowState{
		{Name: "Backlog", Transitions: []string{"Todo"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Backlog"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Blocked", "Todo", "Done"}},
		{Name: "In Review", Transitions: []string{"Merging", "In Progress"}},
		{Name: "Blocked", Transitions: []string{"Todo"}},
		{Name: "Merging", Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}
	fourLanes := []WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}
	operatorDone := []WorkflowState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Blocked", "Done"}},
		{Name: "In Review", Transitions: []string{"In Progress"}},
		{Name: "Blocked", Transitions: []string{"In Progress"}},
		{Name: "Done", Terminal: true, OperatorOnly: true},
	}
	operatorReview := []WorkflowState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Todo"}},
		{Name: "In Review", OperatorOnly: true},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
	}
	for _, test := range []struct {
		name    string
		states  []WorkflowState
		current string
		review  string
		refusal bool
		changed bool
		want    string
		wantOK  bool
	}{
		{name: "four lane completion ends accepted unchanged work", states: fourLanes, current: "In Progress", want: "Done", wantOK: true},
		{name: "four lane completion respects configured review", states: fourLanes, current: "In Progress", review: "Human Review", changed: true, want: "Human Review", wantOK: true},
		{name: "four lane refusal falls back from missing Blocked", states: fourLanes, current: "In Progress", review: "Blocked", refusal: true, want: "Human Review", wantOK: true},
		{name: "refusal prefers configured review before earlier parking lane", states: review, current: "In Progress", review: "In Review", refusal: true, want: "In Review", wantOK: true},
		{name: "refusal skips operator-only review and dispatchable fallback", states: operatorReview, current: "In Progress", review: "In Review", refusal: true},
		{name: "refusal never ends work", states: hosted, current: "In Progress", review: "Done", refusal: true},
		{name: "change goes to the configured review lane", states: review, current: "In Progress", review: "In Review", changed: true, want: "In Review", wantOK: true},
		{name: "review lane is matched loosely", states: review, current: " in progress ", review: "in review", changed: true, want: "In Review", wantOK: true},
		{name: "an earlier parking lane is not review", states: review, current: "Todo", review: "In Review", changed: true},
		{name: "change never ends the work", states: hosted, current: "In Progress", review: "Human Review", changed: true},
		{name: "a terminal lane named as review is not review", states: hosted, current: "In Progress", review: "Done", changed: true},
		{name: "operator-only review is never chosen", states: operatorReview, current: "In Progress", review: "In Review", changed: true},
		{name: "no change ends the work", states: review, current: "In Progress", review: "In Review", want: "Done", wantOK: true},
		{name: "no change in the hosted workflow ends the work", states: hosted, current: "In Progress", want: "Done", wantOK: true},
		{name: "no change never parks in a non-terminal lane", states: operatorDone, current: "In Progress", review: "In Review"},
		{name: "a terminal lane has nowhere to go", states: review, current: "Done", review: "In Review"},
		{name: "unknown lane", states: review, current: "Rework", review: "In Review", changed: true},
		{name: "no workflow", current: "In Progress", review: "In Review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CompletionLane(test.states, test.current, test.review, test.changed)
			if test.refusal {
				got, ok = LandingRefusalLane(test.states, test.current, test.review, false)
			}
			if got != test.want || ok != test.wantOK {
				t.Fatalf("CompletionLane = %q, %t; want %q, %t", got, ok, test.want, test.wantOK)
			}
		})
	}
}
