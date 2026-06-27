package detent

import (
	"os"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
	assertContains(t, onboarding, `detent --format pretty onboarding explain-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision`)
	assertContains(t, onboarding, "Show this summary before the canonical `answers.env` keys")
	assertContains(t, onboarding, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision`)
	assertContains(t, onboarding, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation`)
	assertContains(t, onboarding, "Do not choose label mode for the operator")
	assertContains(t, onboarding, "rg '^GITHUB_MODE=project_v2$'")
	assertContains(t, onboarding, "rg '^BOARD_MODE=(reuse|create)$'")
	assertContains(t, onboarding, "rg '^PROJECT_NUMBER='")
	assertContains(t, onboarding, "rg '^PROJECT_TITLE='")
	assertContains(t, onboarding, "rg '^STATUS_LABEL_PREFIX='")
	assertContainsWords(t, onboarding, "read-only `detent doctor --port 0` before proposing changes")
	assertContainsWords(t, onboarding, "Do not pass `--allow-write-probes` during this identity-safe verification")
	assertContainsWords(t, onboarding, "do not pass `--allow-write-probes` before Phase 2.5")
	assertContains(t, onboarding, "detent doctor --port 0 --allow-write-probes")
	assertMutationBlocksUseValidator(t, onboarding)

	assertContains(t, readme, "do not create, link, mutate, or delete GitHub Projects, issue fields, labels")
	assertContainsWords(t, readme, "until Phase 2 answers are recorded in `answers.env`")
	assertContainsWords(t, readme, "read-only doctor status with `detent doctor --port 0` before recommending changes")
	assertContains(t, readme, "Do not pass `--allow-write-probes` until the mutation gate passes")
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
		`detent onboarding draft-answers --output pretty`,
		`detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write`,
		`If a verified local Detent source checkout is available and you want the draft`,
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

	assertContains(t, readme, "detent onboarding draft-answers --output pretty")
	assertContains(t, readme, `detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write`)
	assertContains(t, readme, "Distinguish reference repositories from the target repository")
	assertContains(t, readme, `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity`)
	assertContains(t, readme, "Ask and record `GITHUB_MODE` explicitly")
}

func TestOnboardingDocsRequireDetentSourceFreshnessBeforeRunbook(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		`gh api repos/digitaldrywood/detent/git/ref/heads/main --jq '.object.sha'`,
		"DETENT_DOCS_ACCESS_METHOD=github_api",
		"DETENT_DOCS_REPOSITORY=digitaldrywood/detent",
		"DETENT_DOCS_REF=main",
		"DETENT_DOCS_COMMIT",
		`git -C "$DETENT_SOURCE_ROOT" fetch origin main:refs/remotes/origin/main`,
		`git -C "$DETENT_SOURCE_ROOT" rev-parse HEAD`,
		`git -C "$DETENT_SOURCE_ROOT" rev-parse refs/remotes/origin/main`,
		`detent --format json version`,
		"DETENT_SOURCE_MATCHES_CANONICAL",
		"DETENT_BINARY_MATCHES_CANONICAL",
		"$ONBOARDING_DIR/detent-source-freshness.env",
		"Continue from the remote GitHub documentation source",
		"documentation repository, ref, commit SHA, access",
	} {
		assertContains(t, onboarding, want)
	}

	for _, want := range []string{
		"Pin the Detent docs to a concrete canonical commit before relying on them",
		"`DETENT_DOCS_ACCESS_METHOD=github_api`",
		"`DETENT_DOCS_REPOSITORY=digitaldrywood/detent`",
		"`DETENT_DOCS_REF=main`",
		"`DETENT_DOCS_COMMIT`",
		`git -C "$DETENT_SOURCE_ROOT" fetch origin main:refs/remotes/origin/main`,
		"`DETENT_SOURCE_MATCHES_CANONICAL`",
		"`DETENT_BINARY_MATCHES_CANONICAL`",
		"read Detent docs from GitHub at `DETENT_DOCS_COMMIT` instead of cloning or relying on local files",
		`--detent-source-root "$DETENT_SOURCE_ROOT"`,
	} {
		assertContainsWords(t, readme, want)
	}

	assertOrder(t, readme, "Pin the Detent docs", "From the pinned Detent documentation source, read README.md")
	assertOrder(t, onboarding, "## Source Freshness Gate", "## Start Here")
	assertOrder(t, onboarding, "DETENT_DOCS_COMMIT", "## Phase 0.5")
}

func TestOnboardingDocsAllowRemoteDetentSourceInspectionWithoutLocalCheckout(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		"Remote Detent Documentation Source",
		"DETENT_DOCS_ACCESS_METHOD=github_api",
		"DETENT_DOCS_REPOSITORY=digitaldrywood/detent",
		"DETENT_DOCS_REF=main",
		"DETENT_DOCS_COMMIT",
		`gh api repos/digitaldrywood/detent/git/ref/heads/main --jq '.object.sha'`,
		"README.md AGENTS.md CLAUDE.md docs/ONBOARDING.md CONTRIBUTING.md",
		".github/workflows docs/templates",
		"Do not clone Detent by default",
	} {
		assertContains(t, onboarding, want)
	}

	for _, want := range []string{
		"Use GitHub as the first-class Detent documentation source when a verified local checkout is absent, stale, or not desired",
		"gh api repos/digitaldrywood/detent/git/ref/heads/main --jq '.object.sha'",
		"`DETENT_DOCS_ACCESS_METHOD=github_api`",
		"`DETENT_DOCS_COMMIT`",
		"Do not clone Detent by default",
	} {
		assertContainsWords(t, readme, want)
	}

	assertOrder(t, onboarding, "Remote Detent Documentation Source", "Local Detent Source Checkout")
	assertOrder(t, readme, "Use GitHub as the", "If a verified")
}

func TestOnboardingDocsPreserveEarlyLabelStatusSourceDecision(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		"volunteer a status-source answer before identity is confirmed",
		"preserve it as a pending decision outside `answers.env`",
		"append `GITHUB_MODE=label` and run the decision validator without asking again",
		"Do not write `GITHUB_MODE` to `answers.env` until the identity phase passes",
	} {
		assertContainsWords(t, readme, want)
	}

	for _, want := range []string{
		"If the operator already volunteered a status-source answer before identity validation, do not ask the status-source question again",
		"After `detent onboarding validate-answers --answers \"$ONBOARDING_DIR/answers.env\" --phase identity` passes, append the pending `GITHUB_MODE` answer",
		"`GITHUB_MODE=label`",
		"run `detent onboarding validate-answers --answers \"$ONBOARDING_DIR/answers.env\" --phase decision` without re-asking",
	} {
		assertContainsWords(t, onboarding, want)
	}
}

func TestOnboardingDocsInferCurrentCheckoutCandidateBeforeRawFields(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		"infer and restate an identity candidate from the current git checkout",
		"If the current working directory is a GitHub checkout and is not the canonical Detent source checkout, propose it as the target candidate",
		"Present the candidate in human-facing language first, then show the `answers.env` representation",
	} {
		assertContainsWords(t, readme, want)
	}

	for _, want := range []string{
		"`do not assume` means infer a candidate from identity-safe local evidence and confirm it",
		"`pwd`, `git rev-parse --show-toplevel`, `git remote get-url origin`",
		"Reuse the existing project id when it is the same target repository or source root",
		"Current directory is `/home/loganlanou/projects/digitaldrywood/creswoodcorners-phone`",
		"Detent source checkout is `/home/loganlanou/projects/digitaldrywood/detent`",
		"Onboarding mode is `add-project`",
		"Customer/workstream: `creswoodcorners`",
		"Project id: `creswoodcorners-phone`",
		"Target repository: `digitaldrywood/creswoodcorners-phone`",
		"Source checkout: `/home/loganlanou/projects/digitaldrywood/creswoodcorners-phone`",
		"customer_id_source=repo_prefix",
		"customer_id_confidence=medium",
		"Customer/workstream alternatives: `digitaldrywood`",
		"`CUSTOMER_ID` is only a stable local grouping id",
	} {
		assertContainsWords(t, onboarding, want)
	}

	assertOrder(t, onboarding, "Customer/workstream:", "CUSTOMER_ID=<customer-or-workstream-id>")
	assertOrder(t, onboarding, "Project id:", "DETENT_PROJECT_ID=<local-detent-project-id>")
	assertOrder(t, onboarding, "Target repository:", "TARGET_REPOSITORY=<repo-owner>/<repo-name>")
	assertOrder(t, onboarding, "Source checkout:", "TARGET_SOURCE_ROOT=<absolute-local-checkout-path>")
}

func TestOnboardingDocsDefaultProjectKanbanIntegrationForOperatorOwnedAddProject(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		"Keep fleet `/kanban` read-only",
		"operator-owned local or private Detent instance",
		"For `GITHUB_MODE=label` add-project onboarding on an operator-owned local or private Detent instance, recommend `KANBAN_MODE=integration` even when the pre-mutation `detent doctor --port 0` skipped write probes",
		"Skipped pre-mutation write probes must not become a `read_only` recommendation",
		"Ask or select the delivery profile before emitting low-level `KANBAN_MODE` defaults",
		"shared observer dashboard",
		"failed post-authorization write probes",
		`KANBAN_MODE="${KANBAN_MODE:?set KANBAN_MODE to read_only or integration from answers.env}"`,
		"mode: ${KANBAN_MODE}",
	} {
		assertContainsWords(t, onboarding, want)
	}

	for _, want := range []string{
		"fleet `/kanban` board stays read-only",
		"trusted operator project board should default to `integration` before mutation authorization",
		"Skipped pre-mutation write probes are not evidence for `read_only`",
		"observer or shared dashboard",
		"server.kanban.mode: integration",
	} {
		assertContainsWords(t, readme, want)
	}

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content := readRepositoryTextFile(t, path)
			assertContains(t, content, "mode: integration")
			assertContains(t, content, "allowed_transitions")

			workflow, err := workflowconfig.ParseWorkflow([]byte(content))
			if err != nil {
				t.Fatalf("ParseWorkflow(%s) error = %v", path, err)
			}
			if workflow.Config.Server.Kanban.Mode != workflowconfig.KanbanModeIntegration {
				t.Fatalf("Kanban mode = %q, want %q", workflow.Config.Server.Kanban.Mode, workflowconfig.KanbanModeIntegration)
			}
		})
	}
}

func TestOnboardingDocsPresentDeliveryProfilesBeforeAnswersEnvFields(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")

	for _, want := range []string{
		"Present the operator's plain-English operating model first, then show the canonical `answers.env` fields.",
		"Conservative review: stop at `Human Review` until a human explicitly promotes work.",
		"Autonomous delivery: move Todo work through PR, CI/gates, Merging, and Done without human review or quiet-window delay when there are no real blockers.",
		"Custom/advanced: expose the underlying fields for teams that need a mixed policy.",
		"When the operator says \"no human review, no wait state, unblock aggressively, only stop on real blockers,\" recommend `autonomous_delivery`.",
	} {
		assertContainsWords(t, onboarding, want)
	}

	assertOrder(t, onboarding, "Conservative review:", "DELIVERY_PROFILE=<conservative_review|autonomous_delivery>")
	assertOrder(t, onboarding, "Autonomous delivery:", "DELIVERY_PROFILE=<conservative_review|autonomous_delivery>")
	assertOrder(t, onboarding, "Custom/advanced:", "DELIVERY_PROFILE=<conservative_review|autonomous_delivery>")
}

func TestOnboardingDocsSkipDuplicateProfileSuppliedQuestions(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")

	for _, want := range []string{
		"After selecting `DELIVERY_PROFILE=autonomous_delivery`, do not ask the Kanban interaction, validation-gate automated-review, Merging concurrency, review policy, or dependency waiting policy questions again unless the operator asks for an advanced override.",
		"Advanced override means the operator switches to Custom/advanced after seeing the expansion; remove or omit `DELIVERY_PROFILE` before recording a profile-supplied key with a different value.",
		"Ask the remaining Phase 2 questions that are not supplied by the selected profile.",
	} {
		assertContainsWords(t, onboarding, want)
	}
}

func TestWorkflowTemplatesAreCurrentAndModeSpecific(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")
	staleCanonicalURL := "https://raw.githubusercontent.com/digitaldrywood/detent-orchestration/main/WORKFLOW.md"
	if strings.Contains(onboarding, staleCanonicalURL) || strings.Contains(readme, staleCanonicalURL) {
		t.Fatalf("docs still reference stale canonical template URL %q", staleCanonicalURL)
	}

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		assertContains(t, onboarding, path)
		assertContains(t, readme, path)
	}

	tests := []struct {
		path             string
		source           string
		want             []string
		unwanted         []string
		wantProjectSlug  bool
		wantRepository   bool
		wantStatusField  string
		wantStatusPrefix string
	}{
		{
			path:            "docs/templates/WORKFLOW.project_v2.md",
			source:          workflowconfig.GitHubStatusSourceProjectV2,
			want:            []string{"github_status_source: project_v2", "project_slug: <project-node-id>"},
			unwanted:        []string{"repository: <repo-owner>/<repo-name>"},
			wantProjectSlug: true,
		},
		{
			path:            "docs/templates/WORKFLOW.issue_field.md",
			source:          workflowconfig.GitHubStatusSourceIssueField,
			want:            []string{"github_status_source: issue_field", "repository: <repo-owner>/<repo-name>", "status_field: Status"},
			unwanted:        []string{"project_slug:"},
			wantRepository:  true,
			wantStatusField: "Status",
		},
		{
			path:             "docs/templates/WORKFLOW.label.md",
			source:           workflowconfig.GitHubStatusSourceLabel,
			want:             []string{"github_status_source: label", "repository: <repo-owner>/<repo-name>", `status_label_prefix: "detent:"`},
			unwanted:         []string{"project_slug:", "status_field:"},
			wantRepository:   true,
			wantStatusPrefix: "detent:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			t.Parallel()

			content := strings.ReplaceAll(readRepositoryTextFile(t, tt.path), "\r\n", "\n")
			for _, want := range tt.want {
				assertContains(t, content, want)
			}
			for _, unwanted := range append(tt.unwanted, "endpoint:", "api_key:", "interval_ms: 15000") {
				if strings.Contains(content, unwanted) {
					t.Fatalf("%s contains stale or wrong field %q:\n%s", tt.path, unwanted, content)
				}
			}

			workflow, err := workflowconfig.ParseWorkflow([]byte(content))
			if err != nil {
				t.Fatalf("ParseWorkflow(%s) error = %v", tt.path, err)
			}
			cfg := workflow.Config
			cfg.Tracker.APIKey = "test-token"
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%s) error = %v", tt.path, err)
			}

			if cfg.Tracker.GitHubStatusSource != tt.source {
				t.Fatalf("GitHubStatusSource = %q, want %q", cfg.Tracker.GitHubStatusSource, tt.source)
			}
			if cfg.Polling.IntervalMS != workflowconfig.DefaultPollingIntervalMS {
				t.Fatalf("Polling.IntervalMS = %d, want %d", cfg.Polling.IntervalMS, workflowconfig.DefaultPollingIntervalMS)
			}
			assertContains(t, content, "max_concurrent_agents_by_state:\n    Merging: 1")
			if cfg.Agent.MaxConcurrentAgentsByState["merging"] != 1 {
				t.Fatalf("Merging concurrency = %d, want 1", cfg.Agent.MaxConcurrentAgentsByState["merging"])
			}
			if tt.wantProjectSlug && strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" {
				t.Fatal("ProjectSlug is blank")
			}
			if tt.wantRepository && strings.TrimSpace(cfg.Tracker.Repository) == "" {
				t.Fatal("Repository is blank")
			}
			if tt.wantStatusField != "" && cfg.Tracker.StatusField != tt.wantStatusField {
				t.Fatalf("StatusField = %q, want %q", cfg.Tracker.StatusField, tt.wantStatusField)
			}
			if tt.wantStatusPrefix != "" && cfg.Tracker.StatusLabelPrefix != tt.wantStatusPrefix {
				t.Fatalf("StatusLabelPrefix = %q, want %q", cfg.Tracker.StatusLabelPrefix, tt.wantStatusPrefix)
			}
		})
	}
}

func TestWorkflowTemplatesRecommendRequiredExecutionFlow(t *testing.T) {
	t.Parallel()

	onboarding := readRepositoryTextFile(t, "docs/ONBOARDING.md")
	readme := readRepositoryTextFile(t, "README.md")

	for _, want := range []string{
		"`## Required Execution Flow`",
		"`For Todo`",
		"`For In Progress`",
		"`For Rework`",
		"`For Merging`",
		"`$go-workflow:ship`",
		"`gh pr merge` directly outside ship",
		"Codex environment exposes `$go-workflow:ship`",
		"issue remaining in `Merging` with a concrete external blocker recorded",
		"Current Detent status: {{ issue.state }}",
	} {
		assertContainsWords(t, onboarding, want)
	}

	for _, want := range []string{
		"`## Required Execution Flow`",
		"`For Todo`",
		"`For In Progress`",
		"`For Rework`",
		"`For Merging`",
		"invoke `$go-workflow:ship`",
		"Codex environment exposes `$go-workflow:ship`",
		"`Done`",
		"`Rework` with an actionable defect",
		"`Merging` with a concrete external blocker recorded",
		"Current Detent status: {{ issue.state }}",
	} {
		assertContainsWords(t, readme, want)
	}

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content := readRepositoryTextFile(t, path)
			for _, want := range []string{
				"## Required Execution Flow",
				"Current Detent status: {{ issue.state }}",
				"### For Todo",
				"### For In Progress",
				"### For Rework",
				"### For Merging",
				"Move the issue to `In Progress`.",
				"Move the issue to `Human Review` only after the pull request is open",
				"Confirm `$go-workflow:ship` is available in the Codex environment.",
				"record the missing ship workflow",
				"as an external blocker",
				"Invoke and follow `$go-workflow:ship`.",
				"Do not call `gh pr merge` directly outside the ship workflow.",
				"pull request merged and issue moved to `Done`",
				"issue moved to `Rework` with an actionable defect",
				"issue remains in `Merging` with a concrete external blocker recorded",
				"Move the issue to `Done` only after the pull request is merged.",
			} {
				assertContains(t, content, want)
			}

			assertOrder(t, content, "### For Todo", "### For In Progress")
			assertOrder(t, content, "### For In Progress", "### For Rework")
			assertOrder(t, content, "### For Rework", "### For Merging")
		})
	}
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

func assertOrder(t *testing.T, text string, before string, after string) {
	t.Helper()

	beforeIndex := strings.Index(text, before)
	if beforeIndex == -1 {
		t.Fatalf("document missing %q", before)
	}
	afterIndex := strings.Index(text, after)
	if afterIndex == -1 {
		t.Fatalf("document missing %q", after)
	}
	if beforeIndex > afterIndex {
		t.Fatalf("document places %q after %q", before, after)
	}
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
