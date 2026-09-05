package templates

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardHumanDependencyEvidence(t *testing.T) {
	t.Parallel()
	for _, ready := range []bool{false, true} {
		ref := telemetry.BlockedRef{Identifier: "owner/repo#10", State: "Done", TrackerState: "closed", HumanOwned: true, HumanCompletionReady: ready}
		active, cleared := projectKanbanBlockerLabels([]telemetry.BlockedRef{ref}, map[string]struct{}{"done": {}}, "Todo")
		if ready {
			if len(active) != 0 || len(cleared) != 1 || !strings.Contains(cleared[0], "completion evidence recorded") {
				t.Fatalf("ready labels = %v %v", active, cleared)
			}
		} else if len(active) != 1 || len(cleared) != 0 || !strings.Contains(active[0], "human prerequisite owner/repo#10") {
			t.Fatalf("waiting labels = %v %v", active, cleared)
		}
		if projectKanbanBlockedRefCleared(ref, map[string]struct{}{"done": {}}) != ready {
			t.Fatal("board readiness ignored human evidence")
		}
	}
}

func TestBoardHumanWaitAndFailurePriority(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, park, want string
		ready            bool
	}{
		{name: "human wait", want: "Waiting · 1"},
		{name: "independent failure", park: "repeated_failure_circuit_breaker", want: "Needs review"},
		{name: "completed human", ready: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs := []telemetry.BlockedRef{{Identifier: "owner/repo#10", HumanOwned: true, HumanCompletionReady: tt.ready}}
			card := projectKanbanCard{HumanDependencyWait: projectKanbanHumanDependencyWait(refs), BlockedReason: tt.park}
			if !tt.ready {
				card.Blockers = []string{"human prerequisite owner/repo#10"}
			}
			signals := boardCardSignals(boardCardView{State: "Todo"}, card)
			if tt.want == "" {
				if len(signals) != 0 || card.HumanDependencyWait != "" {
					t.Fatalf("completed prerequisite still waiting: %+v", signals)
				}
			} else if len(signals) == 0 || signals[0].Text != tt.want {
				t.Fatalf("signals = %+v, want %s", signals, tt.want)
			}
		})
	}
}
