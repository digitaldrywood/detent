package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/citrigger"
	"github.com/digitaldrywood/detent/internal/gate"
)

type ciTriggerLabelDeps struct {
	now          func() time.Time
	wait         func(context.Context, time.Duration) error
	acquire      func(string) (citrigger.Lock, error)
	userCacheDir func() (string, error)
	runCommand   CommandRunner
}

type ciTriggerLabelInput struct {
	Repository      string
	PullRequest     int
	Label           string
	LabelBase64     string
	Hostname        string
	HostnameBase64  string
	StaggerSeconds  int
	CoordinationDir string
}

type ciTriggerLabelResult struct {
	Repository  string `json:"repository"`
	PullRequest int    `json:"pull_request"`
	Label       string `json:"label"`
	Reapplied   bool   `json:"reapplied"`
}

func newCITriggerLabelCommand(opts options) *cobra.Command {
	deps := defaultCITriggerLabelDeps(opts)
	var input ciTriggerLabelInput
	cmd := &cobra.Command{
		Use:          "ci-trigger-label",
		Short:        "Reapply a pull request CI trigger label with host-wide staggering",
		Example:      "detent ci-trigger-label --repository owner/repo --pull-request 42 --label ci:ready --hostname github.com --stagger-seconds 15",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runCITriggerLabel(cmd.Context(), input, deps)
			if err != nil {
				return err
			}
			return writeCITriggerLabelResult(cmd, result)
		},
	}
	cmd.Flags().StringVar(&input.Repository, "repository", "", "GitHub repository in owner/name form")
	cmd.Flags().IntVar(&input.PullRequest, "pull-request", 0, "pull request number")
	cmd.Flags().StringVar(&input.Label, "label", "", "CI trigger label to remove and add")
	cmd.Flags().StringVar(&input.LabelBase64, "label-base64", "", "URL-safe base64-encoded CI trigger label")
	cmd.Flags().StringVar(&input.Hostname, "hostname", "", "GitHub host for API requests")
	cmd.Flags().StringVar(&input.HostnameBase64, "hostname-base64", "", "URL-safe base64-encoded GitHub host")
	cmd.Flags().IntVar(&input.StaggerSeconds, "stagger-seconds", gate.DefaultCITriggerLabelStaggerSeconds, "minimum seconds between trigger-label reapplications on this host")
	cmd.Flags().StringVar(&input.CoordinationDir, "coordination-dir", "", "host-local coordination directory")
	return cmd
}

func defaultCITriggerLabelDeps(opts options) ciTriggerLabelDeps {
	return ciTriggerLabelDeps{
		runCommand: opts.runCommand,
	}
}

func runCITriggerLabel(ctx context.Context, input ciTriggerLabelInput, deps ciTriggerLabelDeps) (result ciTriggerLabelResult, err error) {
	input, err = decodeCITriggerLabelInput(input)
	if err != nil {
		return ciTriggerLabelResult{}, err
	}
	input.Repository = strings.TrimSpace(input.Repository)
	input.Label = strings.TrimSpace(input.Label)
	input.Hostname = strings.TrimSpace(input.Hostname)
	if !validCITriggerLabelRepository(input.Repository) {
		return ciTriggerLabelResult{}, NewValidationError("ci-trigger-label --repository must be owner/name", "Pass --repository owner/name.", nil)
	}
	if input.PullRequest <= 0 {
		return ciTriggerLabelResult{}, NewValidationError("ci-trigger-label --pull-request must be greater than 0", "Pass the pull request number.", nil)
	}
	if input.Label == "" {
		return ciTriggerLabelResult{}, NewValidationError("ci-trigger-label --label is required", "Pass the configured gate.ci_trigger_label value.", nil)
	}
	if input.StaggerSeconds <= 0 {
		return ciTriggerLabelResult{}, NewValidationError("ci-trigger-label --stagger-seconds must be greater than 0", "Pass a positive stagger.", nil)
	}
	deps = deps.withDefaults()
	if err := citrigger.Reapply(ctx, citrigger.Options{
		CoordinationDir: input.CoordinationDir,
		Repository:      input.Repository,
		Stagger:         time.Duration(input.StaggerSeconds) * time.Second,
	}, citrigger.Dependencies{
		Now:          deps.now,
		Wait:         deps.wait,
		Acquire:      deps.acquire,
		UserCacheDir: deps.userCacheDir,
	}, func(ctx context.Context) error {
		return reapplyGitHubPullRequestLabel(ctx, input, deps.runCommand)
	}); err != nil {
		return ciTriggerLabelResult{}, err
	}
	return ciTriggerLabelResult{Repository: input.Repository, PullRequest: input.PullRequest, Label: input.Label, Reapplied: true}, nil
}

func decodeCITriggerLabelInput(input ciTriggerLabelInput) (ciTriggerLabelInput, error) {
	var err error
	input.Label, err = decodeCITriggerLabelValue("--label", input.Label, "--label-base64", input.LabelBase64)
	if err != nil {
		return ciTriggerLabelInput{}, err
	}
	input.Hostname, err = decodeCITriggerLabelValue("--hostname", input.Hostname, "--hostname-base64", input.HostnameBase64)
	if err != nil {
		return ciTriggerLabelInput{}, err
	}
	return input, nil
}

func decodeCITriggerLabelValue(plainFlag string, plainValue string, encodedFlag string, encodedValue string) (string, error) {
	plainValue = strings.TrimSpace(plainValue)
	encodedValue = strings.TrimSpace(encodedValue)
	if plainValue != "" && encodedValue != "" {
		return "", NewValidationError("ci-trigger-label "+plainFlag+" and "+encodedFlag+" are mutually exclusive", "Pass only one representation.", nil)
	}
	if encodedValue == "" {
		return plainValue, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encodedValue)
	if err != nil {
		return "", NewValidationError("ci-trigger-label "+encodedFlag+" is invalid", "Pass unpadded URL-safe base64.", nil)
	}
	return string(decoded), nil
}

func (deps ciTriggerLabelDeps) withDefaults() ciTriggerLabelDeps {
	if deps.runCommand == nil {
		deps.runCommand = defaultCommandRunner
	}
	return deps
}

func validCITriggerLabelRepository(repository string) bool {
	owner, name, ok := strings.Cut(repository, "/")
	return ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(name) != "" && !strings.Contains(name, "/")
}

func ciTriggerLabelCoordinationPaths(dir string, repository string) (string, string) {
	return citrigger.CoordinationPaths(dir, repository)
}

func reapplyGitHubPullRequestLabel(ctx context.Context, input ciTriggerLabelInput, run CommandRunner) error {
	basePath := "repos/" + input.Repository + "/issues/" + strconv.Itoa(input.PullRequest) + "/labels"
	output, err := run(ctx, "gh", ciTriggerLabelGHArgs(input.Hostname, "--paginate", basePath, "--jq", ".[].name")...)
	if err != nil {
		return fmt.Errorf("list pull request labels: %w", err)
	}
	if ciTriggerLabelPresent(output, input.Label) {
		labelPath := basePath + "/" + url.PathEscape(input.Label)
		if _, err := run(ctx, "gh", ciTriggerLabelGHArgs(input.Hostname, "--method", "DELETE", labelPath, "--silent")...); err != nil {
			return fmt.Errorf("remove CI trigger label: %w", err)
		}
	}
	if _, err := run(ctx, "gh", ciTriggerLabelGHArgs(input.Hostname, "--method", "POST", basePath, "-f", "labels[]="+input.Label, "--silent")...); err != nil {
		return fmt.Errorf("add CI trigger label: %w", err)
	}
	return nil
}

func ciTriggerLabelGHArgs(hostname string, args ...string) []string {
	result := []string{"api"}
	if hostname = strings.TrimSpace(hostname); hostname != "" {
		result = append(result, "--hostname", hostname)
	}
	return append(result, args...)
}

func ciTriggerLabelPresent(output string, label string) bool {
	for _, current := range strings.Split(output, "\n") {
		if strings.EqualFold(strings.TrimSpace(current), strings.TrimSpace(label)) {
			return true
		}
	}
	return false
}

func writeCITriggerLabelResult(cmd *cobra.Command, result ciTriggerLabelResult) error {
	output, err := OutputForCommand(cmd)
	if err != nil {
		return err
	}
	if output.Format == OutputFormatJSON {
		return WriteJSON(output.Out, result)
	}
	_, err = fmt.Fprintf(output.Out, "Reapplied %s to %s#%d\n", result.Label, result.Repository, result.PullRequest)
	return err
}
