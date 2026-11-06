package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestCreationOriginCards(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"operator", "routine", "lesson", "worker", "doctor", "audit"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			body := strings.Repeat("Description ", 30)
			if kind != "operator" {
				body = issueorigin.Stamp(body, issueorigin.Origin{Kind: kind, Instance: "instance", Source: "run", Fingerprint: "problem"})
			}
			data := DashboardData{}
			card := projectKanbanCardForIssue(data, telemetry.Issue{ID: "I_1", Identifier: "example/repo#1", Title: "Finding", Description: body}, "Backlog", time.Time{}, time.Now())
			view := boardCardViewFromCard(data, projectKanbanLane{}, card, false, "", "")
			if view.CreationOrigin != kind || workItemDataAttributes(view.Work, "board")["data-work-origin"] != kind {
				t.Fatalf("origin = %s, metadata = %+v", view.CreationOrigin, view.Work)
			}
			var out bytes.Buffer
			if err := boardCardView2(view).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `data-board-creation-origin="`+kind+`"`) {
				t.Fatalf("origin marker missing: %s", out.String())
			}
		})
	}
}
