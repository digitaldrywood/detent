package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestOnboardingValidateAnswersCommandRejectsMissingGitHubMode(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "GITHUB_MODE must be project_v2, issue_field, or label") {
		t.Fatalf("Execute() error missing GITHUB_MODE validation:\n%s", err.Error())
	}
}

func TestOnboardingValidateAnswersCommandAcceptsIdentityPhase(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath, "--phase", "identity"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Status                string   `json:"status"`
		Phase                 string   `json:"phase"`
		CustomerID            string   `json:"customer_id"`
		DetentProjectID       string   `json:"detent_project_id"`
		TargetRepository      string   `json:"target_repository"`
		TargetSourceRoot      string   `json:"target_source_root"`
		ReferenceRepositories []string `json:"reference_repositories"`
		DetentOnboardingMode  string   `json:"detent_onboarding_mode"`
		IdentityConfirmed     bool     `json:"identity_confirmed"`
		MutationConfirmed     bool     `json:"mutation_confirmed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "ok" || got.Phase != "identity" || !got.IdentityConfirmed || got.MutationConfirmed {
		t.Fatalf("validation result = %#v, want accepted identity phase", got)
	}
	if got.CustomerID != "digitaldrywood" || got.DetentProjectID != "detent" || got.TargetRepository != "digitaldrywood/detent" {
		t.Fatalf("identity fields = %#v, want explicit target identity", got)
	}
	if got.TargetSourceRoot == "" || got.DetentOnboardingMode != "add-project" {
		t.Fatalf("identity fields = %#v, want source root and onboarding mode", got)
	}
	if len(got.ReferenceRepositories) != 2 || got.ReferenceRepositories[0] != "digitaldrywood/detent-orchestration" || got.ReferenceRepositories[1] != "corylanou/website-template" {
		t.Fatalf("reference repositories = %#v, want explicit references", got.ReferenceRepositories)
	}
}

func TestOnboardingValidateAnswersCommandRejectsGitHubModeBeforeIdentity(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, "GITHUB_MODE=label\n")
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath, "--phase", "decision"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want validation error")
	}
	for _, want := range []string{
		"CUSTOMER_ID is required",
		"GITHUB_MODE cannot be set before identity answers are valid",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Execute() error missing %q:\n%s", want, err.Error())
		}
	}
}

func TestOnboardingValidateAnswersCommandRejectsWrongTargetRemote(t *testing.T) {
	t.Parallel()

	sourceRoot := initOnboardingGitRepository(t, "https://github.com/example/other.git")
	answersPath := writeOnboardingAnswers(t, strings.Join([]string{
		"CUSTOMER_ID=digitaldrywood",
		"DETENT_PROJECT_ID=detent",
		"TARGET_REPOSITORY=digitaldrywood/detent",
		"TARGET_SOURCE_ROOT=" + sourceRoot,
		"REFERENCE_REPOSITORIES=",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath, "--phase", "identity"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "TARGET_SOURCE_ROOT origin remote must match TARGET_REPOSITORY digitaldrywood/detent") {
		t.Fatalf("Execute() error missing remote mismatch validation:\n%s", err.Error())
	}
}

func TestOnboardingValidateAnswersCommandRequiresFinalMutationConfirmation(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"STATUS_LABEL_PREFIX=custom:",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "MUTATION_CONFIRMED=true must be the final nonblank line") {
		t.Fatalf("Execute() error missing final confirmation validation:\n%s", err.Error())
	}
}

func TestOnboardingValidateAnswersCommandRequiresModeSpecificMutationAnswers(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=project_v2",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "BOARD_MODE must be reuse or create for GITHUB_MODE=project_v2") {
		t.Fatalf("Execute() error missing board mode validation:\n%s", err.Error())
	}
}

func TestOnboardingValidateAnswersCommandRejectsMissingDeliveryProfileExpansionBeforeMutation(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "validate-answers", "--answers", answersPath})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want missing delivery profile expansion validation error")
	}
	for _, want := range []string{
		"AUTO_PROMOTE_ENABLED is required when DELIVERY_PROFILE=full_autopilot",
		"KANBAN_MODE is required when DELIVERY_PROFILE=full_autopilot",
		"detent onboarding normalize-answers --answers",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Execute() error missing %q:\n%s", want, err.Error())
		}
	}
}

func TestOnboardingValidateAnswersCommandAcceptsDecisionPhase(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+"GITHUB_MODE=issue_field\n")
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath, "--phase", "decision"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Status            string `json:"status"`
		Phase             string `json:"phase"`
		GitHubMode        string `json:"github_mode"`
		MutationConfirmed bool   `json:"mutation_confirmed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "ok" || got.Phase != "decision" || got.GitHubMode != "issue_field" || got.MutationConfirmed {
		t.Fatalf("validation result = %#v, want accepted decision phase", got)
	}
}

func TestOnboardingValidateAnswersCommandAcceptsLabelMode(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Status            string `json:"status"`
		Path              string `json:"path"`
		Phase             string `json:"phase"`
		GitHubMode        string `json:"github_mode"`
		MutationConfirmed bool   `json:"mutation_confirmed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "ok" || got.Path != answersPath || got.Phase != "mutation" || got.GitHubMode != "label" || !got.MutationConfirmed {
		t.Fatalf("validation result = %#v, want accepted label mutation", got)
	}
}

func TestOnboardingValidateAnswersCommandExpandsFullAutopilotProfile(t *testing.T) {
	t.Parallel()

	profileAnswers := fullAutopilotProfileAnswers()
	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"KANBAN_MODE=" + profileAnswers["KANBAN_MODE"],
		"AUTO_PROMOTE_ENABLED=" + profileAnswers["AUTO_PROMOTE_ENABLED"],
		"AUTO_PROMOTE_QUIET_SECONDS=" + profileAnswers["AUTO_PROMOTE_QUIET_SECONDS"],
		"GATE_REQUIRE_AUTOMATED_REVIEW=" + profileAnswers["GATE_REQUIRE_AUTOMATED_REVIEW"],
		"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW=" + profileAnswers["AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW"],
		"DEPENDENCY_AUTO_UNBLOCK_ENABLED=" + profileAnswers["DEPENDENCY_AUTO_UNBLOCK_ENABLED"],
		"MERGING_CONCURRENCY=" + profileAnswers["MERGING_CONCURRENCY"],
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		DeliveryProfile        string            `json:"delivery_profile"`
		DeliveryProfileAnswers map[string]string `json:"delivery_profile_answers"`
		AnswersSummary         struct {
			EffectiveDeliveryProfile      string   `json:"effective_delivery_profile"`
			EffectiveDeliveryProfileLabel string   `json:"effective_delivery_profile_label"`
			KanbanMode                    string   `json:"kanban_mode"`
			GateBehavior                  string   `json:"gate_behavior"`
			AutoPromotionBehavior         string   `json:"auto_promotion_behavior"`
			QuietWindowBehavior           string   `json:"quiet_window_behavior"`
			DependencyAutoUnblockBehavior string   `json:"dependency_auto_unblock_behavior"`
			MergeConcurrencyBehavior      string   `json:"merge_concurrency_behavior"`
			StopConditions                []string `json:"stop_conditions"`
		} `json:"answers_summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.DeliveryProfile != "full_autopilot" {
		t.Fatalf("DeliveryProfile = %q, want full_autopilot", got.DeliveryProfile)
	}
	for key, value := range profileAnswers {
		if got.DeliveryProfileAnswers[key] != value {
			t.Fatalf("DeliveryProfileAnswers[%q] = %q, want %q; all answers = %#v", key, got.DeliveryProfileAnswers[key], value, got.DeliveryProfileAnswers)
		}
	}
	if got.AnswersSummary.EffectiveDeliveryProfile != "full_autopilot" ||
		got.AnswersSummary.EffectiveDeliveryProfileLabel != "Full autopilot" ||
		got.AnswersSummary.KanbanMode != "integration" {
		t.Fatalf("AnswersSummary identity = %#v, want full autopilot integration summary", got.AnswersSummary)
	}
	summaryValues := []string{
		got.AnswersSummary.GateBehavior,
		got.AnswersSummary.AutoPromotionBehavior,
		got.AnswersSummary.QuietWindowBehavior,
		got.AnswersSummary.DependencyAutoUnblockBehavior,
		got.AnswersSummary.MergeConcurrencyBehavior,
	}
	for _, want := range []string{
		"No automated GitHub PR review is required when the command gate is passing and the workflow says so.",
		"Detent automatically promotes eligible work from `Human Review` to `Merging` when the linked PR, local gate, CI, and guardrails pass.",
		"There is no quiet-window delay before promotion.",
		"Dependency-waiting `Blocked` issues can move back to `Todo` when declared blockers are terminal or merged.",
		"`Merging` remains serialized for this project.",
	} {
		if !containsString(summaryValues, want) {
			t.Fatalf("AnswersSummary missing %q: %#v", want, got.AnswersSummary)
		}
	}
	if !containsString(got.AnswersSummary.StopConditions, "gate failures") {
		t.Fatalf("StopConditions = %#v, want gate failures", got.AnswersSummary.StopConditions)
	}
}

func TestOnboardingExplainAnswersCommandSummarizesFullAutopilot(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "onboarding", "explain-answers", "--answers", answersPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	summaryIndex := strings.Index(output, "Onboarding answer behavior summary:")
	canonicalIndex := strings.Index(output, "Canonical delivery answer keys:")
	if summaryIndex < 0 || canonicalIndex < 0 {
		t.Fatalf("output missing summary or canonical keys:\n%s", output)
	}
	if summaryIndex > canonicalIndex {
		t.Fatalf("summary appears after canonical keys:\n%s", output)
	}
	for _, want := range []string{
		"Effective delivery profile: Full autopilot (`full_autopilot`).",
		"No automated GitHub PR review is required when the command gate is passing and the workflow says so.",
		"Detent automatically promotes eligible work from `Human Review` to `Merging` when the linked PR, local gate, CI, and guardrails pass.",
		"There is no quiet-window delay before promotion.",
		"Dependency-waiting `Blocked` issues can move back to `Todo` when declared blockers are terminal or merged.",
		"`Merging` remains serialized for this project.",
		"Existing validation, CI, unresolved review feedback, dependency blockers, mergeability, and gate failures still stop progress.",
		"DELIVERY_PROFILE=full_autopilot",
		"AUTO_PROMOTE_QUIET_SECONDS=0",
		"GATE_REQUIRE_AUTOMATED_REVIEW=false",
		"MERGING_CONCURRENCY=1",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestOnboardingValidateAnswersCommandSummarizesReviewGateProfile(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=review_gate",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath, "--phase", "decision"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		AnswersSummary struct {
			EffectiveDeliveryProfile      string `json:"effective_delivery_profile"`
			EffectiveDeliveryProfileLabel string `json:"effective_delivery_profile_label"`
			KanbanMode                    string `json:"kanban_mode"`
			GateRequiresAutomatedReview   bool   `json:"gate_requires_automated_review"`
			GateBehavior                  string `json:"gate_behavior"`
			AutoPromoteEnabled            bool   `json:"auto_promote_enabled"`
			AutoPromoteQuietSeconds       int    `json:"auto_promote_quiet_seconds"`
			AutoPromotionBehavior         string `json:"auto_promotion_behavior"`
			QuietWindowBehavior           string `json:"quiet_window_behavior"`
			DependencyAutoUnblockEnabled  bool   `json:"dependency_auto_unblock_enabled"`
			DependencyAutoUnblockBehavior string `json:"dependency_auto_unblock_behavior"`
			MergeConcurrencyBehavior      string `json:"merge_concurrency_behavior"`
		} `json:"answers_summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	summary := got.AnswersSummary
	if summary.EffectiveDeliveryProfile != "review_gate" ||
		summary.EffectiveDeliveryProfileLabel != "Review gate" ||
		summary.KanbanMode != "integration" ||
		summary.GateRequiresAutomatedReview ||
		summary.AutoPromoteEnabled ||
		summary.AutoPromoteQuietSeconds != 600 ||
		summary.DependencyAutoUnblockEnabled {
		t.Fatalf("AnswersSummary = %#v, want review gate defaults", summary)
	}
	summaryValues := []string{
		summary.GateBehavior,
		summary.AutoPromotionBehavior,
		summary.QuietWindowBehavior,
		summary.DependencyAutoUnblockBehavior,
		summary.MergeConcurrencyBehavior,
	}
	for _, want := range []string{
		"No automated GitHub PR review is required when the command gate is passing and the workflow says so.",
		"Detent stops in `Human Review` until an operator approves promotion to `Merging`.",
		"Auto-promotion is disabled; the 600-second quiet window only matters if auto-promotion is enabled later.",
		"Dependency-waiting `Blocked` issues remain `Blocked` until a human or workflow moves them.",
		"`Merging` remains serialized for this project.",
	} {
		if !containsString(summaryValues, want) {
			t.Fatalf("AnswersSummary missing %q: %#v", want, summary)
		}
	}
}

func TestOnboardingValidateAnswersCommandSummarizesConservativeManualProfile(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=conservative_manual",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "validate-answers", "--answers", answersPath, "--phase", "decision"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		AnswersSummary struct {
			EffectiveDeliveryProfile      string `json:"effective_delivery_profile"`
			EffectiveDeliveryProfileLabel string `json:"effective_delivery_profile_label"`
			KanbanMode                    string `json:"kanban_mode"`
			GateRequiresAutomatedReview   bool   `json:"gate_requires_automated_review"`
			AutoPromoteEnabled            bool   `json:"auto_promote_enabled"`
			DependencyAutoUnblockEnabled  bool   `json:"dependency_auto_unblock_enabled"`
			AutoPromotionBehavior         string `json:"auto_promotion_behavior"`
		} `json:"answers_summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	summary := got.AnswersSummary
	if summary.EffectiveDeliveryProfile != "conservative_manual" ||
		summary.EffectiveDeliveryProfileLabel != "Conservative/manual" ||
		summary.KanbanMode != "read_only" ||
		!summary.GateRequiresAutomatedReview ||
		summary.AutoPromoteEnabled ||
		summary.DependencyAutoUnblockEnabled {
		t.Fatalf("AnswersSummary = %#v, want conservative/manual defaults", summary)
	}
	if summary.AutoPromotionBehavior != "Detent stops in `Human Review` until an operator approves promotion to `Merging`." {
		t.Fatalf("AutoPromotionBehavior = %q, want operator approval summary", summary.AutoPromotionBehavior)
	}
}

func TestOnboardingNormalizeAnswersCommandWritesFullAutopilotProfileExpansion(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"STATUS_LABEL_PREFIX=detent:",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "normalize-answers", "--answers", answersPath, "--write"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\nstdout:\n%s", err, stdout.String())
	}

	var got struct {
		Status            string `json:"status"`
		Path              string `json:"path"`
		Written           bool   `json:"written"`
		Changed           bool   `json:"changed"`
		DeliveryProfile   string `json:"delivery_profile"`
		MutationConfirmed bool   `json:"mutation_confirmed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "ok" || got.Path != answersPath || !got.Written || !got.Changed || got.DeliveryProfile != "full_autopilot" || got.MutationConfirmed {
		t.Fatalf("normalization result = %#v, want written full autopilot profile normalization", got)
	}

	raw, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	for key, value := range fullAutopilotProfileAnswers() {
		want := key + "=" + value
		if !strings.Contains(content, want) {
			t.Fatalf("answers.env missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "MUTATION_CONFIRMED=true") {
		t.Fatalf("normalization must not add mutation confirmation:\n%s", content)
	}
}

func TestOnboardingNormalizeAnswersCommandPreservesFinalMutationConfirmationWhenAlreadyCanonical(t *testing.T) {
	t.Parallel()

	profileAnswers := fullAutopilotProfileAnswers()
	answersPath := writeOnboardingAnswers(t, validIdentityOnboardingAnswers(t)+strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"KANBAN_MODE=" + profileAnswers["KANBAN_MODE"],
		"AUTO_PROMOTE_ENABLED=" + profileAnswers["AUTO_PROMOTE_ENABLED"],
		"AUTO_PROMOTE_QUIET_SECONDS=" + profileAnswers["AUTO_PROMOTE_QUIET_SECONDS"],
		"GATE_REQUIRE_AUTOMATED_REVIEW=" + profileAnswers["GATE_REQUIRE_AUTOMATED_REVIEW"],
		"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW=" + profileAnswers["AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW"],
		"DEPENDENCY_AUTO_UNBLOCK_ENABLED=" + profileAnswers["DEPENDENCY_AUTO_UNBLOCK_ENABLED"],
		"MERGING_CONCURRENCY=" + profileAnswers["MERGING_CONCURRENCY"],
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n"))
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "onboarding", "normalize-answers", "--answers", answersPath, "--write"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\nstdout:\n%s", err, stdout.String())
	}

	var got struct {
		Written           bool `json:"written"`
		Changed           bool `json:"changed"`
		MutationConfirmed bool `json:"mutation_confirmed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if !got.Written || got.Changed || !got.MutationConfirmed {
		t.Fatalf("normalization result = %#v, want no-op write with final confirmation preserved", got)
	}
	raw, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	if last := lastNonblankOnboardingLine(content); last != "MUTATION_CONFIRMED=true" {
		t.Fatalf("last nonblank line = %q, want MUTATION_CONFIRMED=true\n%s", last, content)
	}
	if strings.Index(content, "MERGING_CONCURRENCY=1") > strings.Index(content, "MUTATION_CONFIRMED=true") {
		t.Fatalf("profile expansion was written after final mutation confirmation:\n%s", content)
	}
}

func TestOnboardingNormalizeAnswersCommandRejectsStaleMutationConfirmation(t *testing.T) {
	t.Parallel()

	content := validIdentityOnboardingAnswers(t) + strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n")
	answersPath := writeOnboardingAnswers(t, content)
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "normalize-answers", "--answers", answersPath, "--write"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want stale mutation confirmation validation error")
	}
	for _, want := range []string{
		"MUTATION_CONFIRMED=true is already present",
		"rerun detent onboarding normalize-answers",
		"record a fresh confirmation",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Execute() error missing %q:\n%s", want, err.Error())
		}
	}
	raw, readErr := os.ReadFile(answersPath)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(raw) != content {
		t.Fatalf("answers.env changed after stale confirmation error:\n%s", string(raw))
	}
}

func TestOnboardingNormalizeAnswersCommandRejectsDeliveryProfileExpansionConflict(t *testing.T) {
	t.Parallel()

	content := validIdentityOnboardingAnswers(t) + strings.Join([]string{
		"GITHUB_MODE=label",
		"DELIVERY_PROFILE=full_autopilot",
		"KANBAN_MODE=read_only",
		"STATUS_LABEL_PREFIX=detent:",
		"MUTATION_CONFIRMED=true",
		"",
	}, "\n")
	answersPath := writeOnboardingAnswers(t, content)
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"onboarding", "normalize-answers", "--answers", answersPath, "--write"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want delivery profile expansion conflict")
	}
	if !strings.Contains(err.Error(), "KANBAN_MODE=read_only conflicts with DELIVERY_PROFILE=full_autopilot, which expands KANBAN_MODE=integration") {
		t.Fatalf("Execute() error missing profile conflict:\n%s", err.Error())
	}
	raw, readErr := os.ReadFile(answersPath)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(raw) != content {
		t.Fatalf("answers.env changed after conflict:\n%s", string(raw))
	}
}

func TestOnboardingDraftAnswersCommandUsesCurrentNonDetentCheckoutAsTarget(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	wantTargetRoot := canonicalOnboardingTestPath(t, targetRoot)
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Status                         string   `json:"status"`
		CustomerIDCandidate            string   `json:"customer_id_candidate"`
		DetentProjectIDCandidate       string   `json:"detent_project_id_candidate"`
		TargetRepositoryCandidate      string   `json:"target_repository_candidate"`
		TargetSourceRootCandidate      string   `json:"target_source_root_candidate"`
		ReferenceRepositoriesCandidate []string `json:"reference_repositories_candidate"`
		DetentOnboardingModeCandidate  string   `json:"detent_onboarding_mode_candidate"`
		Confidence                     string   `json:"confidence"`
		Notes                          []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "draft" {
		t.Fatalf("status = %q, want draft", got.Status)
	}
	if got.CustomerIDCandidate != "acme" || got.DetentProjectIDCandidate != "api" {
		t.Fatalf("identity candidates = %#v, want customer acme and project api", got)
	}
	if got.TargetRepositoryCandidate != "acme/api" || got.TargetSourceRootCandidate != wantTargetRoot {
		t.Fatalf("target candidates = %#v, want current checkout", got)
	}
	if len(got.ReferenceRepositoriesCandidate) != 1 || got.ReferenceRepositoriesCandidate[0] != "digitaldrywood/detent" {
		t.Fatalf("reference repositories = %#v, want Detent source reference", got.ReferenceRepositoriesCandidate)
	}
	if got.DetentOnboardingModeCandidate != "new-install" {
		t.Fatalf("detent onboarding mode = %q, want new-install", got.DetentOnboardingModeCandidate)
	}
	if got.Confidence == "" || len(got.Notes) == 0 {
		t.Fatalf("confidence/notes = %q/%#v, want review guidance", got.Confidence, got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandPrefersRepoPrefixForSharedOwner(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/creswoodcorners-phone.git")
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		CustomerIDCandidate       string   `json:"customer_id_candidate"`
		CustomerIDSource          string   `json:"customer_id_source"`
		CustomerIDConfidence      string   `json:"customer_id_confidence"`
		CustomerIDReviewRequired  bool     `json:"customer_id_review_required"`
		CustomerIDAlternatives    []string `json:"customer_id_alternatives"`
		DetentProjectIDCandidate  string   `json:"detent_project_id_candidate"`
		DetentProjectIDSource     string   `json:"detent_project_id_source"`
		TargetRepositoryCandidate string   `json:"target_repository_candidate"`
		Confidence                string   `json:"confidence"`
		Notes                     []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.CustomerIDCandidate != "creswoodcorners" {
		t.Fatalf("customer id candidate = %q, want repo prefix creswoodcorners", got.CustomerIDCandidate)
	}
	if got.CustomerIDSource != "repo_prefix" || got.CustomerIDConfidence != "medium" || got.CustomerIDReviewRequired {
		t.Fatalf("customer id metadata = source %q confidence %q review %t, want repo_prefix medium without ambiguity", got.CustomerIDSource, got.CustomerIDConfidence, got.CustomerIDReviewRequired)
	}
	if !containsString(got.CustomerIDAlternatives, "digitaldrywood") {
		t.Fatalf("customer id alternatives = %#v, want owner alternative", got.CustomerIDAlternatives)
	}
	if got.DetentProjectIDCandidate != "creswoodcorners-phone" || got.DetentProjectIDSource != "repo_name" {
		t.Fatalf("project id metadata = %#v, want repo-name project", got)
	}
	if got.TargetRepositoryCandidate != "digitaldrywood/creswoodcorners-phone" || got.Confidence != "medium" {
		t.Fatalf("draft result = %#v, want medium-confidence target repository draft", got)
	}
	if !containsSubstring(got.Notes, `customer id candidate "creswoodcorners" came from repository prefix before suffix "phone"`) {
		t.Fatalf("notes = %#v, want repo-prefix explanation", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandKeepsNormalOwnerForProductSuffixRepo(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/payments-api.git")
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		CustomerIDCandidate      string `json:"customer_id_candidate"`
		CustomerIDSource         string `json:"customer_id_source"`
		CustomerIDConfidence     string `json:"customer_id_confidence"`
		CustomerIDReviewRequired bool   `json:"customer_id_review_required"`
		DetentProjectIDCandidate string `json:"detent_project_id_candidate"`
		DetentProjectIDSource    string `json:"detent_project_id_source"`
		Confidence               string `json:"confidence"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.CustomerIDCandidate != "acme" || got.CustomerIDSource != "owner" {
		t.Fatalf("customer id metadata = %#v, want normal owner candidate", got)
	}
	if got.CustomerIDConfidence != "medium" || got.CustomerIDReviewRequired || got.Confidence != "medium" {
		t.Fatalf("confidence = customer %q review %t overall %q, want medium owner draft", got.CustomerIDConfidence, got.CustomerIDReviewRequired, got.Confidence)
	}
	if got.DetentProjectIDCandidate != "payments-api" || got.DetentProjectIDSource != "repo_name" {
		t.Fatalf("project id metadata = %#v, want repo-name project", got)
	}
}

func TestOnboardingDraftAnswersCommandMarksSharedOwnerAmbiguity(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/service.git")
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		CustomerIDCandidate      string   `json:"customer_id_candidate"`
		CustomerIDSource         string   `json:"customer_id_source"`
		CustomerIDConfidence     string   `json:"customer_id_confidence"`
		CustomerIDReviewRequired bool     `json:"customer_id_review_required"`
		CustomerIDAlternatives   []string `json:"customer_id_alternatives"`
		Confidence               string   `json:"confidence"`
		Notes                    []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.CustomerIDCandidate != "service" || got.CustomerIDSource != "repo_name" {
		t.Fatalf("customer id metadata = %#v, want repo-name candidate for shared owner", got)
	}
	if got.CustomerIDConfidence != "needs-review" || !got.CustomerIDReviewRequired || got.Confidence != "needs-review" {
		t.Fatalf("confidence = customer %q review %t overall %q, want needs-review ambiguity", got.CustomerIDConfidence, got.CustomerIDReviewRequired, got.Confidence)
	}
	if !containsString(got.CustomerIDAlternatives, "digitaldrywood") {
		t.Fatalf("customer id alternatives = %#v, want shared owner alternative", got.CustomerIDAlternatives)
	}
	if !containsSubstring(got.Notes, "customer id candidate needs operator review because owner digitaldrywood looks like a shared operator") {
		t.Fatalf("notes = %#v, want shared-owner ambiguity note", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandPrettyExplainsCustomerChoice(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/creswoodcorners-phone.git")
	wantTargetRoot := canonicalOnboardingTestPath(t, targetRoot)
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--output", "pretty", "--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"I found a likely target checkout from the current shell:",
		"Customer/workstream: `creswoodcorners`",
		"customer_id_source=repo_prefix",
		"customer_id_confidence=medium",
		"Customer/workstream alternatives: `digitaldrywood`",
		"`CUSTOMER_ID` is only a stable local grouping id for this Detent install.",
		"answers.env preview:",
		"CUSTOMER_ID=creswoodcorners",
		"DETENT_PROJECT_ID=creswoodcorners-phone",
		"TARGET_REPOSITORY=digitaldrywood/creswoodcorners-phone",
		"TARGET_SOURCE_ROOT=" + wantTargetRoot,
		"IDENTITY_CONFIRMED=false",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("pretty output missing %q:\n%s", want, output)
		}
	}
}

func TestOnboardingDraftAnswersCommandReportsStaleDetentSourceAndBinary(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	sourceRoot, sourceHead, canonicalMain := initOnboardingDetentSourceCheckout(t, true)
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(
		context.Background(),
		cli.WithStdoutTTY(func() bool { return false }),
		cli.WithCommandRunner(onboardingTestCommandRunner(detentVersionJSON(sourceHead))),
	)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"--config", filepath.Join(t.TempDir(), "global.yaml"),
		"onboarding", "draft-answers",
		"--detent-source-root", sourceRoot,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Confidence      string `json:"confidence"`
		DetentFreshness struct {
			SourceChecked                bool   `json:"source_checked"`
			SourceHead                   string `json:"source_head"`
			CanonicalMain                string `json:"canonical_main"`
			SourceMatchesCanonical       bool   `json:"source_matches_canonical"`
			SourceStatus                 string `json:"source_status"`
			BinaryChecked                bool   `json:"binary_checked"`
			BinaryCommit                 string `json:"binary_commit"`
			BinaryMatchesCanonical       bool   `json:"binary_matches_canonical"`
			BinaryStatus                 string `json:"binary_status"`
			Phase2RecommendationsBlocked bool   `json:"phase2_recommendations_blocked"`
		} `json:"detent_freshness"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	freshness := got.DetentFreshness
	if !freshness.SourceChecked || freshness.SourceHead != sourceHead || freshness.CanonicalMain != canonicalMain {
		t.Fatalf("source freshness = %#v, want stale source %s behind %s", freshness, sourceHead, canonicalMain)
	}
	if freshness.SourceMatchesCanonical || freshness.SourceStatus != "stale" {
		t.Fatalf("source freshness = %#v, want stale mismatch", freshness)
	}
	if !freshness.BinaryChecked || freshness.BinaryCommit != sourceHead || freshness.BinaryMatchesCanonical || freshness.BinaryStatus != "stale" {
		t.Fatalf("binary freshness = %#v, want stale binary at %s", freshness, sourceHead)
	}
	if !freshness.Phase2RecommendationsBlocked || got.Confidence != "needs-review" {
		t.Fatalf("freshness = %#v confidence = %q, want blocked needs-review", freshness, got.Confidence)
	}
	if !containsSubstring(got.Notes, "read onboarding docs from GitHub at the canonical head before Phase 2 recommendations") {
		t.Fatalf("notes = %#v, want stale source stop guidance", got.Notes)
	}
	if !containsSubstring(got.Notes, "installed Detent binary commit differs from fetched origin/main") {
		t.Fatalf("notes = %#v, want stale binary reinstall guidance", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandReportsCurrentDetentSourceAndBinary(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	sourceRoot, sourceHead, canonicalMain := initOnboardingDetentSourceCheckout(t, false)
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(
		context.Background(),
		cli.WithStdoutTTY(func() bool { return false }),
		cli.WithCommandRunner(onboardingTestCommandRunner(detentVersionJSON(canonicalMain))),
	)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"--config", filepath.Join(t.TempDir(), "global.yaml"),
		"onboarding", "draft-answers",
		"--detent-source-root", sourceRoot,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		DetentFreshness struct {
			SourceHead                   string `json:"source_head"`
			CanonicalMain                string `json:"canonical_main"`
			SourceMatchesCanonical       bool   `json:"source_matches_canonical"`
			SourceStatus                 string `json:"source_status"`
			BinaryCommit                 string `json:"binary_commit"`
			BinaryMatchesCanonical       bool   `json:"binary_matches_canonical"`
			BinaryStatus                 string `json:"binary_status"`
			Phase2RecommendationsBlocked bool   `json:"phase2_recommendations_blocked"`
		} `json:"detent_freshness"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	freshness := got.DetentFreshness
	if sourceHead != canonicalMain {
		t.Fatalf("test setup source head = %s, canonical = %s, want equal", sourceHead, canonicalMain)
	}
	if freshness.SourceHead != sourceHead || freshness.CanonicalMain != canonicalMain || !freshness.SourceMatchesCanonical || freshness.SourceStatus != "current" {
		t.Fatalf("source freshness = %#v, want current source %s", freshness, canonicalMain)
	}
	if freshness.BinaryCommit != canonicalMain || !freshness.BinaryMatchesCanonical || freshness.BinaryStatus != "current" {
		t.Fatalf("binary freshness = %#v, want current binary %s", freshness, canonicalMain)
	}
	if freshness.Phase2RecommendationsBlocked {
		t.Fatalf("freshness = %#v, want Phase 2 recommendations allowed", freshness)
	}
	if !containsSubstring(got.Notes, "Detent source checkout matches fetched origin/main") {
		t.Fatalf("notes = %#v, want current source note", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandBlocksUnprovenDetentBinary(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	sourceRoot, _, canonicalMain := initOnboardingDetentSourceCheckout(t, false)
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(
		context.Background(),
		cli.WithStdoutTTY(func() bool { return false }),
		cli.WithCommandRunner(onboardingTestCommandRunner(detentVersionJSON("none"))),
	)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"--config", filepath.Join(t.TempDir(), "global.yaml"),
		"onboarding", "draft-answers",
		"--detent-source-root", sourceRoot,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Confidence      string `json:"confidence"`
		DetentFreshness struct {
			CanonicalMain                string `json:"canonical_main"`
			SourceStatus                 string `json:"source_status"`
			BinaryCommit                 string `json:"binary_commit"`
			BinaryStatus                 string `json:"binary_status"`
			Phase2RecommendationsBlocked bool   `json:"phase2_recommendations_blocked"`
		} `json:"detent_freshness"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	freshness := got.DetentFreshness
	if freshness.CanonicalMain != canonicalMain || freshness.SourceStatus != "current" {
		t.Fatalf("freshness = %#v, want current source at %s", freshness, canonicalMain)
	}
	if freshness.BinaryCommit != "none" || freshness.BinaryStatus != "unknown_commit" {
		t.Fatalf("freshness = %#v, want unknown binary commit", freshness)
	}
	if !freshness.Phase2RecommendationsBlocked || got.Confidence != "needs-review" {
		t.Fatalf("freshness = %#v confidence = %q, want blocked needs-review", freshness, got.Confidence)
	}
	if !containsSubstring(got.Notes, "installed Detent binary freshness could not be proven against fetched origin/main") {
		t.Fatalf("notes = %#v, want unproven binary stop guidance", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandAcceptsIdentityOverrides(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/creswoodcorners-phone.git")
	answersPath := filepath.Join(t.TempDir(), "answers.env")
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"--config", filepath.Join(t.TempDir(), "global.yaml"),
		"onboarding", "draft-answers",
		"--customer-id", "creswood",
		"--detent-project-id", "phone",
		"--answers", answersPath,
		"--write",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		CustomerIDCandidate      string `json:"customer_id_candidate"`
		CustomerIDSource         string `json:"customer_id_source"`
		CustomerIDConfidence     string `json:"customer_id_confidence"`
		CustomerIDReviewRequired bool   `json:"customer_id_review_required"`
		DetentProjectIDCandidate string `json:"detent_project_id_candidate"`
		DetentProjectIDSource    string `json:"detent_project_id_source"`
		Confidence               string `json:"confidence"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.CustomerIDCandidate != "creswood" || got.CustomerIDSource != "override" || got.CustomerIDConfidence != "explicit" || got.CustomerIDReviewRequired {
		t.Fatalf("customer override metadata = %#v, want explicit override", got)
	}
	if got.DetentProjectIDCandidate != "phone" || got.DetentProjectIDSource != "override" || got.Confidence != "medium" {
		t.Fatalf("project override metadata = %#v, want explicit project override with medium draft", got)
	}

	raw, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"CUSTOMER_ID=creswood",
		"DETENT_PROJECT_ID=phone",
		"TARGET_REPOSITORY=digitaldrywood/creswoodcorners-phone",
		"IDENTITY_CONFIRMED=false",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("answers.env missing %q:\n%s", want, content)
		}
	}
}

func TestOnboardingDraftAnswersCommandRequiresExplicitTargetFromDetentSourceCheckout(t *testing.T) {
	sourceRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/detent.git")
	t.Chdir(sourceRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "global.yaml"), "onboarding", "draft-answers"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want explicit target validation error")
	}
	if !strings.Contains(err.Error(), "current checkout is the Detent source repository") ||
		!strings.Contains(err.Error(), "--target-source-root") {
		t.Fatalf("Execute() error = %q, want explicit target guidance", err.Error())
	}
}

func TestOnboardingDraftAnswersCommandParsesGitHubRemoteFormats(t *testing.T) {
	t.Chdir(t.TempDir())

	tests := []struct {
		name       string
		remote     string
		repository string
	}{
		{name: "ssh", remote: "git@github.com:acme/api.git", repository: "acme/api"},
		{name: "https", remote: "https://github.com/acme/web.git", repository: "acme/web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetRoot := initOnboardingGitRepository(t, tt.remote)
			wantTargetRoot := canonicalOnboardingTestPath(t, targetRoot)
			cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{
				"--format", "json",
				"--config", filepath.Join(t.TempDir(), "global.yaml"),
				"onboarding", "draft-answers",
				"--target-source-root", targetRoot,
			})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var got struct {
				TargetRepositoryCandidate string `json:"target_repository_candidate"`
				TargetSourceRootCandidate string `json:"target_source_root_candidate"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if got.TargetRepositoryCandidate != tt.repository || got.TargetSourceRootCandidate != wantTargetRoot {
				t.Fatalf("target candidates = %#v, want %s at %s", got, tt.repository, wantTargetRoot)
			}
		})
	}
}

func TestOnboardingDraftAnswersCommandNotesProjectIDCollision(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	otherRoot := initOnboardingGitRepository(t, "https://github.com/acme/other.git")
	configPath := writeOnboardingGlobalConfig(t, []globalconfig.Project{{
		ID:          "api",
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     otherRoot,
		Weight:      1,
	}})
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", configPath, "onboarding", "draft-answers"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		DetentProjectIDCandidate      string   `json:"detent_project_id_candidate"`
		DetentOnboardingModeCandidate string   `json:"detent_onboarding_mode_candidate"`
		RegisteredProjectIDs          []string `json:"registered_project_ids"`
		Notes                         []string `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.DetentProjectIDCandidate != "api" || got.DetentOnboardingModeCandidate != "add-project" {
		t.Fatalf("draft = %#v, want colliding api candidate for add-project", got)
	}
	if len(got.RegisteredProjectIDs) != 1 || got.RegisteredProjectIDs[0] != "api" {
		t.Fatalf("registered project ids = %#v, want api", got.RegisteredProjectIDs)
	}
	if !containsSubstring(got.Notes, `project id candidate "api" already exists`) {
		t.Fatalf("notes = %#v, want project id collision note", got.Notes)
	}
}

func TestOnboardingDraftAnswersCommandWritesUnconfirmedAnswers(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	wantTargetRoot := canonicalOnboardingTestPath(t, targetRoot)
	answersPath := filepath.Join(t.TempDir(), "answers.env")
	t.Chdir(targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"--config", filepath.Join(t.TempDir(), "global.yaml"),
		"onboarding", "draft-answers",
		"--answers", answersPath,
		"--write",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	raw, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"CUSTOMER_ID=acme",
		"DETENT_PROJECT_ID=api",
		"TARGET_REPOSITORY=acme/api",
		"TARGET_SOURCE_ROOT=" + wantTargetRoot,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent",
		"DETENT_ONBOARDING_MODE=new-install",
		"IDENTITY_CONFIRMED=false",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("answers.env missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "IDENTITY_CONFIRMED=true") {
		t.Fatalf("answers.env must not confirm identity:\n%s", content)
	}
}

func TestOnboardingDiagnoseGateDetectsEnvPollutedFailure(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{
			name: "envrc exports",
			file: ".envrc",
		},
		{
			name: "dotenv assignments",
			file: ".env",
		},
		{
			name: "dotenv variant assignments",
			file: ".env.development",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetRoot := initOnboardingGitRepository(t, "https://github.com/digitaldrywood/creswoodcorners-phone.git")
			writeOnboardingEnvOverrideModule(t, targetRoot)
			if err := os.WriteFile(filepath.Join(targetRoot, tt.file), []byte(strings.Join([]string{
				"export PUBLIC_BASE_URL=https://local.example.test",
				"SUPPORT_PHONE=555-0100",
				"",
			}, "\n")), 0o600); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", tt.file, err)
			}
			t.Setenv("PUBLIC_BASE_URL", "https://local.example.test")
			t.Setenv("SUPPORT_PHONE", "555-0100")
			t.Setenv("UNRELATED_ENV", "kept")

			cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{
				"--format", "json",
				"onboarding", "diagnose-gate",
				"--source-root", targetRoot,
				"--command", "go test ./...",
				"--timeout", "30s",
			})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v\nstdout:\n%s", err, stdout.String())
			}

			var got struct {
				Status                    string   `json:"status"`
				FailingCommand            string   `json:"failing_command"`
				RelevantEnvironmentKeys   []string `json:"relevant_environment_keys"`
				PassingSanitizedCommand   string   `json:"passing_sanitized_command"`
				RecommendedGateCommand    string   `json:"recommended_gate_command"`
				CandidateEnvironmentFiles []string `json:"candidate_environment_files"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if got.Status != "env_polluted" {
				t.Fatalf("status = %q, want env_polluted: %#v", got.Status, got)
			}
			if got.FailingCommand != "go test ./..." {
				t.Fatalf("failing command = %q, want go test ./...", got.FailingCommand)
			}
			for _, want := range []string{"PUBLIC_BASE_URL", "SUPPORT_PHONE"} {
				if !containsString(got.RelevantEnvironmentKeys, want) {
					t.Fatalf("relevant environment keys = %#v, want %q", got.RelevantEnvironmentKeys, want)
				}
				if !strings.Contains(got.PassingSanitizedCommand, "-u "+want) {
					t.Fatalf("passing sanitized command = %q, want unset %s", got.PassingSanitizedCommand, want)
				}
			}
			if len(got.RelevantEnvironmentKeys) != 2 {
				t.Fatalf("relevant environment keys = %#v, want only polluted keys", got.RelevantEnvironmentKeys)
			}
			if strings.Contains(got.PassingSanitizedCommand, "UNRELATED_ENV") {
				t.Fatalf("passing sanitized command includes unrelated key: %q", got.PassingSanitizedCommand)
			}
			if got.RecommendedGateCommand != got.PassingSanitizedCommand {
				t.Fatalf("recommended gate command = %q, want passing sanitized command %q", got.RecommendedGateCommand, got.PassingSanitizedCommand)
			}
			if !containsString(got.CandidateEnvironmentFiles, filepath.Join(targetRoot, tt.file)) {
				t.Fatalf("candidate environment files = %#v, want %s", got.CandidateEnvironmentFiles, filepath.Join(targetRoot, tt.file))
			}
		})
	}
}

func TestOnboardingDiagnoseGateKeepsPassingCommandRecommended(t *testing.T) {
	targetRoot := initOnboardingGitRepository(t, "https://github.com/acme/api.git")
	writeOnboardingEnvOverrideModule(t, targetRoot)

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--format", "json",
		"onboarding", "diagnose-gate",
		"--source-root", targetRoot,
		"--command", "go test ./...",
		"--timeout", "30s",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\nstdout:\n%s", err, stdout.String())
	}

	var got struct {
		Status                 string `json:"status"`
		PassingCommand         string `json:"passing_command"`
		RecommendedGateCommand string `json:"recommended_gate_command"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != "pass" || got.PassingCommand != "go test ./..." || got.RecommendedGateCommand != "go test ./..." {
		t.Fatalf("diagnostic result = %#v, want passing command recommendation", got)
	}
}

func validIdentityOnboardingAnswers(t *testing.T) string {
	t.Helper()

	sourceRoot := initOnboardingGitRepository(t, "git@github.com:digitaldrywood/detent.git")
	return strings.Join([]string{
		"CUSTOMER_ID=digitaldrywood",
		"DETENT_PROJECT_ID=detent",
		"TARGET_REPOSITORY=digitaldrywood/detent",
		"TARGET_SOURCE_ROOT=" + sourceRoot,
		"REFERENCE_REPOSITORIES=digitaldrywood/detent-orchestration,corylanou/website-template",
		"DETENT_ONBOARDING_MODE=add-project",
		"IDENTITY_CONFIRMED=true",
		"",
	}, "\n")
}

func writeOnboardingAnswers(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "answers.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func fullAutopilotProfileAnswers() map[string]string {
	return map[string]string{
		"KANBAN_MODE":                           "integration",
		"AUTO_PROMOTE_ENABLED":                  "true",
		"AUTO_PROMOTE_QUIET_SECONDS":            "0",
		"GATE_REQUIRE_AUTOMATED_REVIEW":         "false",
		"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": "false",
		"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       "true",
		"MERGING_CONCURRENCY":                   "1",
	}
}

func lastNonblankOnboardingLine(content string) string {
	last := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

func initOnboardingGitRepository(t *testing.T, remote string) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "origin", remote)
	return dir
}

func initOnboardingDetentSourceCheckout(t *testing.T, stale bool) (string, string, string) {
	t.Helper()

	root := t.TempDir()
	remote := filepath.Join(root, "detent.git")
	runGitOutput(t, "", "init", "--bare", remote)

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("MkdirAll(seed) error = %v", err)
	}
	runGit(t, seed, "init")
	runGit(t, seed, "checkout", "-b", "main")
	runGit(t, seed, "config", "user.email", "detent@example.test")
	runGit(t, seed, "config", "user.name", "Detent Test")
	writeOnboardingTestFile(t, filepath.Join(seed, "README.md"), "initial\n")
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	local := filepath.Join(root, "local")
	runGitOutput(t, "", "clone", "-b", "main", remote, local)
	sourceHead := runGitOutput(t, local, "rev-parse", "HEAD")
	canonicalMain := sourceHead
	if stale {
		writeOnboardingTestFile(t, filepath.Join(seed, "README.md"), "updated\n")
		runGit(t, seed, "add", "README.md")
		runGit(t, seed, "commit", "-m", "update")
		runGit(t, seed, "push", "origin", "main")
		canonicalMain = runGitOutput(t, seed, "rev-parse", "HEAD")
	}
	return local, sourceHead, canonicalMain
}

func writeOnboardingTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func detentVersionJSON(commit string) string {
	return `{"version":"v0.11.1","commit":"` + commit + `","build_date":"2026-06-24T18:49:56Z","go_version":"go1.26.4","os":"linux","arch":"amd64"}` + "\n"
}

func onboardingTestCommandRunner(detentVersionOutput string) cli.CommandRunner {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "detent" {
			return detentVersionOutput, nil
		}
		cmd := exec.CommandContext(ctx, name, args...)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
}

func writeOnboardingEnvOverrideModule(t *testing.T, dir string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/envpolluted\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config_test.go"), []byte(strings.Join([]string{
		"package envpolluted",
		"",
		"import (",
		"\t\"os\"",
		"\t\"testing\"",
		")",
		"",
		"func TestConfigDefaults(t *testing.T) {",
		"\tif value := os.Getenv(\"PUBLIC_BASE_URL\"); value != \"\" {",
		"\t\tt.Fatalf(\"PUBLIC_BASE_URL = %q, want empty\", value)",
		"\t}",
		"\tif value := os.Getenv(\"SUPPORT_PHONE\"); value != \"\" {",
		"\t\tt.Fatalf(\"SUPPORT_PHONE = %q, want empty\", value)",
		"\t}",
		"}",
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile(config_test.go) error = %v", err)
	}
}

func writeOnboardingGlobalConfig(t *testing.T, projects []globalconfig.Project) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "global.yaml")
	cfg, err := globalconfig.DefaultAt(path)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	cfg.Projects = projects
	if err := globalconfig.Write(path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return path
}

func canonicalOnboardingTestPath(t *testing.T, path string) string {
	t.Helper()

	evaluated, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(evaluated)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	return filepath.Clean(absolute)
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	runGitOutput(t, dir, args...)
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	commandArgs := append([]string(nil), args...)
	if strings.TrimSpace(dir) != "" {
		commandArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
