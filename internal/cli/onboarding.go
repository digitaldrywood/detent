package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	onboardingprofile "github.com/digitaldrywood/detent/internal/onboarding"
)

const (
	onboardingAnswersPhaseIdentity = "identity"
	onboardingAnswersPhaseDecision = "decision"
	onboardingAnswersPhaseMutation = "mutation"
	onboardingDetentRepository     = "digitaldrywood/detent"
)

var (
	onboardingAnswerIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	onboardingAnswerRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type onboardingAnswersValidationResult struct {
	Status                 string            `json:"status"`
	Path                   string            `json:"path"`
	Phase                  string            `json:"phase"`
	CustomerID             string            `json:"customer_id"`
	DetentProjectID        string            `json:"detent_project_id"`
	TargetRepository       string            `json:"target_repository"`
	TargetSourceRoot       string            `json:"target_source_root"`
	ReferenceRepositories  []string          `json:"reference_repositories"`
	DetentOnboardingMode   string            `json:"detent_onboarding_mode"`
	IdentityConfirmed      bool              `json:"identity_confirmed"`
	GitHubMode             string            `json:"github_mode"`
	DeliveryProfile        string            `json:"delivery_profile,omitempty"`
	DeliveryProfileAnswers map[string]string `json:"delivery_profile_answers,omitempty"`
	MutationConfirmed      bool              `json:"mutation_confirmed"`
}

type onboardingDraftAnswersResult struct {
	Status                         string   `json:"status"`
	AnswersPath                    string   `json:"answers_path,omitempty"`
	Written                        bool     `json:"written"`
	CustomerIDCandidate            string   `json:"customer_id_candidate"`
	CustomerIDSource               string   `json:"customer_id_source"`
	CustomerIDConfidence           string   `json:"customer_id_confidence"`
	CustomerIDReviewRequired       bool     `json:"customer_id_review_required"`
	CustomerIDAlternatives         []string `json:"customer_id_alternatives"`
	DetentProjectIDCandidate       string   `json:"detent_project_id_candidate"`
	DetentProjectIDSource          string   `json:"detent_project_id_source"`
	TargetRepositoryCandidate      string   `json:"target_repository_candidate"`
	TargetSourceRootCandidate      string   `json:"target_source_root_candidate"`
	ReferenceRepositoriesCandidate []string `json:"reference_repositories_candidate"`
	DetentOnboardingModeCandidate  string   `json:"detent_onboarding_mode_candidate"`
	ConfigPath                     string   `json:"config_path"`
	ConfigPathRule                 string   `json:"config_path_rule"`
	ConfigInstalled                bool     `json:"config_installed"`
	RegisteredProjectIDs           []string `json:"registered_project_ids"`
	Confidence                     string   `json:"confidence"`
	Notes                          []string `json:"notes"`
}

type onboardingAnswers struct {
	Values       map[string]string
	LastNonblank string
	Problems     []string
}

type onboardingDraftAnswersConfig struct {
	AnswersPath      string
	ConfigPath       string
	CustomerID       string
	DetentProjectID  string
	TargetSourceRoot string
	DetentSourceRoot string
	Write            bool
	Options          options
}

type onboardingCustomerIDCandidate struct {
	ID             string
	Source         string
	Confidence     string
	ReviewRequired bool
	Alternatives   []string
	Notes          []string
}

func newOnboardingCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "onboarding",
		Short:   "Draft and validate onboarding setup decisions",
		Example: `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env"`,
	}
	cmd.AddCommand(
		newOnboardingValidateAnswersCommand(),
		newOnboardingDraftAnswersCommand(configPath, opts),
	)
	return cmd
}

func newOnboardingDraftAnswersCommand(configPath *string, opts options) *cobra.Command {
	var answersPath string
	var customerID string
	var detentSourceRoot string
	var detentProjectID string
	var output string
	var targetSourceRoot string
	var write bool

	cmd := &cobra.Command{
		Use:          "draft-answers",
		Short:        "Draft onboarding identity answers from local evidence",
		Example:      `detent onboarding draft-answers --output pretty`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(output) != "" {
				if err := cmd.Root().PersistentFlags().Set(outputFormatFlag, output); err != nil {
					return err
				}
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			cfg := onboardingDraftAnswersConfig{
				AnswersPath:      answersPath,
				CustomerID:       customerID,
				DetentProjectID:  detentProjectID,
				TargetSourceRoot: targetSourceRoot,
				DetentSourceRoot: detentSourceRoot,
				Write:            write,
				Options:          opts,
			}
			if configPath != nil {
				cfg.ConfigPath = *configPath
			}
			result, err := draftOnboardingAnswers(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if write {
				if err := writeOnboardingDraftAnswers(result.AnswersPath, result); err != nil {
					return err
				}
				result.Written = true
			}
			return out.Write(func(w io.Writer) error {
				return writeOnboardingDraftAnswersPretty(w, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&answersPath, "answers", "answers.env", "path to onboarding answers.env")
	cmd.Flags().StringVar(&customerID, "customer-id", "", "explicit customer/workstream id candidate")
	cmd.Flags().StringVar(&detentProjectID, "detent-project-id", "", "explicit Detent project id candidate")
	cmd.Flags().StringVar(&targetSourceRoot, "target-source-root", "", "explicit target git checkout root")
	cmd.Flags().StringVar(&detentSourceRoot, "detent-source-root", "", "explicit Detent source checkout root")
	cmd.Flags().StringVar(&output, "output", "", "output format: pretty or json")
	cmd.Flags().BoolVar(&write, "write", false, "write drafted identity answers to answers.env")
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

func draftOnboardingAnswers(ctx context.Context, cfg onboardingDraftAnswersConfig) (onboardingDraftAnswersResult, error) {
	opts := cfg.Options
	if opts.resolvePath == nil || opts.read == nil {
		defaults := defaultOptions()
		if opts.resolvePath == nil {
			opts.resolvePath = defaults.resolvePath
		}
		if opts.read == nil {
			opts.read = defaults.read
		}
	}

	targetInput := strings.TrimSpace(cfg.TargetSourceRoot)
	targetExplicit := targetInput != ""
	if targetInput == "" {
		wd, err := os.Getwd()
		if err != nil {
			return onboardingDraftAnswersResult{}, fmt.Errorf("resolve current directory: %w", err)
		}
		targetInput = wd
	}
	targetRoot, targetRepository, err := onboardingGitCheckoutEvidence(ctx, targetInput)
	if err != nil {
		return onboardingDraftAnswersResult{}, err
	}
	if !targetExplicit && strings.EqualFold(targetRepository, onboardingDetentRepository) {
		return onboardingDraftAnswersResult{}, NewValidationError(
			"current checkout is the Detent source repository; provide --target-source-root for the repository being onboarded",
			"Run detent onboarding draft-answers from the target repository, or pass --target-source-root /path/to/target.",
			nil,
		)
	}

	resolution, err := resolveConfigPathResolution(cfg.ConfigPath, opts)
	if err != nil {
		return onboardingDraftAnswersResult{}, err
	}

	customerCandidate, err := onboardingCustomerIDFromRepository(targetRepository, cfg.CustomerID)
	if err != nil {
		return onboardingDraftAnswersResult{}, err
	}
	projectIDCandidate, projectIDSource, err := onboardingDetentProjectIDFromRepository(targetRepository, cfg.DetentProjectID)
	if err != nil {
		return onboardingDraftAnswersResult{}, err
	}

	result := onboardingDraftAnswersResult{
		Status:                    "draft",
		AnswersPath:               strings.TrimSpace(cfg.AnswersPath),
		CustomerIDCandidate:       customerCandidate.ID,
		CustomerIDSource:          customerCandidate.Source,
		CustomerIDConfidence:      customerCandidate.Confidence,
		CustomerIDReviewRequired:  customerCandidate.ReviewRequired,
		CustomerIDAlternatives:    customerCandidate.Alternatives,
		DetentProjectIDCandidate:  projectIDCandidate,
		DetentProjectIDSource:     projectIDSource,
		TargetRepositoryCandidate: targetRepository,
		TargetSourceRootCandidate: targetRoot,
		ConfigPath:                resolution.Path,
		ConfigPathRule:            string(resolution.Rule),
		Confidence:                "medium",
		Notes: []string{
			"inferred target repository from the local origin remote",
			"review all candidates with the operator before setting IDENTITY_CONFIRMED=true",
		},
	}
	result.Notes = append(result.Notes, customerCandidate.Notes...)

	result.ReferenceRepositoriesCandidate = onboardingReferenceRepositoryCandidates(ctx, cfg.DetentSourceRoot, targetRepository, &result.Notes)
	global, installed, err := readOnboardingDraftConfigEvidence(resolution.Path, opts)
	if err != nil {
		return onboardingDraftAnswersResult{}, err
	}
	result.ConfigInstalled = installed
	if installed {
		result.RegisteredProjectIDs = onboardingRegisteredProjectIDs(global.Projects)
		mode, notes := onboardingModeCandidate(global.Projects, result.DetentProjectIDCandidate, targetRoot)
		result.DetentOnboardingModeCandidate = mode
		result.Notes = append(result.Notes, notes...)
	} else {
		result.DetentOnboardingModeCandidate = "new-install"
		result.Notes = append(result.Notes, "no readable global config was found, so this looks like a new-install candidate")
	}
	if containsOnboardingReviewNote(result.Notes) {
		result.Confidence = "needs-review"
	}
	if result.CustomerIDReviewRequired {
		result.Confidence = "needs-review"
	}
	return result, nil
}

func onboardingGitCheckoutEvidence(ctx context.Context, path string) (string, string, error) {
	root, err := defaultGitTopLevel(ctx, path)
	if err != nil {
		return "", "", NewValidationError(
			"target source root must be a git checkout: "+err.Error(),
			"Run detent onboarding draft-answers from a GitHub checkout, or pass --target-source-root /path/to/target.",
			nil,
		)
	}
	remote, err := defaultGitRemoteURL(ctx, root)
	if err != nil {
		return "", "", NewValidationError(
			"target source root must have an origin remote: "+err.Error(),
			"Add a GitHub origin remote or pass a different --target-source-root.",
			nil,
		)
	}
	repository, ok := gitHubRepositoryFromRemote(remote)
	if !ok {
		return "", "", NewValidationError(
			"target source root origin remote must be a GitHub owner/name repository",
			"Use a checkout whose origin remote is hosted on github.com.",
			nil,
		)
	}
	return root, repository, nil
}

func defaultGitTopLevel(ctx context.Context, path string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", "-C", path, "rev-parse", "--show-toplevel") // #nosec G204 -- onboarding uses fixed git arguments against a local checkout path.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("git top-level is blank")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve git top-level %s: %w", root, err)
	}
	return filepath.Clean(absolute), nil
}

func onboardingReferenceRepositoryCandidates(ctx context.Context, detentSourceRoot string, targetRepository string, notes *[]string) []string {
	repository := onboardingDetentRepository
	if strings.TrimSpace(detentSourceRoot) != "" {
		_, detected, err := onboardingGitCheckoutEvidence(ctx, detentSourceRoot)
		if err == nil {
			repository = detected
			*notes = append(*notes, "inferred reference repository from --detent-source-root")
		} else {
			*notes = append(*notes, "could not inspect --detent-source-root; using canonical Detent source repository")
		}
	}
	if strings.EqualFold(repository, targetRepository) {
		return nil
	}
	return []string{repository}
}

func readOnboardingDraftConfigEvidence(path string, opts options) (globalconfig.Config, bool, error) {
	global, err := opts.read(path)
	if err == nil {
		return global, true, nil
	}
	var missing globalconfig.MissingFileError
	if errors.As(err, &missing) && errors.Is(missing.Err, os.ErrNotExist) {
		return globalconfig.Config{}, false, nil
	}
	return globalconfig.Config{}, false, fmt.Errorf("read global config evidence %s: %w", path, err)
}

func onboardingRegisteredProjectIDs(projects []globalconfig.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		if id := strings.TrimSpace(project.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func onboardingModeCandidate(projects []globalconfig.Project, candidateProjectID string, targetRoot string) (string, []string) {
	var notes []string
	for _, project := range projects {
		if sameOnboardingPath(project.Workdir, targetRoot) {
			return "existing-install", []string{"target source root already matches a registered project workdir"}
		}
	}
	if projectIndex(projects, candidateProjectID) >= 0 {
		notes = append(notes, fmt.Sprintf("project id candidate %q already exists in global config and needs operator review", candidateProjectID))
	}
	return "add-project", notes
}

func sameOnboardingPath(left string, right string) bool {
	leftAbs, leftErr := cleanOnboardingPath(left)
	rightAbs, rightErr := cleanOnboardingPath(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftAbs == rightAbs
}

func cleanOnboardingPath(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(absolute)
	evaluated, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.Clean(evaluated), nil
	}
	return clean, nil
}

func containsOnboardingReviewNote(notes []string) bool {
	for _, note := range notes {
		if strings.Contains(note, "needs operator review") || strings.Contains(note, "could not inspect") {
			return true
		}
	}
	return false
}

func repositoryOwner(repository string) string {
	owner, _, _ := strings.Cut(repository, "/")
	return owner
}

func repositoryName(repository string) string {
	_, name, _ := strings.Cut(repository, "/")
	return name
}

func onboardingCustomerIDFromRepository(repository string, override string) (onboardingCustomerIDCandidate, error) {
	override = strings.TrimSpace(override)
	owner := repositoryOwner(repository)
	name := repositoryName(repository)
	if override != "" {
		if !onboardingAnswerIdentifierPattern.MatchString(override) {
			return onboardingCustomerIDCandidate{}, NewValidationError(
				"--customer-id must contain only letters, digits, underscore, dot, or hyphen",
				"Pass a stable local customer/workstream id such as --customer-id creswoodcorners.",
				nil,
			)
		}
		return onboardingCustomerIDCandidate{
			ID:         override,
			Source:     "override",
			Confidence: "explicit",
			Notes:      []string{fmt.Sprintf("customer id candidate %q came from --customer-id", override)},
		}, nil
	}

	prefix, suffix, hasPrefix := repositoryCustomerPrefix(name)
	if ownerLooksSharedOperator(owner) {
		if hasPrefix && !strings.EqualFold(prefix, owner) {
			return onboardingCustomerIDCandidate{
				ID:           prefix,
				Source:       "repo_prefix",
				Confidence:   "medium",
				Alternatives: onboardingCandidateAlternatives(owner),
				Notes: []string{
					fmt.Sprintf("customer id candidate %q came from repository prefix before suffix %q", prefix, suffix),
				},
			}, nil
		}
		return onboardingCustomerIDCandidate{
			ID:             name,
			Source:         "repo_name",
			Confidence:     "needs-review",
			ReviewRequired: true,
			Alternatives:   onboardingCandidateAlternatives(owner),
			Notes: []string{
				fmt.Sprintf("customer id candidate needs operator review because owner %s looks like a shared operator", owner),
			},
		}, nil
	}

	return onboardingCustomerIDCandidate{
		ID:         owner,
		Source:     "owner",
		Confidence: "medium",
		Notes:      []string{fmt.Sprintf("customer id candidate %q came from repository owner", owner)},
	}, nil
}

func onboardingDetentProjectIDFromRepository(repository string, override string) (string, string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		return repositoryName(repository), "repo_name", nil
	}
	if !onboardingAnswerIdentifierPattern.MatchString(override) {
		return "", "", NewValidationError(
			"--detent-project-id must contain only letters, digits, underscore, dot, or hyphen",
			"Pass a stable local Detent project id such as --detent-project-id phone.",
			nil,
		)
	}
	return override, "override", nil
}

func repositoryCustomerPrefix(name string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(name), "-")
	if len(parts) < 2 {
		return "", "", false
	}
	suffix := strings.ToLower(parts[len(parts)-1])
	if !repositoryProductSuffix(suffix) {
		return "", "", false
	}
	prefix := strings.Join(parts[:len(parts)-1], "-")
	if prefix == "" || !onboardingAnswerIdentifierPattern.MatchString(prefix) {
		return "", "", false
	}
	return prefix, suffix, true
}

func repositoryProductSuffix(suffix string) bool {
	switch suffix {
	case "admin", "agent", "api", "app", "backend", "bot", "cli", "dashboard", "docs", "frontend",
		"hub", "infra", "ios", "mobile", "ops", "phone", "portal", "service", "site", "web",
		"worker":
		return true
	default:
		return false
	}
}

func ownerLooksSharedOperator(owner string) bool {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "digitaldrywood" || owner == "digital-drywood" {
		return true
	}
	for _, marker := range []string{"agency", "consulting", "labs", "studio", "solutions"} {
		if strings.Contains(owner, marker) {
			return true
		}
	}
	return false
}

func onboardingCandidateAlternatives(candidates ...string) []string {
	alternatives := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		alternatives = append(alternatives, candidate)
	}
	return alternatives
}

func writeOnboardingDraftAnswersPretty(w io.Writer, result onboardingDraftAnswersResult) error {
	lines := []string{
		"I found a likely target checkout from the current shell:",
		"",
		fmt.Sprintf("Customer/workstream: `%s`", result.CustomerIDCandidate),
		fmt.Sprintf("Project id: `%s`", result.DetentProjectIDCandidate),
		fmt.Sprintf("Target repository: `%s`", result.TargetRepositoryCandidate),
		fmt.Sprintf("Source checkout: `%s`", result.TargetSourceRootCandidate),
		fmt.Sprintf("Reference repositories: `%s`", strings.Join(result.ReferenceRepositoriesCandidate, ",")),
		fmt.Sprintf("Onboarding mode: `%s`", result.DetentOnboardingModeCandidate),
		"",
		"customer_id_source=" + result.CustomerIDSource,
		"customer_id_confidence=" + result.CustomerIDConfidence,
		"detent_project_id_source=" + result.DetentProjectIDSource,
		"confidence=" + result.Confidence,
	}
	if len(result.CustomerIDAlternatives) > 0 {
		lines = append(lines, "Customer/workstream alternatives: "+onboardingPrettyList(result.CustomerIDAlternatives))
	}
	if result.CustomerIDReviewRequired {
		lines = append(lines, "Customer/workstream requires operator review before identity confirmation.")
	}
	if result.ConfigPath != "" {
		lines = append(lines, "config_path: "+result.ConfigPath)
	}
	if len(result.RegisteredProjectIDs) > 0 {
		lines = append(lines, "registered_project_ids: "+strings.Join(result.RegisteredProjectIDs, ","))
	}
	if result.Written {
		lines = append(lines, "wrote_answers: "+result.AnswersPath)
	}
	lines = append(lines,
		"",
		"`CUSTOMER_ID` is only a stable local grouping id for this Detent install.",
		"I will not inspect target labels, issues, boards, WORKFLOW.md, validation commands, or runtime docs until you confirm this identity and the identity validator passes.",
		"",
		"answers.env preview:",
	)
	lines = append(lines, strings.Split(strings.TrimSpace(buildOnboardingDraftAnswersContent(nil, result)), "\n")...)
	if len(result.Notes) > 0 {
		lines = append(lines, "")
		lines = append(lines, "notes:")
		for _, note := range result.Notes {
			lines = append(lines, "- "+note)
		}
	}
	_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}

func onboardingPrettyList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			quoted = append(quoted, "`"+value+"`")
		}
	}
	return strings.Join(quoted, ", ")
}

func writeOnboardingDraftAnswers(path string, result onboardingDraftAnswersResult) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return NewValidationError(
			"--answers is required when --write is set",
			`Run detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write.`,
			nil,
		)
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing onboarding answers %s: %w", path, err)
	}
	content := buildOnboardingDraftAnswersContent(raw, result)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create onboarding answers directory %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write onboarding answers %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict onboarding answers %s: %w", path, err)
	}
	return nil
}

func buildOnboardingDraftAnswersContent(raw []byte, result onboardingDraftAnswersResult) string {
	replace := map[string]bool{
		"CUSTOMER_ID":            true,
		"DETENT_PROJECT_ID":      true,
		"TARGET_REPOSITORY":      true,
		"TARGET_SOURCE_ROOT":     true,
		"REFERENCE_REPOSITORIES": true,
		"DETENT_ONBOARDING_MODE": true,
		"IDENTITY_CONFIRMED":     true,
	}
	lines := make([]string, 0)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if key, ok := onboardingAnswerLineKey(line); ok && replace[key] {
			continue
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines,
		"CUSTOMER_ID="+result.CustomerIDCandidate,
		"DETENT_PROJECT_ID="+result.DetentProjectIDCandidate,
		"TARGET_REPOSITORY="+result.TargetRepositoryCandidate,
		"TARGET_SOURCE_ROOT="+result.TargetSourceRootCandidate,
		"REFERENCE_REPOSITORIES="+strings.Join(result.ReferenceRepositoriesCandidate, ","),
		"DETENT_ONBOARDING_MODE="+result.DetentOnboardingModeCandidate,
		"IDENTITY_CONFIRMED=false",
	)
	return strings.Join(lines, "\n") + "\n"
}

func onboardingAnswerLineKey(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, _, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	return key, ok && validOnboardingAnswerKey(key)
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

	problems = append(problems, validateOnboardingDeliveryProfileAnswers(answers, result)...)

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

func validateOnboardingDeliveryProfileAnswers(answers onboardingAnswers, result *onboardingAnswersValidationResult) []string {
	profile := strings.TrimSpace(answers.Values["DELIVERY_PROFILE"])
	if profile == "" {
		return nil
	}
	settings, ok := onboardingprofile.DeliveryProfile(profile)
	if !ok {
		return []string{"DELIVERY_PROFILE must be conservative_review or autonomous_delivery"}
	}
	expansion, _ := onboardingprofile.DeliveryProfileAnswerExpansion(settings.ID)
	result.DeliveryProfile = settings.ID
	result.DeliveryProfileAnswers = expansion

	var problems []string
	for _, key := range onboardingprofile.SortedDeliveryProfileAnswerKeys(expansion) {
		existing := strings.TrimSpace(answers.Values[key])
		if existing == "" {
			answers.Values[key] = expansion[key]
			continue
		}
		if existing != expansion[key] {
			problems = append(problems, fmt.Sprintf("%s=%s conflicts with DELIVERY_PROFILE=%s, which expands %s=%s", key, existing, settings.ID, key, expansion[key]))
		}
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
