package templates_test

import (
	"regexp"
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
		`id="dashboard-sidebar"`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
		`href="/projects/detent"`,
		`href="/reports"`,
		`href="/settings"`,
		`href="/"`,
		`id="github-api-health"`,
		"Health",
		"Unknown",
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
	assertTemplateSharedDashboardShellOnce(t, html)
	assertTemplateHealthInSidebar(t, html)
	assertTemplateSingleCurrentSidebarItem(t, html)
	assertTemplateActiveSidebarLink(t, html, "/reports")
	assertTemplateInactiveSidebarLink(t, html, "/")
	assertTemplateInactiveSidebarLink(t, html, "/settings")
	assertTemplateInactiveSidebarLink(t, html, "/projects/detent")
}

func assertTemplateActiveSidebarLink(t *testing.T, html string, href string) {
	t.Helper()

	if !templateSidebarLinkActive(html, href) {
		t.Fatalf("page missing active sidebar link %q:\n%s", href, html)
	}
}

func assertTemplateInactiveSidebarLink(t *testing.T, html string, href string) {
	t.Helper()

	if templateSidebarLinkActive(html, href) {
		t.Fatalf("page rendered inactive sidebar link %q as active:\n%s", href, html)
	}
}

func assertTemplateSingleCurrentSidebarItem(t *testing.T, html string) {
	t.Helper()

	currentLinks := regexp.MustCompile(`<a[^>]*aria-current="page"[^>]*>`).FindAllString(html, -1)
	if len(currentLinks) != 1 {
		t.Fatalf("page rendered %d current sidebar links, want 1: %v\n%s", len(currentLinks), currentLinks, html)
	}
	if !strings.Contains(currentLinks[0], `data-tui-sidebar-active="true"`) {
		t.Fatalf("current sidebar link missing active marker: %s\n%s", currentLinks[0], html)
	}
}

func assertTemplateSharedDashboardShellOnce(t *testing.T, html string) {
	t.Helper()

	for _, marker := range []string{
		`data-tui-sidebar-layout`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
	} {
		if got := strings.Count(html, marker); got != 1 {
			t.Fatalf("page rendered %q %d times, want 1:\n%s", marker, got, html)
		}
	}
}

func templateSidebarLinkActive(html string, href string) bool {
	pattern := `<a[^>]*href="` + regexp.QuoteMeta(href) + `"[^>]*>`
	for _, link := range regexp.MustCompile(pattern).FindAllString(html, -1) {
		if strings.Contains(link, `data-tui-sidebar-active="true"`) && strings.Contains(link, `aria-current="page"`) {
			return true
		}
	}
	return false
}
