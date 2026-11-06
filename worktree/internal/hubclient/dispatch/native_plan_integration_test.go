package dispatch

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

func hubserverPlanStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
		{Name: "Done", Terminal: true},
	}
}
