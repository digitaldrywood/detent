package templates_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestSettingsIncludesSharedSidebarShell(t *testing.T) {
	t.Parallel()

	html := renderComponent(t, templates.Settings(templates.SettingsData{
		Title:            "Detent settings",
		Version:          "v1.2.3",
		SidebarCollapsed: true,
		SidebarProjects: []templates.ProjectSmallMultiple{
			{
				ID:      "detent",
				Name:    "Detent",
				Running: 2,
			},
		},
		Projects: []templates.SettingsProject{
			{
				ID:                    "detent",
				TrackerKind:           "github",
				DependencyAutoUnblock: "enabled",
			},
		},
	}))

	for _, want := range []string{
		`<title>Detent settings</title>`,
		`id="app-sidebar"`,
		`data-rail="true"`,
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		`href="/projects/detent"`,
		`href="/health/ui"`,
		"Health",
		">Detent</span>",
		"Read-only view of the running configuration.",
		"v1.2.3",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("settings page missing shared shell marker %q:\n%s", want, html)
		}
	}

	for _, forbidden := range []string{
		"dashboard-nav flex min-w-0 items-center gap-4",
		"dashboard-nav-link",
		"underline decoration-2 underline-offset-4",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("settings page rendered old nav marker %q:\n%s", forbidden, html)
		}
	}

	currentLinks := regexp.MustCompile(`<a[^>]*aria-current="page"[^>]*>`).FindAllString(html, -1)
	if len(currentLinks) != 1 {
		t.Fatalf("page rendered %d current sidebar links, want 1: %v\n%s", len(currentLinks), currentLinks, html)
	}
	if !strings.Contains(currentLinks[0], `href="/settings"`) {
		t.Fatalf("current sidebar link is not settings: %s", currentLinks[0])
	}
}
