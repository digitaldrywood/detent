package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const (
	onboardingAnswersPhaseDecision = "decision"
	onboardingAnswersPhaseMutation = "mutation"
)

type onboardingAnswersValidationResult struct {
	Status            string `json:"status"`
	Path              string `json:"path"`
	Phase             string `json:"phase"`
	GitHubMode        string `json:"github_mode"`
	MutationConfirmed bool   `json:"mutation_confirmed"`
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
	cmd.Flags().StringVar(&phase, "phase", onboardingAnswersPhaseMutation, "validation phase: decision or mutation")
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
			"Ask the status-source question, record the explicit answer in answers.env, and rerun the validator before any mutation.",
			nil,
		)
	}
	return result, nil
}

func normalizeOnboardingAnswersPhase(phase string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "", onboardingAnswersPhaseMutation:
		return onboardingAnswersPhaseMutation, nil
	case onboardingAnswersPhaseDecision:
		return onboardingAnswersPhaseDecision, nil
	default:
		return "", NewValidationError(
			"--phase must be decision or mutation",
			"Use --phase decision after the status-source answer or --phase mutation before setup changes.",
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
