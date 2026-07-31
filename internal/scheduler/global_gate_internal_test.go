package scheduler

import "testing"

func TestGlobalDispatchGateSetProjectsCleansCycleRecords(t *testing.T) {
	t.Parallel()

	configured := ProjectCandidate{ID: "configured", Weight: 1}
	orphan := ProjectCandidate{ID: "orphan", Weight: 1}
	for _, tt := range []struct {
		name      string
		cycle     ProjectCandidate
		wantCycle bool
	}{
		{name: "configured project cycle remains", cycle: configured, wantCycle: true},
		{name: "cycle-only orphan is removed", cycle: orphan},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gate := NewGlobalDispatchGate(
				NewStrictPriority(Config{Capacity: 1}),
				configured,
			)
			gate.MarkIdle(tt.cycle)
			gate.SetProjects([]ProjectCandidate{configured})

			cycle, ok := gate.projectCycles[tt.cycle.ID]
			if ok != tt.wantCycle {
				t.Fatalf("projectCycles[%q] present = %t, want %t", tt.cycle.ID, ok, tt.wantCycle)
			}
			if ok && !cycle.idle {
				t.Fatalf("projectCycles[%q].idle = false, want true", tt.cycle.ID)
			}
		})
	}
}
