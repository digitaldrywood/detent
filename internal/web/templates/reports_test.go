package templates_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestReportsRendersEveryProjectBreakdown(t *testing.T) {
	t.Parallel()

	projects := make([]templates.UsageBucketData, 0, 6)
	var totalTokens int64
	for i := 1; i <= 6; i++ {
		tokens := int64((7 - i) * 100)
		totalTokens += tokens
		projects = append(projects, templates.UsageBucketData{
			Bucket:       "project-" + strconv.Itoa(i),
			Label:        "project-" + strconv.Itoa(i),
			InputTokens:  tokens / 2,
			OutputTokens: tokens / 2,
			TotalTokens:  tokens,
			Events:       1,
		})
	}

	html := renderComponent(t, templates.Reports(templates.ReportsData{
		Title:       "Detent reports",
		GeneratedAt: time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC),
		Day: templates.UsageReportData{
			Totals: templates.UsageTotalsData{
				TotalTokens: totalTokens,
				Events:      int64(len(projects)),
			},
		},
		Project: templates.UsageReportData{
			Totals: templates.UsageTotalsData{
				TotalTokens: totalTokens,
				Events:      int64(len(projects)),
			},
			Breakdowns: projects,
		},
	}))

	for i := 1; i <= 6; i++ {
		want := "project-" + strconv.Itoa(i)
		if !strings.Contains(html, want) {
			t.Fatalf("reports page missing %q:\n%s", want, html)
		}
	}
}

func TestReportsIncludesResponsiveLayoutClasses(t *testing.T) {
	t.Parallel()

	html := renderComponent(t, templates.Reports(templates.ReportsData{
		Title:       "Detent reports",
		GeneratedAt: time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC),
		Projects: []templates.ProjectSmallMultiple{
			{
				ID:      "detent",
				Name:    "Detent",
				Running: 2,
			},
		},
		Day: templates.UsageReportData{
			Totals: templates.UsageTotalsData{
				TotalTokens: 125_000,
				SpendUSD:    4.25,
				Events:      3,
			},
		},
	}))

	for _, want := range []string{
		"overflow-x-hidden",
		`data-tui-sidebar-layout`,
		`data-tui-sidebar-collapsible="icon"`,
		`data-tui-sidebar="menu-badge"`,
		`data-tui-sheet`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
		`data-tui-sidebar-active="true" aria-current="page"`,
		`href="/reports"`,
		`href="/settings"`,
		`href="/"`,
		"dashboard-topbar",
		`data-tui-sidebar-target="dashboard-sidebar"`,
		`<h1 class="sr-only">Reports</h1>`,
		"grid min-w-0 gap-5 xl:grid-cols-2",
		"grid min-w-0 gap-5 xl:grid-cols-[minmax(0,1fr)_22rem]",
		"break-all font-mono text-2xl",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("reports page missing responsive marker %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		"dashboard-nav flex min-w-0 items-center gap-4",
		"dashboard-nav-link",
		"underline decoration-2 underline-offset-4",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("reports page rendered old nav marker %q:\n%s", forbidden, html)
		}
	}
	if !strings.Contains(html, `aria-current="page"`) {
		t.Fatalf("reports page missing active sidebar aria-current:\n%s", html)
	}
}
