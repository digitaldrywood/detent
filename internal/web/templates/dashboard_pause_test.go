package templates

import (
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestProjectOverviewSnapshotsDisplayPauseDetails(t *testing.T) {
	t.Parallel()

	data := DashboardData{
		ProjectID:          "detent",
		ProjectName:        "Detent",
		ProjectPaused:      true,
		ProjectPauseReason: "release hold",
		ProjectPauseIssue:  "digitaldrywood/detent#1499",
	}

	tests := []struct {
		name      string
		component templ.Component
	}{
		{name: "legacy snapshot", component: ProjectOverviewSnapshot(data)},
		{name: "active snapshot", component: projectOverviewSnapshotBody(data)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			html := renderBoardComponent(t, tt.component)
			for _, want := range []string{
				"Detent is paused.",
				"Reason: release hold",
				"Until issue closes: digitaldrywood/detent#1499",
				"Evaluation: pending",
			} {
				if !strings.Contains(html, want) {
					t.Fatalf("paused banner missing %q:\n%s", want, html)
				}
			}
		})
	}
}

func TestProjectSmallMultipleCardDisplaysPauseEvaluation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		project ProjectSmallMultiple
		want    string
	}{
		{
			name: "evaluable through owning project",
			project: ProjectSmallMultiple{
				ID:                 "video",
				Paused:             true,
				PauseIssue:         "digitaldrywood/video-studio#147",
				PauseExitEvaluated: true,
				PauseExitEvaluable: true,
				PauseExitResolver:  "video-studio",
			},
			want: "Evaluation: evaluable via video-studio",
		},
		{
			name: "unevaluable names resolver error",
			project: ProjectSmallMultiple{
				ID:                 "video",
				Paused:             true,
				PauseIssue:         "digitaldrywood/video-studio#147",
				PauseExitEvaluated: true,
				PauseExitError:     "resolver unavailable",
			},
			want: "Evaluation: unevaluable: resolver unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cards := projectSmallMultipleCards(DashboardData{Projects: []ProjectSmallMultiple{tt.project}})
			if len(cards) != 1 {
				t.Fatalf("projectSmallMultipleCards() len = %d, want 1", len(cards))
			}
			html := renderBoardComponent(t, projectSmallMultipleCardView(cards[0]))
			if !strings.Contains(html, tt.want) {
				t.Fatalf("fleet card missing %q:\n%s", tt.want, html)
			}
		})
	}
}

func TestProjectSmallMultipleCardDisplaysPauseDetails(t *testing.T) {
	t.Parallel()

	cards := projectSmallMultipleCards(DashboardData{Projects: []ProjectSmallMultiple{{
		ID:          "detent",
		Name:        "Detent",
		Paused:      true,
		PauseReason: "release hold",
		PauseUntil:  "2026-08-01T12:00:00Z",
	}}})
	if len(cards) != 1 {
		t.Fatalf("projectSmallMultipleCards() len = %d, want 1", len(cards))
	}

	html := renderBoardComponent(t, projectSmallMultipleCardView(cards[0]))
	for _, want := range []string{
		"paused / 0 running / 0 queued / 0 blocked",
		"Reason: release hold",
		"Until: 2026-08-01T12:00:00Z",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet card missing %q:\n%s", want, html)
		}
	}
}

func TestProjectSmallMultipleCardHidesInactivePauseMetadata(t *testing.T) {
	t.Parallel()

	cards := projectSmallMultipleCards(DashboardData{Projects: []ProjectSmallMultiple{{
		ID:          "detent",
		Name:        "Detent",
		PauseReason: "stale reason",
		PauseUntil:  "2026-08-01T12:00:00Z",
	}}})
	if len(cards) != 1 {
		t.Fatalf("projectSmallMultipleCards() len = %d, want 1", len(cards))
	}
	if cards[0].PauseDetail != "" {
		t.Fatalf("active project pause detail = %q, want empty", cards[0].PauseDetail)
	}
}
