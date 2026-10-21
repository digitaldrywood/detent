package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// PrepareSSHSource uses the same private GitHub config as local agent turns.
// The worker never writes a token to its persistent checkout or Git config.
// gitCredentialHelperEnvironment is the git configuration every worker git
// command runs with. A handed-off worker token lives in the private
// GH_CONFIG_DIR and gh reads it from there. Without a handed-off token that
// directory holds no login, so an https push would have no credential at all;
// the helper then runs gh against the host's own configuration, which is the
// runner's own credential, the same way an ssh remote already pushes with the
// host's key. Agents keep the private GH_CONFIG_DIR for their own gh calls.
func gitCredentialHelperEnvironment(workerToken string) map[string]string {
	helper := "!gh auth git-credential"
	if strings.TrimSpace(workerToken) == "" {
		helper = "!env -u GH_CONFIG_DIR gh auth git-credential"
		if dir := hostGitHubConfigDir(); dir != "" {
			helper = "!env GH_CONFIG_DIR=" + shellQuote(dir) + " gh auth git-credential"
		}
	}
	return map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "", "GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": helper}
}

func hostGitHubConfigDir() string {
	hostGitHubCLI.mu.Lock()
	defer hostGitHubCLI.mu.Unlock()
	if !hostGitHubCLI.captured {
		return os.Getenv("GH_CONFIG_DIR")
	}
	if value := hostGitHubCLI.values["GH_CONFIG_DIR"]; value != nil {
		return *value
	}
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func PrepareSSHSource(ctx context.Context, cfg config.Config, workdir, repository, scratch string) error {
	_, graphQL, _, err := workerGitHubEndpoints(cfg)
	if err != nil {
		return err
	}
	turn := AgentTurnRequest{TempDir: scratch, workerGitHub: workerGitHubPolicy{Token: cfg.Worker.GitHubToken, GraphQLURL: graphQL}}
	if err := configureWorkerGitHubEnvironment(&turn); err != nil {
		return err
	}
	captureHostGitHubCLIEnvironment()
	for name, value := range turn.Environment.Variables {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	// Git invokes gh for clone, workspace setup, hooks, gates, and pushes. Do
	// not persist the helper.
	for name, value := range gitCredentialHelperEnvironment(cfg.Worker.GitHubToken) {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	if cfg.Workspace.Kind == workspace.KindFilesystem {
		return nil
	}
	source := cfg.Workspace.SourceRoot
	if source == "" {
		source = workdir
	}
	if source == "" {
		return errors.New("SSH worker source root is required")
	}
	if _, err := os.Stat(filepath.Join(source, ".git")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if repository == "" || workspace.HTTPSRemoteURL(repository) != repository {
		return errors.New("SSH worker needs a provisioned source checkout or an HTTPS repository origin")
	}
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--", strings.TrimRight(repository, "/")+".git", source) // #nosec G204 -- fixed executable/subcommand; validated HTTPS origin and source follow -- as separate arguments.
	cmd.WaitDelay = time.Second
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: turn.Environment.Variables})
	// Clone diagnostics are intentionally omitted: transport URLs and credential
	// helpers can include secrets. The failure stays instance-owned.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prepare SSH source checkout: %w", err)
	}
	return nil
}
