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

func initOnboardingGitRepository(t *testing.T, remote string) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "origin", remote)
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
}
