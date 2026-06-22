package detent_test

import (
	"os"
	"strings"
	"testing"
)

type requiredStatusCheck struct {
	name     string
	jobStart string
	jobEnd   string
	markers  []string
}

var requiredMainStatusChecks = []requiredStatusCheck{
	{
		name:     "Lint",
		jobStart: "  lint:",
		jobEnd:   "  verify:",
		markers:  []string{"name: Lint"},
	},
	{
		name:     "Verify (ubuntu-latest)",
		jobStart: "  verify:",
		jobEnd:   "  test-cover:",
		markers:  []string{"name: Verify (${{ matrix.os }})", "os: ubuntu-latest"},
	},
	{
		name:     "Verify (macos-latest)",
		jobStart: "  verify:",
		jobEnd:   "  test-cover:",
		markers:  []string{"name: Verify (${{ matrix.os }})", "os: macos-latest"},
	},
	{
		name:     "Verify (windows-latest)",
		jobStart: "  verify:",
		jobEnd:   "  test-cover:",
		markers:  []string{"name: Verify (${{ matrix.os }})", "os: windows-latest"},
	},
	{
		name:     "Test Coverage",
		jobStart: "  test-cover:",
		jobEnd:   "  browser-visual:",
		markers:  []string{"name: Test Coverage"},
	},
	{
		name:     "Browser Visual",
		jobStart: "  browser-visual:",
		jobEnd:   "  windows-core:",
		markers:  []string{"name: Browser Visual"},
	},
	{
		name:     "Windows Core",
		jobStart: "  windows-core:",
		jobEnd:   "  installer-smoke:",
		markers:  []string{"name: Windows Core"},
	},
	{
		name:     "Installer Smoke (ubuntu-latest)",
		jobStart: "  installer-smoke:",
		jobEnd:   "  goreleaser-snapshot:",
		markers:  []string{"name: Installer Smoke (${{ matrix.os }})", "os: [ubuntu-latest, windows-latest]"},
	},
	{
		name:     "Installer Smoke (windows-latest)",
		jobStart: "  installer-smoke:",
		jobEnd:   "  goreleaser-snapshot:",
		markers:  []string{"name: Installer Smoke (${{ matrix.os }})", "os: [ubuntu-latest, windows-latest]"},
	},
	{
		name:     "GoReleaser Snapshot",
		jobStart: "  goreleaser-snapshot:",
		jobEnd:   "",
		markers:  []string{"name: GoReleaser Snapshot"},
	},
}

func TestCIConcurrencyKeepsMainPushRuns(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	concurrency := workflowBetween(t, workflow, "concurrency:\n", "\njobs:")
	for _, want := range []string{
		"group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || github.run_id }}",
		"cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
	} {
		if !strings.Contains(concurrency, want) {
			t.Fatalf("CI concurrency missing %q", want)
		}
	}
}

func TestMainProtectionDocumentationMatchesWorkflow(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	docs := readNormalizedFile(t, "docs/execution-seams.md")
	protection := workflowBetween(t, docs, "### Main Branch Protection\n", "\n## Still Git/PR Coupled")

	for _, want := range []string{
		"`required_status_checks.strict: true`",
		"`cancel-in-progress: ${{ github.event_name == 'pull_request' }}`",
		"`Browser Visual`",
	} {
		if !strings.Contains(protection, want) {
			t.Fatalf("main branch protection docs missing %q", want)
		}
	}

	for _, check := range requiredMainStatusChecks {
		if !strings.Contains(protection, "- `"+check.name+"`") {
			t.Fatalf("main branch protection docs missing required check %q", check.name)
		}

		job := workflowBetween(t, workflow, check.jobStart, check.jobEnd)
		for _, marker := range check.markers {
			if !strings.Contains(job, marker) {
				t.Fatalf("workflow job for required check %q missing %q", check.name, marker)
			}
		}
	}
}

func TestInstallerSmokeUsesAuthenticatedReleaseVersion(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
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

func readNormalizedFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
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
