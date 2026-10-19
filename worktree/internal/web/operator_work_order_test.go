package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/displayorder/testfixture"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestSnapshotWorkListOrder(t *testing.T) {
	for _, test := range testfixture.Comparisons() {
		t.Run(test.Name, func(t *testing.T) {
			state := "Todo"
			if test.Terminal {
				state = "Done"
			}
			issues := []telemetry.Issue{
				{ID: "left", State: state, Priority: test.Left.Priority, UpdatedAt: &test.Left.LastActivityAt, Identifier: test.Left.Identifier},
				{ID: "right", State: state, Priority: test.Right.Priority, UpdatedAt: &test.Right.LastActivityAt, Identifier: test.Right.Identifier},
			}
			sortWorkIssues(issues, []string{"Todo", "Done"}, []string{"Done"})
			want := "left"
			if test.Want > 0 {
				want = "right"
			}
			if issues[0].ID != want {
				t.Fatalf("first = %s, want %s", issues[0].ID, want)
			}
		})
	}
	t.Run("configured lanes and unknown states", func(t *testing.T) {
		now := time.Now()
		issues := []telemetry.Issue{
			{State: "Unknown Z", Identifier: "1"},
			{State: "Done", Identifier: "2", UpdatedAt: &now},
			{State: "Todo", Identifier: "3"},
			{State: "Unknown A", Identifier: "4"},
			{State: "Backlog", Identifier: "5"},
		}
		sortWorkIssues(issues, []string{"Backlog", "Todo", "Done"}, []string{"Done"})
		for index, want := range []string{"5", "3", "2", "4", "1"} {
			if issues[index].Identifier != want {
				t.Fatalf("item %d = %s, want %s", index, issues[index].Identifier, want)
			}
		}
	})
}
