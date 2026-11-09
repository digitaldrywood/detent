package hubserver

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIntakeFixture(t *testing.T) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "health-intake")
	addHealthBacklog(t, f)
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true})
	for i := range states {
		if states[i].Name == "Todo" {
			states[i].Transitions = append(states[i].Transitions, "Backlog")
		}
	}
	setHealthProjectStates(t, f, states)
	return f
}
