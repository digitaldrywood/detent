package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardHumanWaitLanes(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Human Review", "Blocked"} {
		t.Run(lane, func(t *testing.T) {
			reason := "Where should this defect go? (waiting 10h0m0s)"
			issue := telemetry.Issue{ID: "2159", Identifier: "owner/repo#2159", Title: "Disposition", State: lane}
			data := DashboardData{Snapshot: telemetry.Snapshot{GeneratedAt: time.Now(), LastKnown: true, Blocked: []telemetry.Blocked{{Issue: issue, Error: reason, RecoveryRemedy: reason, NeedsHumanAttention: true, Source: telemetry.BlockedSourceProjectStatus}}}, Kanban: KanbanData{States: []string{"Todo", "Rework", "Human Review", "Blocked"}}}
			board := projectKanbanBoardView(data)
			found := false
			for _, column := range board.AllLanes {
				for _, card := range column.Cards {
					if card.IssueID != issue.ID {
						continue
					}
					found = true
					if column.Title != lane || card.BlockedReason != reason {
						t.Fatalf("card lane/reason = %s / %s", column.Title, card.BlockedReason)
					}
					view := boardCardViewFromCard(data, column, card, false, "fleet", "")
					html := renderBoardComponent(t, boardCardView2(view))
					if !strings.Contains(html, reason) {
						t.Fatalf("card omits human wait reason: %s", html)
					}
				}
			}
			if !found {
				t.Fatal("wait card missing")
			}
		})
	}
}
