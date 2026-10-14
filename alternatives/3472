package detent

import "testing"

func TestRepositoryGuidanceRequiresUISurfaceContracts(t *testing.T) {
	t.Parallel()

	contributing := readRepositoryTextFile(t, "CONTRIBUTING.md")
	featureTemplate := readRepositoryTextFile(t, ".github/ISSUE_TEMPLATE/feature_request.md")
	bugTemplate := readRepositoryTextFile(t, ".github/ISSUE_TEMPLATE/bug_report.md")
	prTemplate := readRepositoryTextFile(t, ".github/PULL_REQUEST_TEMPLATE.md")

	for _, want := range []string{
		"High-impact UI changes require explicit issue authorization before implementation starts",
		"Persistent top-of-screen messages require explicit issue authorization",
		"generic words like \"alert\", \"blocked\", \"status\", or \"notify\"",
		"Workflow state that is already visible in a Kanban card defaults to contextual card indicators",
		"Visual changes to primary operator screens require screenshot or browser verification before review",
		"Small copy changes, icon swaps, and local component styling do not need a heavyweight UI surface contract",
	} {
		assertContainsWords(t, contributing, want)
	}

	for _, content := range []string{featureTemplate, bugTemplate} {
		for _, want := range []string{
			"## UI Surface Contract",
			"primary operator screen, persistent notification surface, layout density, first-viewport real estate, navigation, dashboard region, or responsive visibility",
			"Persistent global messaging allowed: yes/no",
			"Information that must stay contextual on cards, rows, or details",
			"Expected first viewport, screenshot, mockup, or text description",
			"Screenshot or browser verification expected",
		} {
			assertContainsWords(t, content, want)
		}
	}

	for _, want := range []string{
		"## UI Surface Contract",
		"the linked issue explicitly authorizes any high-impact UI surface, layout, density, first-viewport, or responsive visibility tradeoff",
		"Persistent top-of-screen messaging is explicitly authorized by the issue",
		"Visual changes to primary operator screens include screenshot or browser verification below",
	} {
		assertContainsWords(t, prTemplate, want)
	}
}
