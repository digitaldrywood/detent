package detent

import (
	"os"
	"strings"
	"testing"
)

func TestOnboardingDocsRequireMutationAuthorization(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	assertContains(t, onboarding, "## Phase 2.5")
	assertContains(t, onboarding, "MUTATION_CONFIRMED=true")
	assertContains(t, onboarding, "GITHUB_MODE=<project_v2|issue_field|label>")
	assertContains(t, onboarding, "tracker.github_status_source: label")
	assertContains(t, onboarding, "rg '^GITHUB_MODE=project_v2$'")
	assertContains(t, onboarding, "rg '^BOARD_MODE=(reuse|create)$'")
	assertContains(t, onboarding, "rg '^STATUS_LABEL_PREFIX='")

	assertContains(t, readme, "do not create, link, mutate, or delete GitHub Projects, issue fields, labels")
	assertContainsWords(t, readme, "until Phase 2 answers are recorded in `answers.env`")
	assertContains(t, readme, "Defaults are recommendations only")
}

func readRepositoryTextFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(raw)
}

func assertContains(t *testing.T, text string, want string) {
	t.Helper()

	if !strings.Contains(text, want) {
		t.Fatalf("document missing %q", want)
	}
}

func assertContainsWords(t *testing.T, text string, want string) {
	t.Helper()

	normalizedText := strings.Join(strings.Fields(text), " ")
	normalizedWant := strings.Join(strings.Fields(want), " ")
	assertContains(t, normalizedText, normalizedWant)
}
