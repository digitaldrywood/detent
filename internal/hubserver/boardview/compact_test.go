package boardview

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func compactTestView(perLane map[string]int) View {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	project := Project{Project: tracker.NativeProject{ID: "prj_example", Name: "Example", Slug: "example", States: []tracker.NativeState{
		{Name: "Todo", Dispatchable: true}, {Name: "Backlog"}, {Name: "Done", Terminal: true},
	}}, Work: &tracker.NativeWorkSummary{Lanes: []tracker.NativeWorkLane{{State: "Todo", Total: perLane["Todo"]}, {State: "Backlog", Total: perLane["Backlog"]}, {State: "Done", Total: 40}}, AsOf: now}}
	view := View{Projects: []Project{project}, Running: map[tracker.NativeWorkItemID]bool{}}
	number := 0
	for _, state := range []string{"Todo", "Backlog"} {
		for range perLane[state] {
			number++
			view.Cards = append(view.Cards, tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", number)), Number: number},
				Title: strings.Repeat("t", 200), State: state, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}})
		}
	}
	return view
}

func TestCompactKeepsEveryLaneWithinBudget(t *testing.T) {
	lanes := Lanes{Preset: viewstore.LanesOpen, Include: []string{}, Exclude: []string{}}
	for _, test := range []struct {
		name     string
		perLane  map[string]int
		budget   int
		cut      bool
		wantLane map[string]bool
	}{
		{name: "fits", perLane: map[string]int{"Todo": 3, "Backlog": 2}, budget: 1 << 20},
		{name: "cut", perLane: map[string]int{"Todo": 40, "Backlog": 400}, budget: 24 << 10, cut: true, wantLane: map[string]bool{"Todo": true, "Backlog": true}},
		{name: "one tiny lane", perLane: map[string]int{"Todo": 1, "Backlog": 400}, budget: 16 << 10, cut: true, wantLane: map[string]bool{"Backlog": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := compactTestView(test.perLane)
			board, cursors, err := Compact("org_example", time.Time{}, view, nil, lanes, nil, test.budget)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(board)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > test.budget {
				t.Fatalf("result %d bytes exceeds budget %d", len(raw), test.budget)
			}
			returned := map[string]int{}
			for _, card := range board.Cards {
				returned[card.State]++
			}
			for state, total := range test.perLane {
				if returned[state] == 0 {
					t.Fatalf("lane %s returned no cards: %v", state, returned)
				}
				if !test.cut && returned[state] != total {
					t.Fatalf("lane %s returned %d of %d without a cut", state, returned[state], total)
				}
			}
			if board.Truncated != test.cut {
				t.Fatalf("truncated=%v want %v", board.Truncated, test.cut)
			}
			more := map[string]bool{}
			for _, cursor := range cursors {
				more[cursor.State] = cursor.More
				var last CompactCard
				for _, card := range board.Cards {
					if card.State == cursor.State {
						last = card
					}
				}
				if cursor.Identifier != last.Identifier || cursor.Identifier == "" {
					t.Fatalf("cursor %+v does not continue after the last returned card %q", cursor, last.Identifier)
				}
			}
			for state := range test.perLane {
				if more[state] != test.wantLane[state] {
					t.Fatalf("lane %s continuation=%v want %v", state, more[state], test.wantLane[state])
				}
			}
			for _, lane := range board.Projects[0].Lanes {
				if lane.State == "Done" && (lane.Selected || lane.Total != 40 || lane.Returned != 0) {
					t.Fatalf("excluded terminal lane=%+v, want its count without cards", lane)
				}
				if lane.State != "Done" && (!lane.Selected || lane.Returned != returned[lane.State]) {
					t.Fatalf("selected lane=%+v returned=%d", lane, returned[lane.State])
				}
			}
		})
	}
}

func TestCompactRefusesAPageWithNoRoomForACard(t *testing.T) {
	view := compactTestView(map[string]int{"Todo": 1})
	view.Cards[0].Issue.Title = strings.Repeat("t", 8<<10)
	lanes := Lanes{Preset: viewstore.LanesOpen, Include: []string{"Todo"}, Exclude: []string{}}
	if _, _, err := Compact("org_example", time.Time{}, view, nil, lanes, nil, 6<<10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a page that cannot hold its next card returned %v, want ErrTooLarge so a continuation never repeats", err)
	}
}

func TestLanesSelectByWorkflowFlags(t *testing.T) {
	states := []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}, {Name: "Merging", Dispatchable: true}, {Name: "Done", Terminal: true}}
	for _, test := range []struct {
		lanes Lanes
		want  string
	}{
		{Lanes{Preset: viewstore.LanesOpen}, "Backlog Todo Merging"},
		{Lanes{Preset: viewstore.LanesActive}, "Todo Merging"},
		{Lanes{Preset: viewstore.LanesAll}, "Backlog Todo Merging Done"},
		{Lanes{Preset: viewstore.LanesAll, Include: []string{"Done", "Todo"}}, "Todo Done"},
		{Lanes{Preset: viewstore.LanesOpen, Exclude: []string{"Backlog"}}, "Todo Merging"},
		{Lanes{Preset: viewstore.LanesActive, Include: []string{"Backlog"}}, ""},
	} {
		var selected []string
		for _, state := range states {
			if test.lanes.Selects(state) {
				selected = append(selected, state.Name)
			}
		}
		if got := strings.Join(selected, " "); got != test.want {
			t.Errorf("%+v selected %q want %q", test.lanes, got, test.want)
		}
	}
}

func TestLaneCursorsEncodeOnlyLanesWithMore(t *testing.T) {
	t.Parallel()
	refused := errors.New("cursor key unavailable")
	for _, test := range []struct {
		name      string
		positions []Cursor
		encodeErr error
		want      []Lane
		encoded   []string
		err       error
	}{
		{name: "no positions", want: []Lane{}},
		{name: "complete lanes need no cursor", positions: []Cursor{{State: "Done"}, {State: "Cancelled"}}, want: []Lane{{State: "Done"}, {State: "Cancelled"}}},
		{name: "a lane with more", positions: []Cursor{{State: "Done", More: true, Identifier: "example#9"}, {State: "Cancelled"}}, want: []Lane{{State: "Done", NextCursor: "cursor:Done"}, {State: "Cancelled"}}, encoded: []string{"Done"}},
		{name: "encoding fails", positions: []Cursor{{State: "Done", More: true}}, encodeErr: refused, err: refused, encoded: []string{"Done"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var encoded []string
			lanes, err := LaneCursors(test.positions, func(position Cursor) (string, error) {
				encoded = append(encoded, position.State)
				return "cursor:" + position.State, test.encodeErr
			})
			if !errors.Is(err, test.err) {
				t.Fatalf("err = %v, want %v", err, test.err)
			}
			if fmt.Sprint(encoded) != fmt.Sprint(test.encoded) {
				t.Fatalf("encoded = %v, want %v", encoded, test.encoded)
			}
			if test.err == nil && fmt.Sprint(lanes) != fmt.Sprint(test.want) {
				t.Fatalf("lanes = %v, want %v", lanes, test.want)
			}
		})
	}
}
