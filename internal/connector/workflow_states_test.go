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
		changed bool
		want    string
		wantOK  bool
	}{
		{name: "change waits for review", states: review, current: "In Progress", changed: true, want: "In Review", wantOK: true},
		{name: "no change ends the work", states: review, current: "In Progress", want: "Done", wantOK: true},
		{name: "change without a review lane ends the work", states: hosted, current: "In Progress", changed: true, want: "Done", wantOK: true},
		{name: "no change in the hosted workflow ends the work", states: hosted, current: "In Progress", want: "Done", wantOK: true},
		{name: "lane names are matched loosely", states: hosted, current: " in progress ", want: "Done", wantOK: true},
		{name: "operator-only end parks an unchanged run", states: operatorDone, current: "In Progress", want: "In Review", wantOK: true},
		{name: "operator-only review is never chosen", states: operatorReview, current: "In Progress", changed: true},
		{name: "first non-dispatchable successor in workflow order", states: review, current: "Todo", changed: true, want: "Blocked", wantOK: true},
		{name: "a terminal lane has nowhere to go", states: review, current: "Done"},
		{name: "unknown lane", states: review, current: "Rework", changed: true},
		{name: "no workflow", current: "In Progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CompletionLane(test.states, test.current, test.changed)
			if got != test.want || ok != test.wantOK {
				t.Fatalf("CompletionLane = %q, %t; want %q, %t", got, ok, test.want, test.wantOK)
			}
		})
	}
}
