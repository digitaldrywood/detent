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
	workflow := string(raw)
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
