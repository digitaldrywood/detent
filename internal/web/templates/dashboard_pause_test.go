package templates

import (
	"strings"
	"testing"
)

func TestProjectOverviewSnapshotDisplaysPauseDetails(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, ProjectOverviewSnapshot(DashboardData{
		ProjectID:          "detent",
		ProjectName:        "Detent",
		ProjectPaused:      true,
		ProjectPauseReason: "release hold",
		ProjectPauseIssue:  "digitaldrywood/detent#1499",
	}))

	for _, want := range []string{
		"Detent is paused.",
		"Reason: release hold",
		"Until issue closes: digitaldrywood/detent#1499",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("paused banner missing %q:\n%s", want, html)
		}
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
