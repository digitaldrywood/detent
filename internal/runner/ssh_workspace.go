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
func PrepareSSHSource(ctx context.Context, cfg config.Config, workdir, repository, scratch string) error {
	_, graphQL, _, err := workerGitHubEndpoints(cfg.Tracker.Endpoint)
	if err != nil {
		return err
	}
	turn := AgentTurnRequest{TempDir: scratch, workerGitHub: workerGitHubPolicy{Token: cfg.Worker.GitHubToken, GraphQLURL: graphQL}}
	if err := configureWorkerGitHubEnvironment(&turn); err != nil {
		return err
	}
	for name, value := range turn.Environment.Variables {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	// Git invokes gh against the private GH_CONFIG_DIR, including clone,
	// workspace setup, hooks, gates, and pushes. Do not persist the helper.
	for name, value := range map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "", "GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!gh auth git-credential"} {
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
