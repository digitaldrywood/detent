package templates_test

import (
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
		`data-tui-sidebar-layout`,
		`id="dashboard-sidebar"`,
		`data-tui-sidebar-state="collapsed"`,
		`data-tui-sidebar-collapsible="icon"`,
		`data-tui-sidebar="menu-badge"`,
		`data-tui-sheet`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		`href="/projects/detent"`,
		`Detent - active, 2 running`,
		`<h1 class="sr-only">Settings</h1>`,
		"Startup configuration, project paths, and runtime files.",
		"Project list and project settings",
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

	assertTemplateSharedDashboardShellOnce(t, html)
	assertTemplateSingleCurrentSidebarItem(t, html)
	assertTemplateActiveSidebarLink(t, html, "/settings")
	assertTemplateInactiveSidebarLink(t, html, "/")
	assertTemplateInactiveSidebarLink(t, html, "/reports")
	assertTemplateInactiveSidebarLink(t, html, "/projects/detent")
}
