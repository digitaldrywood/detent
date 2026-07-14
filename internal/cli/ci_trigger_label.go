package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/instancelock"
)

const ciTriggerLabelLockRetry = 250 * time.Millisecond

type ciTriggerLabelLock interface {
	Close() error
}

type ciTriggerLabelDeps struct {
	now          func() time.Time
	wait         func(context.Context, time.Duration) error
	acquire      func(string) (ciTriggerLabelLock, error)
	userCacheDir func() (string, error)
	runCommand   CommandRunner
}

type ciTriggerLabelInput struct {
	Repository      string
	PullRequest     int
	Label           string
	Hostname        string
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
	cmd.Flags().StringVar(&input.Hostname, "hostname", "", "GitHub host for API requests")
	cmd.Flags().IntVar(&input.StaggerSeconds, "stagger-seconds", gate.DefaultCITriggerLabelStaggerSeconds, "minimum seconds between trigger-label reapplications on this host")
	cmd.Flags().StringVar(&input.CoordinationDir, "coordination-dir", "", "host-local coordination directory")
	return cmd
}

func defaultCITriggerLabelDeps(opts options) ciTriggerLabelDeps {
	return ciTriggerLabelDeps{
		now:  time.Now,
		wait: waitForCITriggerLabel,
		acquire: func(path string) (ciTriggerLabelLock, error) {
			return instancelock.Acquire(path)
		},
		userCacheDir: os.UserCacheDir,
		runCommand:   opts.runCommand,
	}
}

func runCITriggerLabel(ctx context.Context, input ciTriggerLabelInput, deps ciTriggerLabelDeps) (result ciTriggerLabelResult, err error) {
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
	coordinationDir, err := ciTriggerLabelCoordinationDir(input.CoordinationDir, deps.userCacheDir)
	if err != nil {
		return ciTriggerLabelResult{}, err
	}
	lockPath, timestampPath := ciTriggerLabelCoordinationPaths(coordinationDir, input.Repository)
	lock, err := acquireCITriggerLabelLock(ctx, lockPath, deps)
	if err != nil {
		return ciTriggerLabelResult{}, err
	}
	defer func() {
		err = errors.Join(err, lock.Close())
	}()

	if err := enforceCITriggerLabelStagger(ctx, timestampPath, time.Duration(input.StaggerSeconds)*time.Second, deps); err != nil {
		return ciTriggerLabelResult{}, err
	}
	if err := reapplyGitHubPullRequestLabel(ctx, input, deps.runCommand); err != nil {
		return ciTriggerLabelResult{}, err
	}
	if err := os.WriteFile(timestampPath, []byte(deps.now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return ciTriggerLabelResult{}, fmt.Errorf("record CI trigger-label timestamp: %w", err)
	}
	return ciTriggerLabelResult{Repository: input.Repository, PullRequest: input.PullRequest, Label: input.Label, Reapplied: true}, nil
}

func (deps ciTriggerLabelDeps) withDefaults() ciTriggerLabelDeps {
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.wait == nil {
		deps.wait = waitForCITriggerLabel
	}
	if deps.acquire == nil {
		deps.acquire = func(path string) (ciTriggerLabelLock, error) { return instancelock.Acquire(path) }
	}
	if deps.userCacheDir == nil {
		deps.userCacheDir = os.UserCacheDir
	}
	if deps.runCommand == nil {
		deps.runCommand = defaultCommandRunner
	}
	return deps
}

func validCITriggerLabelRepository(repository string) bool {
	owner, name, ok := strings.Cut(repository, "/")
	return ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(name) != "" && !strings.Contains(name, "/")
}

func ciTriggerLabelCoordinationDir(configured string, userCacheDir func() (string, error)) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		return filepath.Abs(configured)
	}
	root, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(root, "detent", "ci-trigger-label"), nil
}

func ciTriggerLabelCoordinationPaths(dir string, repository string) (string, string) {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repository))))
	key := hex.EncodeToString(sum[:8])
	base := filepath.Join(dir, key)
	return base + ".lock", base + ".last"
}

func acquireCITriggerLabelLock(ctx context.Context, path string, deps ciTriggerLabelDeps) (ciTriggerLabelLock, error) {
	for {
		lock, err := deps.acquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, instancelock.ErrHeld) {
			return nil, fmt.Errorf("acquire CI trigger-label coordination lock: %w", err)
		}
		if err := deps.wait(ctx, ciTriggerLabelLockRetry); err != nil {
			return nil, err
		}
	}
}

func enforceCITriggerLabelStagger(ctx context.Context, timestampPath string, stagger time.Duration, deps ciTriggerLabelDeps) error {
	if stagger <= 0 {
		return nil
	}
	raw, err := os.ReadFile(timestampPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read CI trigger-label timestamp: %w", err)
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("parse CI trigger-label timestamp: %w", err)
	}
	remaining := last.Add(stagger).Sub(deps.now())
	if remaining <= 0 {
		return nil
	}
	return deps.wait(ctx, remaining)
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

func waitForCITriggerLabel(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
