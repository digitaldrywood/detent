package postgres

import (
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangedBoardNeighbors(t *testing.T) {
	t.Parallel()
	record := func(state string, blockers ...tracker.NativeWorkItemID) *viewstore.BoardRecord {
		issue := &tracker.NativeIssue{}
		for _, id := range blockers {
			issue.Blockers = append(issue.Blockers, tracker.NativeDependency{ID: id, ProjectID: "prj_example"})
		}
		return &viewstore.BoardRecord{Position: viewstore.BoardPosition{State: state}, Display: viewstore.BoardDisplay{Title: "Dependent"}, PageIssue: issue}
	}
	neighbor := func(id tracker.NativeWorkItemID) boardNeighbor {
		return boardNeighbor{project: "prj_example", item: id}
	}
	for _, test := range []struct {
		name           string
		previous, next *viewstore.BoardRecord
		want           []boardNeighbor
	}{
		{"dependency added", record("Todo"), record("Todo", "wi_blocker"), []boardNeighbor{neighbor("wi_blocker")}},
		{"dependency removed", record("Todo", "wi_blocker"), record("Todo"), []boardNeighbor{neighbor("wi_blocker")}},
		{"state changed", record("Todo", "wi_blocker", "wi_other"), record("Done", "wi_blocker", "wi_other"), []boardNeighbor{neighbor("wi_blocker"), neighbor("wi_other")}},
		{"first projection", nil, record("Todo", "wi_blocker"), []boardNeighbor{neighbor("wi_blocker")}},
		{"deleted", record("Todo", "wi_blocker"), nil, []boardNeighbor{neighbor("wi_blocker")}},
		{"unrelated change", record("Todo", "wi_blocker"), record("Todo", "wi_blocker"), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := changedBoardNeighbors(test.previous, test.next); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("changed neighbors = %+v, want %+v", got, test.want)
			}
		})
	}
}
