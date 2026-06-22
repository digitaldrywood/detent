package detent_test

import (
	"os"
	"strings"
	"testing"
)

func TestInstallerSmokeUsesAuthenticatedReleaseVersion(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(.github/workflows/ci.yml) error = %v", err)
	}
	workflow := strings.ReplaceAll(string(raw), "\r\n", "\n")
	job := workflowBetween(t, workflow, "  installer-smoke:", "\n  goreleaser-snapshot:")

	for _, want := range []string{
		"name: Resolve release installer version",
		"GH_TOKEN: ${{ github.token }}",
		"gh release view --repo \"$GITHUB_REPOSITORY\" --json tagName --jq .tagName",
		"DETENT_VERSION=$tag",
		"$GITHUB_ENV",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("installer-smoke job missing %q", want)
		}
	}

	linux := workflowBetween(t, job, "      - name: Smoke release installer\n        if: runner.os == 'Linux'", "      - name: Smoke release installer\n        if: runner.os == 'Windows'")
	for _, want := range []string{
		"2>&1",
		"falling back to go install",
		"Release installer fell back to go install",
		"exit 1",
		"Verified checksum for detent_",
	} {
		if !strings.Contains(linux, want) {
			t.Fatalf("Linux installer smoke step missing %q", want)
		}
	}

	windows := workflowBetween(t, job, "      - name: Smoke release installer\n        if: runner.os == 'Windows'", "")
	for _, want := range []string{
		"falling back to go install",
		"Release installer fell back to go install",
		"Verified checksum for detent_.*_windows_.*\\.zip",
	} {
		if !strings.Contains(windows, want) {
			t.Fatalf("Windows installer smoke step missing %q", want)
		}
	}
}

func TestKanbanBrowserDragDropRunsInVisualGate(t *testing.T) {
	t.Parallel()

	browserTestRaw, err := os.ReadFile("internal/cli/dev_runtime_browser_e2e_test.go")
	if err != nil {
		t.Fatalf("ReadFile(internal/cli/dev_runtime_browser_e2e_test.go) error = %v", err)
	}
	browserTest := strings.ReplaceAll(string(browserTestRaw), "\r\n", "\n")
	if !strings.HasPrefix(browserTest, "//go:build browser_e2e\n\n") {
		t.Fatal("Kanban browser drag/drop Go CDP test must be behind the browser_e2e build tag")
	}
	if !strings.Contains(browserTest, "CI exercises Kanban drag/drop in the Playwright browser visual gate") {
		t.Fatal("Kanban browser drag/drop Go CDP test must document why CI uses the visual gate")
	}

	workflowRaw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(.github/workflows/ci.yml) error = %v", err)
	}
	workflow := strings.ReplaceAll(string(workflowRaw), "\r\n", "\n")
	visualJob := workflowBetween(t, workflow, "  browser-visual:", "\n  windows-core:")
	for _, want := range []string{
		"internal/cli/dev_runtime*.go",
		"name: Upload browser visual evidence",
		"tmp/playwright-evidence",
		"name: Upload browser visual failure artifacts",
		"tmp/playwright-report",
		"tmp/playwright-results",
	} {
		if !strings.Contains(visualJob, want) {
			t.Fatalf("browser visual job missing %q", want)
		}
	}

	visualSpecRaw, err := os.ReadFile("tests/visual/layout.spec.js")
	if err != nil {
		t.Fatalf("ReadFile(tests/visual/layout.spec.js) error = %v", err)
	}
	visualSpec := strings.ReplaceAll(string(visualSpecRaw), "\r\n", "\n")
	for _, want := range []string{
		`test("direct Kanban blocked drag stays client-side"`,
		`Move blocked by transition policy.`,
		`expect(moveRequests).toHaveLength(0)`,
		`#kanban-action-dialog`,
	} {
		if !strings.Contains(visualSpec, want) {
			t.Fatalf("browser visual spec missing %q", want)
		}
	}
}

func workflowBetween(t *testing.T, content string, startMarker string, endMarker string) string {
	t.Helper()

	start := strings.Index(content, startMarker)
	if start == -1 {
		t.Fatalf("workflow missing marker %q", startMarker)
	}
	section := content[start:]
	if endMarker == "" {
		return section
	}
	end := strings.Index(section[len(startMarker):], endMarker)
	if end == -1 {
		t.Fatalf("workflow missing end marker %q after %q", endMarker, startMarker)
	}
	return section[:len(startMarker)+end]
}
