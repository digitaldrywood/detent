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
	assertContains(t, onboarding, "rg -v '^MUTATION_CONFIRMED='")
	assertContains(t, onboarding, "last == \"MUTATION_CONFIRMED=true\"")
	assertContains(t, onboarding, "GITHUB_MODE=<project_v2|issue_field|label>")
	assertContains(t, onboarding, "tracker.github_status_source: label")
	assertContains(t, onboarding, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision`)
	assertContains(t, onboarding, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation`)
	assertContains(t, onboarding, "Do not choose label mode for the operator")
	assertContains(t, onboarding, "rg '^GITHUB_MODE=project_v2$'")
	assertContains(t, onboarding, "rg '^BOARD_MODE=(reuse|create)$'")
	assertContains(t, onboarding, "rg '^PROJECT_NUMBER='")
	assertContains(t, onboarding, "rg '^PROJECT_TITLE='")
	assertContains(t, onboarding, "rg '^STATUS_LABEL_PREFIX='")
	assertMutationBlocksUseValidator(t, onboarding)

	assertContains(t, readme, "do not create, link, mutate, or delete GitHub Projects, issue fields, labels")
	assertContainsWords(t, readme, "until Phase 2 answers are recorded in `answers.env`")
	assertContains(t, readme, "detent onboarding validate-answers")
	assertContains(t, readme, "Defaults are recommendations only")
}

func TestOnboardingDocsRequireIdentityGateBeforeDiscovery(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	identityIndex := strings.Index(onboarding, "## Phase 0.5")
	decisionIndex := strings.Index(onboarding, "## Phase 0.6")
	discoveryIndex := strings.Index(onboarding, "## Phase 1")
	if identityIndex < 0 {
		t.Fatal("docs/ONBOARDING.md missing Phase 0.5 identity gate")
	}
	if decisionIndex < 0 {
		t.Fatal("docs/ONBOARDING.md missing Phase 0.6 status-source decision")
	}
	if discoveryIndex < 0 {
		t.Fatal("docs/ONBOARDING.md missing Phase 1 discovery")
	}
	if identityIndex > discoveryIndex {
		t.Fatal("docs/ONBOARDING.md places identity gate after Phase 1 discovery")
	}
	if decisionIndex > discoveryIndex {
		t.Fatal("docs/ONBOARDING.md places status-source decision after Phase 1 discovery")
	}

	for _, want := range []string{
		"CUSTOMER_ID=<customer-or-workstream-id>",
		"DETENT_PROJECT_ID=<local-detent-project-id>",
		"TARGET_REPOSITORY=<repo-owner>/<repo-name>",
		"TARGET_SOURCE_ROOT=<absolute-local-checkout-path>",
		"REFERENCE_REPOSITORIES=<comma-separated-owner/repo-list-or-empty>",
		"DETENT_ONBOARDING_MODE=<new-install|existing-install|add-project>",
		"IDENTITY_CONFIRMED=true",
		`detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity`,
		"wrong target repository",
	} {
		assertContains(t, onboarding, want)
	}

	assertContains(t, readme, "Distinguish reference repositories from the target repository")
	assertContains(t, readme, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity`)
	assertContains(t, readme, "Ask and record `GITHUB_MODE` explicitly")
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

func assertMutationBlocksUseValidator(t *testing.T, text string) {
	t.Helper()

	lines := strings.Split(text, "\n")
	inFence := false
	fenceStart := 0
	var block []string
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inFence {
				inFence = true
				fenceStart = index + 1
				block = nil
				continue
			}
			blockText := strings.Join(block, "\n")
			if strings.Contains(blockText, "rg '^MUTATION_CONFIRMED=true$'") &&
				!strings.Contains(blockText, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation`) {
				t.Fatalf("mutation confirmation block starting at line %d missing validate-answers", fenceStart)
			}
			inFence = false
			block = nil
			continue
		}
		if inFence {
			block = append(block, line)
		}
	}
}
