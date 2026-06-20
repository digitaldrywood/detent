package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cli"
)

func TestOnboardingValidateAnswersCommandRejectsMissingGitHubMode(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, "MUTATION_CONFIRMED=true\n")
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

func TestOnboardingValidateAnswersCommandRequiresFinalMutationConfirmation(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, strings.Join([]string{
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

	answersPath := writeOnboardingAnswers(t, strings.Join([]string{
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

func TestOnboardingValidateAnswersCommandAcceptsDecisionPhase(t *testing.T) {
	t.Parallel()

	answersPath := writeOnboardingAnswers(t, "GITHUB_MODE=issue_field\n")
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

	answersPath := writeOnboardingAnswers(t, strings.Join([]string{
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

func writeOnboardingAnswers(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "answers.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
