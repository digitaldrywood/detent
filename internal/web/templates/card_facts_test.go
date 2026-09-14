package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardCardFacts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging"} {
		t.Run(lane, func(t *testing.T) {
			count := int64(75)
			issue := telemetry.Issue{ID: "i", ProjectID: "p", State: lane, AttemptsToday: &count, LaneReason: "dispatch_loop_detected", LaneReasonAt: &at}
			data := DashboardData{Snapshot: telemetry.Snapshot{GeneratedAt: at, BoardIssues: []telemetry.Issue{issue}}}
			card := projectKanbanCard{IssueID: "i", ProjectID: "p", Stage: lane}
			facts := boardCardFacts(data, card)
			if len(facts) != 5 || facts[0].Text != "no PR" || !strings.Contains(facts[3].Text, "no session") || !strings.Contains(facts[4].Detail, "dispatch_loop_detected · 2026-09-14T12:00:00Z") {
				t.Fatalf("facts = %+v", facts)
			}
			if !strings.Contains(cardFactDetail(facts), "75 today") {
				t.Fatalf("detail = %s", cardFactDetail(facts))
			}
		})
	}
}

func TestCardFactCompact(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, text, want string }{{"push", "push 2h", "↑2h"}, {"ci", "CI queued", "◷"}, {"ci", "CI running", "↻"}, {"ci", "CI green", "✓"}, {"ci", "CI red", "✕"}, {"ci", "CI skipped", "–"}, {"session", "30m · 12345 tok · 75 today", "30m 12345t ×75"}, {"session", "no session · 0 today", "no session ×0"}, {"reason", "merge_conflict", "merge_conflict"}} {
		t.Run(tt.text, func(t *testing.T) {
			fact := cardFactView{Name: tt.name, Text: tt.text}
			if got := cardFactCompact(fact); got != tt.want {
				t.Fatalf("compact = %q, want %q", got, tt.want)
			}
			if cardFactTitle(fact) != tt.text {
				t.Fatal("full evidence missing from title")
			}
		})
	}
}

func TestBoardCardFactsCustomLane(t *testing.T) {
	for _, tt := range []struct {
		lane string
		want bool
	}{{"Production", true}, {"Rework", true}, {"Merging", true}, {"Done", false}} {
		t.Run(tt.lane, func(t *testing.T) {
			data := DashboardData{Kanban: KanbanData{Projects: map[string]KanbanProjectData{"p": {ProjectID: "p", ActiveStates: []string{"Production"}}}}}
			facts := boardCardFacts(data, projectKanbanCard{ProjectID: "p", Stage: tt.lane})
			if (len(facts) > 0) != tt.want {
				t.Fatalf("facts = %+v", facts)
			}
		})
	}
}
