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
	"time"

	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

const (
	// coordinatorCodexStateDir holds the coordinator's own home, Codex home
	// and temporary directory inside the configured workspace.
	coordinatorCodexStateDir = ".detent-coordinator"
	defaultCoordinatorCodex  = "codex app-server"
	// coordinatorFeatureCheckTimeout bounds the startup feature check.
	coordinatorFeatureCheckTimeout = 30 * time.Second
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
		Command:   replaceCodexHomeAssignment(command, codexHome) + coordinatorCodexToolFlags(),
		Workspace: workspace,
		Env:       coordinatorCodexEnvironment(lookupEnv, home, codexHome, temp, goos),
	}, nil
}

// coordinatorDisabledCodexFeatures are the Codex features whose tools can
// run commands, read files or images, reach the network or other agents'
// tools, or load operator extensions. Probing Codex 0.157.0 with a capturing
// model provider showed that with these disabled a turn offers no shell,
// exec_command, write_stdin, view_image, web_search, browser or connector
// tool: what is left is request_user_input, the JavaScript exec isolate (no
// file system or network), sub-agents that inherit this configuration, and
// apply_patch, which the read-only sandbox and the never approval policy
// reject. The coordinator then works through the hub's own tools alone.
var coordinatorDisabledCodexFeatures = []string{
	"shell_tool", "unified_exec", "shell_snapshot", "view_image", "code_mode", "code_mode_host",
	"apps", "plugins", "browser_use", "computer_use", "image_generation", "multi_agent", "multi_agent_v2",
	"goals", "tool_suggest", "skill_search", "hooks", "memories", "sleep_tool",
}

// coordinatorRequiredCodexFeatures must be known to the installed Codex:
// disabling a feature Codex no longer has would silently leave its tool on,
// so the coordinator refuses to start instead.
var coordinatorRequiredCodexFeatures = []string{"shell_tool", "unified_exec", "view_image", "apps", "plugins", "browser_use", "computer_use"}

// coordinatorCodexToolFlags are the app-server arguments that turn the
// built-in tools off. Values are bare TOML so no shell quoting is needed.
func coordinatorCodexToolFlags() string {
	var flags strings.Builder
	for _, feature := range coordinatorDisabledCodexFeatures {
		flags.WriteString(" -c features." + feature + "=false")
	}
	flags.WriteString(" -c web_search=disabled")
	return flags.String()
}

// coordinatorCodexBinary is the executable the command runs: the first word
// that is not an environment assignment.
func coordinatorCodexBinary(command string) string {
	for _, field := range strings.Fields(command) {
		if name, _, ok := strings.Cut(field, "="); ok && validEnvName(name) {
			continue
		}
		return strings.Trim(field, `"'`)
	}
	return ""
}

// codexFeatureLister runs `codex features list` for the coordinator.
type codexFeatureLister func(ctx context.Context, binary string, env []string) ([]byte, error)

func listCodexFeatures(ctx context.Context, binary string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, "features", "list") // #nosec G204 -- the binary is the operator-configured coordinator command.
	cmd.Env = env
	return cmd.Output()
}

// verifyCoordinatorCodexFeatures fails closed when the installed Codex does
// not know a feature the coordinator must disable.
func verifyCoordinatorCodexFeatures(ctx context.Context, launch coordinatorCodexLaunch, list codexFeatureLister) error {
	binary := coordinatorCodexBinary(launch.Command)
	if binary == "" {
		return errors.New("coordinator codex command names no executable")
	}
	output, err := list(ctx, binary, launch.Env)
	if err != nil {
		return fmt.Errorf("list coordinator Codex features: %w", err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			known[fields[0]] = true
		}
	}
	var missing []string
	for _, feature := range coordinatorRequiredCodexFeatures {
		if !known[feature] {
			missing = append(missing, feature)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the installed Codex cannot disable %s, so a coordinator turn could read files or run commands; the coordinator will not start", strings.Join(missing, ", "))
	}
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), coordinatorFeatureCheckTimeout)
	defer cancel()
	if err := verifyCoordinatorCodexFeatures(ctx, launch, listCodexFeatures); err != nil {
		return nil, err
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

// workspaceHoldsPath reports whether path lies inside workspace once
// symbolic links are resolved on both sides. The hub refuses a coordinator
// workspace that contains its own state, because the Codex read-only sandbox
// still lets a turn read the files under it.
func workspaceHoldsPath(workspace, path string) bool {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	root, err := resolveExistingPath(workspace)
	if err != nil {
		return false
	}
	target, err := resolveExistingPath(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// resolveExistingPath makes path absolute and resolves symbolic links in
// its nearest existing ancestor, keeping the components that do not exist
// yet, such as a database the hub has not created.
func resolveExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var rest []string
	current := absolute
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		rest = append([]string{filepath.Base(current)}, rest...)
		current = parent
	}
}
