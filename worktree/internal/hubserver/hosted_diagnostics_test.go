package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiagnosticsTodoHistory(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	states := map[string]tracker.NativeState{"Todo": {Name: "Todo", Dispatchable: true}, "In Progress": {Name: "In Progress", Dispatchable: true}}
	issue := analyticsResidenceIssue{created: at.Add(-time.Hour), initial: "Todo", events: []analyticsResidenceEvent{{at: at, kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}}}
	for _, test := range []struct {
		name        string
		at          time.Time
		initial     string
		partial     bool
		clipped     bool
		want        int
		unavailable bool
	}{
		{name: "not yet created", at: at.Add(-2 * time.Hour), initial: "Todo", want: 0},
		{name: "future dispatch cannot change past queue", at: at.Add(-time.Minute), initial: "Todo", want: 1},
		{name: "exclusive bucket end", at: at, initial: "Todo", want: 1},
		{name: "dispatch leaves queue", at: at.Add(time.Minute), initial: "Todo", want: 0},
		{name: "missing initial history", at: at, unavailable: true},
		{name: "incomplete history", at: at, initial: "Todo", partial: true, unavailable: true},
		{name: "clipped population", at: at, initial: "Todo", clipped: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := issue
			input.initial, input.partial = test.initial, test.partial
			got := diagnosticsTodoDepth([]analyticsResidenceIssue{input}, states, test.clipped, test.at)
			if test.unavailable {
				if got != nil {
					t.Fatalf("queue depth = %d, want unavailable", *got)
				}
				return
			}
			if got == nil || *got != test.want {
				t.Fatalf("queue depth = %v, want %d", got, test.want)
			}
		})
	}
}
