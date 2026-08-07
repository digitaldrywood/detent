package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestProjectSmallMultipleCardDisplaysOffHoursIndicator(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	nextOpen := time.Date(2026, time.August, 7, 22, 0, 0, 0, location)
	cards := projectSmallMultipleCards(DashboardData{Projects: []ProjectSmallMultiple{{
		ID:   "detent",
		Name: "Detent",
		ActiveHours: telemetry.ActiveHours{
			Configured: true,
			Timezone:   location.String(),
			NextOpen:   &nextOpen,
		},
	}}})
	if len(cards) != 1 {
		t.Fatalf("projectSmallMultipleCards() len = %d, want 1", len(cards))
	}
	card := cards[0]
	if !card.ActiveHoursVisible || card.ActiveHoursCozyLabel != "Off hours · opens 22:00 CDT" || card.ActiveHoursCompactLabel != "22:00" {
		t.Fatalf("active-hours card fields = %#v", card)
	}

	html := renderBoardComponent(t, projectSmallMultipleCardView(card))
	for _, want := range []string{
		`data-help-trigger`,
		`data-help-scope="project-active-hours"`,
		`data-help-term="active-hours-detent"`,
		`data-active-hours-label="cozy"`,
		`data-active-hours-label="compact"`,
		"Off hours · opens 22:00 CDT",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered card missing %q:\n%s", want, html)
		}
	}
}

func TestProjectSmallMultipleCardHidesIndicatorWhenAdmissionIsOpen(t *testing.T) {
	t.Parallel()
	cards := projectSmallMultipleCards(DashboardData{Projects: []ProjectSmallMultiple{{
		ID: "detent",
		ActiveHours: telemetry.ActiveHours{
			Configured:     true,
			Open:           true,
			OverrideActive: true,
		},
	}}})
	if len(cards) != 1 || cards[0].ActiveHoursVisible {
		t.Fatalf("cards = %#v, want hidden active-hours indicator", cards)
	}
}

func TestHealthActiveHoursRowsStayNominal(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	nextOpen := time.Date(2026, time.August, 7, 22, 0, 0, 0, location)
	data := DashboardData{Projects: []ProjectSmallMultiple{{
		ID: "detent",
		ActiveHours: telemetry.ActiveHours{
			Configured: true,
			Timezone:   location.String(),
			NextOpen:   &nextOpen,
		},
	}}}

	rows := healthActiveHoursRows(data)
	if len(rows) != 1 {
		t.Fatalf("healthActiveHoursRows() = %#v, want one row", rows)
	}
	if rows[0].Kind != primitives.KindOK || rows[0].Status != "Off hours" || rows[0].Resets != "at window open" {
		t.Fatalf("active-hours health row = %#v", rows[0])
	}
	if !strings.Contains(rows[0].Detail, "Fri, Aug 7 at 22:00 CDT") {
		t.Fatalf("Detail = %q, want next opening", rows[0].Detail)
	}
}
