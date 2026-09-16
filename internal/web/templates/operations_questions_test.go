package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestQuestionVisibility(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Hour)
	for _, tc := range []struct {
		name         string
		at           *time.Time
		age, summary string
	}{
		{"known", &old, "10h", "Open questions: 1 · Oldest: 10h"},
		{"legacy", nil, "age unknown", "Open questions: 1 · Unknown age: 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := operations.Decision{Kind: "question", ProjectID: "p", Issue: "owner/repo#1", Question: "Route upstream?", AskedAt: tc.at}
			report := operations.Report{DataTime: now, Decisions: []operations.Decision{q}}
			var html bytes.Buffer
			if err := OperationsSnapshot(report).Render(t.Context(), &html); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{tc.summary, "p · Waiting " + tc.age, q.Question} {
				if !strings.Contains(html.String(), want) {
					t.Fatalf("missing %q in rendered operations", want)
				}
			}
			for _, lane := range []string{"Todo", "Rework"} {
				issue := telemetry.Issue{ID: "1", Identifier: q.Issue, ProjectID: "p", State: lane}
				data := DashboardData{Snapshot: telemetry.Snapshot{GeneratedAt: now, BoardIssues: []telemetry.Issue{issue}, OpenQuestions: []operations.Decision{q}}}
				facts := boardCardFacts(data, projectKanbanCard{IssueID: "1", Identifier: q.Issue, ProjectID: "p", Stage: lane})
				if got := facts[4]; got.Text != "waiting for a human reply · "+tc.age || got.Detail != q.Question {
					t.Fatalf("reason = %+v", got)
				}
			}
		})
	}
}
