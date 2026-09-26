package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

const (
	// coordinatorCodexStateDir holds the coordinator's own home, Codex home
	// and temporary directory inside the configured workspace.
	coordinatorCodexStateDir = ".detent-coordinator"
	defaultCoordinatorCodex  = "codex app-server"
)

// coordinatorPassthroughEnvironment names the only parent variables the
// coordinator's Codex process inherits: what a process needs to run and
// reach the provider, and the provider's own credentials. Hub secrets such
// as the WorkOS and Stripe keys, the admin token and database paths never
// reach it.
var coordinatorPassthroughEnvironment = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORGANIZATION", "OPENAI_PROJECT", "CODEX_API_KEY",
}

// coordinatorWindowsEnvironment is what a Windows process cannot start
// without.
var coordinatorWindowsEnvironment = []string{"SystemRoot", "ComSpec", "PATHEXT", "WINDIR"}

// coordinatorCodexLaunch is how the hub starts the coordinator's Codex
// process: the command with its CODEX_HOME assignment pointed at the
// dedicated home, the allowlisted environment and the workspace as cwd.
type coordinatorCodexLaunch struct {
	Command   string
	Workspace string
	Env       []string
}

// prepareCoordinatorCodex creates the coordinator's dedicated directories
// under the workspace and links only the provider credential into its Codex
// home: no config.toml, MCP servers, skills or instructions from the
// operator's own Codex home are loaded.
func prepareCoordinatorCodex(command, workspace string, lookupEnv func(string) (string, bool), userHomeDir func() (string, error), goos string) (coordinatorCodexLaunch, error) {
	credential, err := codexCredentialPath(command, lookupEnv, userHomeDir)
	if err != nil {
		return coordinatorCodexLaunch{}, err
	}
	root := filepath.Join(workspace, coordinatorCodexStateDir)
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex")
	temp := filepath.Join(root, "tmp")
	for _, dir := range []string{root, home, codexHome, temp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return coordinatorCodexLaunch{}, fmt.Errorf("create coordinator directory %s: %w", dir, err)
		}
	}
	if err := linkCoordinatorCredential(credential, filepath.Join(codexHome, "auth.json")); err != nil {
		return coordinatorCodexLaunch{}, err
	}
	return coordinatorCodexLaunch{
		Command:   replaceCodexHomeAssignment(command, codexHome),
		Workspace: workspace,
		Env:       coordinatorCodexEnvironment(lookupEnv, home, codexHome, temp, goos),
	}, nil
}

// linkCoordinatorCredential points the dedicated Codex home at the
// operator's credential. A missing credential is not an error: the provider
// may authenticate through an API key variable instead.
func linkCoordinatorCredential(source, target string) error {
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect Codex credential: %w", err)
	}
	if current, err := os.Readlink(target); err == nil && current == source {
		return nil
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace coordinator Codex credential: %w", err)
	}
	if err := os.Symlink(source, target); err != nil {
		return fmt.Errorf("link coordinator Codex credential: %w", err)
	}
	return nil
}

// coordinatorCodexEnvironment builds the child environment from the
// allowlist alone, with HOME, CODEX_HOME and the temporary directory set to
// the dedicated directories.
func coordinatorCodexEnvironment(lookupEnv func(string) (string, bool), home, codexHome, temp, goos string) []string {
	names := coordinatorPassthroughEnvironment
	if goos == "windows" {
		names = append(append([]string{}, names...), coordinatorWindowsEnvironment...)
	}
	env := make([]string, 0, len(names)+8)
	for _, name := range names {
		if value, ok := lookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, "HOME="+home, "CODEX_HOME="+codexHome, "TMPDIR="+temp, "TMP="+temp, "TEMP="+temp)
	if goos == "windows" {
		env = append(env, "USERPROFILE="+home)
	}
	return env
}

// coordinatorCodexCommand builds the coordinator's Codex process with only
// the prepared environment. Setting Env replaces the hub's environment
// rather than extending it.
func coordinatorCodexCommand(ctx context.Context, launch coordinatorCodexLaunch, shell string) *exec.Cmd {
	cmd := buildCodexCommandFromConfig(ctx, launch.Command, shell)
	cmd.Env = append([]string(nil), launch.Env...)
	cmd.Dir = launch.Workspace
	return cmd
}

// coordinatorCodexOptions forces the most restrictive policy Codex offers:
// a read-only sandbox without network access and no approvals, whatever the
// options section says. Coordinator turns are read-only as well, which the
// backend enforces per turn.
func coordinatorCodexOptions(cfg workflowconfig.CodexOptions) codex.Options {
	options := codex.OptionsFromConfig(cfg)
	options.ApprovalPolicy = "never"
	options.ThreadSandbox = "read-only"
	options.TurnSandboxPolicy = nil
	options.DeliverableElicitationAllowlist = nil
	return options
}

// buildCoordinatorCodexBackend builds the hub's coordinator backend.
func buildCoordinatorCodexBackend(command string, cfg workflowconfig.CodexOptions, workspace string) (runnerpkg.AgentBackend, error) {
	launch, err := prepareCoordinatorCodex(command, workspace, os.LookupEnv, os.UserHomeDir, runtime.GOOS)
	if err != nil {
		return nil, fmt.Errorf("prepare coordinator Codex: %w", err)
	}
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return coordinatorCodexCommand(ctx, launch, cfg.Shell)
	})
	if err != nil {
		return nil, fmt.Errorf("create coordinator codex transport factory: %w", err)
	}
	opts := []codex.AppServerOption{}
	if timeout := durationFromMillis(cfg.ReadTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithReadTimeout(timeout))
	}
	if timeout := durationFromMillis(cfg.TurnTimeoutMS); timeout > 0 {
		opts = append(opts, codex.WithTurnTimeout(timeout))
	}
	client, err := codex.NewAppServer(factory, opts...)
	if err != nil {
		return nil, fmt.Errorf("create coordinator codex app-server: %w", err)
	}
	backend, err := codex.NewAgentBackend(client, coordinatorCodexOptions(cfg))
	if err != nil {
		return nil, fmt.Errorf("create coordinator codex backend: %w", err)
	}
	return backend, nil
}

// workspaceHoldsPath reports whether path lies inside workspace. The hub
// refuses a coordinator workspace that contains its own state, because the
// Codex read-only sandbox still lets a turn read the files under it.
func workspaceHoldsPath(workspace, path string) bool {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
