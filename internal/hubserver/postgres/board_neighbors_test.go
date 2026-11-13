package postgres

import (
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
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

func TestBoardRelationshipsOnly(t *testing.T) {
	t.Parallel()
	record := func(title, blockerState string, payload string) *viewstore.BoardRecord {
		issue := &tracker.NativeIssue{Title: title, Blockers: []tracker.NativeDependency{{ID: "wi_blocker", ProjectID: "prj_other", State: blockerState, Terminal: blockerState == "Done"}}, DependencyReady: blockerState == "Done"}
		list := *issue
		listBytes := 100
		if blockerState == "Done" {
			listBytes = 120
		}
		return &viewstore.BoardRecord{Position: viewstore.BoardPosition{State: "Todo"}, Display: viewstore.BoardDisplay{Title: title}, PageIssue: issue, ListIssue: &list, ListBytes: listBytes, Payload: attachment.ArchiveReference{Key: payload}, ReporterRelations: &viewstore.BoardReporterRelations{ReporterStatus: "planned", Blockers: slices.Clone(issue.Blockers)}}
	}
	for _, test := range []struct {
		name           string
		previous, next *viewstore.BoardRecord
		want           bool
	}{
		{"neighbor chip changed", record("Dependent", "Todo", "first"), record("Dependent", "Done", "second"), true},
		{"own title changed with chip", record("Dependent", "Todo", "first"), record("Renamed", "Done", "second"), false},
		{"own title changed", record("Dependent", "Todo", "first"), record("Renamed", "Todo", "second"), false},
		{"first projection", nil, record("Dependent", "Todo", "first"), false},
		{"deleted", record("Dependent", "Todo", "first"), nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := boardRelationshipsOnly(test.previous, test.next); got != test.want {
				t.Fatalf("relationships only = %v, want %v", got, test.want)
			}
		})
	}
}
