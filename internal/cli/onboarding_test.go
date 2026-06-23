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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
}
