package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const (
	onboardingAnswersPhaseIdentity = "identity"
	onboardingAnswersPhaseDecision = "decision"
	onboardingAnswersPhaseMutation = "mutation"
)

var (
	onboardingAnswerIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	onboardingAnswerRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type onboardingAnswersValidationResult struct {
	Status                string   `json:"status"`
	Path                  string   `json:"path"`
	Phase                 string   `json:"phase"`
	CustomerID            string   `json:"customer_id"`
	DetentProjectID       string   `json:"detent_project_id"`
	TargetRepository      string   `json:"target_repository"`
	TargetSourceRoot      string   `json:"target_source_root"`
	ReferenceRepositories []string `json:"reference_repositories"`
	DetentOnboardingMode  string   `json:"detent_onboarding_mode"`
	IdentityConfirmed     bool     `json:"identity_confirmed"`
	GitHubMode            string   `json:"github_mode"`
	MutationConfirmed     bool     `json:"mutation_confirmed"`
}

type onboardingAnswers struct {
	Values       map[string]string
	LastNonblank string
	Problems     []string
}

func newOnboardingCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "onboarding",
		Short:   "Validate onboarding setup decisions",
		Example: `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env"`,
	}
	cmd.AddCommand(newOnboardingValidateAnswersCommand())
	return cmd
}

func newOnboardingValidateAnswersCommand() *cobra.Command {
	var answersPath string
	var phase string
	cmd := &cobra.Command{
		Use:          "validate-answers",
		Short:        "Validate onboarding answers.env before setup mutation",
		Example:      `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env"`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := validateOnboardingAnswersFile(answersPath, phase)
			if err != nil {
				return err
			}
			return out.Write(func(w io.Writer) error {
				_, writeErr := fmt.Fprintf(w, "onboarding answers valid for %s phase: %s\n", result.Phase, result.Path)
				return writeErr
			}, result)
		},
	}
	cmd.Flags().StringVar(&answersPath, "answers", "answers.env", "path to onboarding answers.env")
	cmd.Flags().StringVar(&phase, "phase", onboardingAnswersPhaseMutation, "validation phase: identity, decision, or mutation")
	return cmd
}

func validateOnboardingAnswersFile(path string, phase string) (onboardingAnswersValidationResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return onboardingAnswersValidationResult{}, NewValidationError(
			"--answers is required",
			`Run detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env".`,
			nil,
		)
	}
	normalizedPhase, err := normalizeOnboardingAnswersPhase(phase)
	if err != nil {
		return onboardingAnswersValidationResult{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return onboardingAnswersValidationResult{}, NewValidationError(
				"answers file not found: "+path,
				"Create answers.env from explicit human answers before continuing onboarding.",
				nil,
			)
		}
		return onboardingAnswersValidationResult{}, fmt.Errorf("read onboarding answers %s: %w", path, err)
	}
	answers := parseOnboardingAnswers(raw)
	result := onboardingAnswersValidationResult{
		Status: "ok",
		Path:   path,
		Phase:  normalizedPhase,
	}
	problems := validateOnboardingAnswers(answers, normalizedPhase, &result)
	if len(problems) > 0 {
		return onboardingAnswersValidationResult{}, NewValidationError(
			strings.Join(problems, "; "),
			onboardingAnswersValidationHint(normalizedPhase),
			nil,
		)
	}
	return result, nil
}

func normalizeOnboardingAnswersPhase(phase string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "", onboardingAnswersPhaseMutation:
		return onboardingAnswersPhaseMutation, nil
	case onboardingAnswersPhaseIdentity:
		return onboardingAnswersPhaseIdentity, nil
	case onboardingAnswersPhaseDecision:
		return onboardingAnswersPhaseDecision, nil
	default:
		return "", NewValidationError(
			"--phase must be identity, decision, or mutation",
			"Use --phase identity before repository-specific discovery, --phase decision after the status-source answer, or --phase mutation before setup changes.",
			nil,
		)
	}
}

func parseOnboardingAnswers(raw []byte) onboardingAnswers {
	answers := onboardingAnswers{
		Values: make(map[string]string),
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		answers.LastNonblank = line
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validOnboardingAnswerKey(key) {
			answers.Problems = append(answers.Problems, fmt.Sprintf("line %d must be KEY=VALUE", lineNumber))
			continue
		}
		answers.Values[key] = trimOnboardingAnswerValue(value)
	}
	if err := scanner.Err(); err != nil {
		answers.Problems = append(answers.Problems, "read answers.env: "+err.Error())
	}
	return answers
}

func validOnboardingAnswerKey(key string) bool {
	if key == "" {
		return false
	}
	for _, char := range key {
		if char == '_' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func trimOnboardingAnswerValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	first := value[0]
	last := value[len(value)-1]
	if first == last && (first == '\'' || first == '"') {
		return value[1 : len(value)-1]
	}
	return value
}

func validateOnboardingAnswers(answers onboardingAnswers, phase string, result *onboardingAnswersValidationResult) []string {
	problems := append([]string(nil), answers.Problems...)
	identityProblems := validateOnboardingIdentityAnswers(answers, result)
	problems = append(problems, identityProblems...)
	if len(identityProblems) > 0 && strings.TrimSpace(answers.Values["GITHUB_MODE"]) != "" {
		problems = append(problems, "GITHUB_MODE cannot be set before identity answers are valid")
	}
	if phase == onboardingAnswersPhaseIdentity {
		return problems
	}

	mode := answers.Values["GITHUB_MODE"]
	switch mode {
	case workflowconfig.GitHubStatusSourceProjectV2, workflowconfig.GitHubStatusSourceIssueField, workflowconfig.GitHubStatusSourceLabel:
		result.GitHubMode = mode
	default:
		problems = append(problems, "GITHUB_MODE must be project_v2, issue_field, or label")
	}
	if phase == onboardingAnswersPhaseMutation {
		problems = append(problems, validateOnboardingMutationAnswers(answers, mode, result)...)
	}
	return problems
}

func validateOnboardingIdentityAnswers(answers onboardingAnswers, result *onboardingAnswersValidationResult) []string {
	var problems []string

	customerID := strings.TrimSpace(answers.Values["CUSTOMER_ID"])
	if customerID == "" {
		problems = append(problems, "CUSTOMER_ID is required")
	} else if !onboardingAnswerIdentifierPattern.MatchString(customerID) {
		problems = append(problems, "CUSTOMER_ID must contain only letters, digits, underscore, dot, or hyphen")
	} else {
		result.CustomerID = customerID
	}

	projectID := strings.TrimSpace(answers.Values["DETENT_PROJECT_ID"])
	if projectID == "" {
		problems = append(problems, "DETENT_PROJECT_ID is required")
	} else if !onboardingAnswerIdentifierPattern.MatchString(projectID) {
		problems = append(problems, "DETENT_PROJECT_ID must contain only letters, digits, underscore, dot, or hyphen")
	} else {
		result.DetentProjectID = projectID
	}

	targetRepository := strings.TrimSpace(answers.Values["TARGET_REPOSITORY"])
	targetRepositoryValid := false
	if targetRepository == "" {
		problems = append(problems, "TARGET_REPOSITORY is required")
	} else if !onboardingAnswerRepositoryPattern.MatchString(targetRepository) {
		problems = append(problems, "TARGET_REPOSITORY must look like owner/name")
	} else {
		targetRepositoryValid = true
		result.TargetRepository = targetRepository
	}

	referenceRepositories, referenceProblems := validateOnboardingReferenceRepositories(answers, targetRepository)
	problems = append(problems, referenceProblems...)
	result.ReferenceRepositories = referenceRepositories

	targetSourceRoot := strings.TrimSpace(answers.Values["TARGET_SOURCE_ROOT"])
	sourceProblems := validateOnboardingTargetSourceRoot(targetSourceRoot, targetRepository, targetRepositoryValid)
	problems = append(problems, sourceProblems...)
	if len(sourceProblems) == 0 {
		result.TargetSourceRoot = targetSourceRoot
	}

	mode := strings.TrimSpace(answers.Values["DETENT_ONBOARDING_MODE"])
	switch mode {
	case "new-install", "existing-install", "add-project":
		result.DetentOnboardingMode = mode
	case "":
		problems = append(problems, "DETENT_ONBOARDING_MODE is required")
	default:
		problems = append(problems, "DETENT_ONBOARDING_MODE must be new-install, existing-install, or add-project")
	}

	result.IdentityConfirmed = answers.Values["IDENTITY_CONFIRMED"] == "true"
	if !result.IdentityConfirmed {
		problems = append(problems, "IDENTITY_CONFIRMED must be true")
	}

	return problems
}

func validateOnboardingReferenceRepositories(answers onboardingAnswers, targetRepository string) ([]string, []string) {
	raw, ok := answers.Values["REFERENCE_REPOSITORIES"]
	if !ok {
		return nil, []string{"REFERENCE_REPOSITORIES is required"}
	}

	var repositories []string
	var problems []string
	for _, part := range strings.Split(raw, ",") {
		repository := strings.TrimSpace(part)
		if repository == "" {
			continue
		}
		if !onboardingAnswerRepositoryPattern.MatchString(repository) {
			problems = append(problems, "REFERENCE_REPOSITORIES entries must look like owner/name")
			continue
		}
		if targetRepository != "" && strings.EqualFold(repository, targetRepository) {
			problems = append(problems, "REFERENCE_REPOSITORIES must not include TARGET_REPOSITORY")
			continue
		}
		repositories = append(repositories, repository)
	}
	return repositories, problems
}

func validateOnboardingTargetSourceRoot(path string, targetRepository string, targetRepositoryValid bool) []string {
	if path == "" {
		return []string{"TARGET_SOURCE_ROOT is required"}
	}
	if !filepath.IsAbs(path) {
		return []string{"TARGET_SOURCE_ROOT must be absolute"}
	}

	var problems []string
	if err := defaultGitWorkTree(context.Background(), path); err != nil {
		return []string{"TARGET_SOURCE_ROOT must be a git checkout: " + err.Error()}
	}
	remote, err := defaultGitRemoteURL(context.Background(), path)
	if err != nil {
		return []string{"TARGET_SOURCE_ROOT must have an origin remote: " + err.Error()}
	}
	if targetRepositoryValid {
		repository, ok := gitHubRepositoryFromRemote(remote)
		if !ok || !strings.EqualFold(repository, targetRepository) {
			problems = append(problems, "TARGET_SOURCE_ROOT origin remote must match TARGET_REPOSITORY "+targetRepository)
		}
	}
	return problems
}

func gitHubRepositoryFromRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}

	if strings.HasPrefix(remote, "git@github.com:") {
		repository := strings.TrimPrefix(remote, "git@github.com:")
		return normalizeGitHubRepositoryPath(repository)
	}

	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", false
	}
	return normalizeGitHubRepositoryPath(parsed.Path)
}

func normalizeGitHubRepositoryPath(path string) (string, bool) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	if !onboardingAnswerRepositoryPattern.MatchString(path) {
		return "", false
	}
	return path, true
}

func onboardingAnswersValidationHint(phase string) string {
	switch phase {
	case onboardingAnswersPhaseIdentity:
		return "Ask the identity checkpoint questions, record confirmed CUSTOMER_ID, DETENT_PROJECT_ID, TARGET_REPOSITORY, TARGET_SOURCE_ROOT, REFERENCE_REPOSITORIES, DETENT_ONBOARDING_MODE, and IDENTITY_CONFIRMED=true, then rerun the validator before repository-specific discovery."
	case onboardingAnswersPhaseDecision:
		return "Complete and validate the identity phase first, then ask the status-source question, record the explicit GITHUB_MODE answer in answers.env, and rerun the validator before discovery recommendations become selected answers."
	default:
		return "Complete and validate the identity and status-source answers, record the explicit mutation confirmation in answers.env, and rerun the validator before any mutation."
	}
}

func validateOnboardingMutationAnswers(answers onboardingAnswers, mode string, result *onboardingAnswersValidationResult) []string {
	var problems []string
	switch mode {
	case workflowconfig.GitHubStatusSourceProjectV2:
		boardMode := answers.Values["BOARD_MODE"]
		switch boardMode {
		case "reuse":
			problems = append(problems, requireOnboardingAnswers(answers, "PROJECT_OWNER", "PROJECT_NUMBER")...)
		case "create":
			problems = append(problems, requireOnboardingAnswers(answers, "PROJECT_OWNER", "PROJECT_TITLE")...)
		default:
			problems = append(problems, "BOARD_MODE must be reuse or create for GITHUB_MODE=project_v2")
		}
	case workflowconfig.GitHubStatusSourceIssueField:
		problems = append(problems, requireOnboardingAnswers(answers, "STATUS_FIELD_NAME")...)
	case workflowconfig.GitHubStatusSourceLabel:
		problems = append(problems, requireOnboardingAnswers(answers, "STATUS_LABEL_PREFIX")...)
	}
	if answers.Values["MUTATION_CONFIRMED"] != "true" {
		problems = append(problems, "MUTATION_CONFIRMED must be true")
	}
	if answers.LastNonblank != "MUTATION_CONFIRMED=true" {
		problems = append(problems, "MUTATION_CONFIRMED=true must be the final nonblank line")
	}
	result.MutationConfirmed = answers.Values["MUTATION_CONFIRMED"] == "true" && answers.LastNonblank == "MUTATION_CONFIRMED=true"
	return problems
}

func requireOnboardingAnswers(answers onboardingAnswers, keys ...string) []string {
	var problems []string
	for _, key := range keys {
		if strings.TrimSpace(answers.Values[key]) == "" {
			problems = append(problems, key+" is required")
		}
	}
	return problems
}
