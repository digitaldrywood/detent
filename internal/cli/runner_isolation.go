package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/claudecode"
	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func firstIsolationPolicy(policies []func() (isolation.Policy, error)) func() (isolation.Policy, error) {
	if len(policies) == 0 {
		return nil
	}
	return policies[0]
}

func runnerIsolationPolicy(identityPath string) func() (isolation.Policy, error) {
	if identityPath == "" {
		return nil
	}
	return func() (isolation.Policy, error) {
		return isolation.Policy{}, errors.New("runner claim isolation policy is unavailable")
	}
}

func probeRunnerIsolation(ctx context.Context, cfg globalconfig.Config) (isolation.Report, []runnerauth.Problem) {
	report := isolation.Report{}
	problems := []runnerauth.Problem{}
	tier := isolation.Sandbox
	var services []string
	if cfg.Client.IdentityFile != "" {
		if snapshot, err := runnerauth.LoadRoutingCache(cfg.Client.IdentityFile); err == nil {
			services = snapshot.Routing.HostServices
			tier = snapshot.Routing.IsolationTier
		}
	}
	for _, configured := range cfg.Projects {
		workflow, err := project.LoadWorkflowContext(ctx, configured)
		if err != nil {
			p := runnerauth.NewProblem("settings_invalid")
			p.ProjectID = configured.ID
			p.Subject = configured.ID + "/workflow"
			p.Check = "load workflow"
			p.ErrorOutput = err.Error()
			problems = append(problems, runnerauth.SanitizeProblem(p))
			continue
		}
		for _, backend := range workflow.Config.AgentBackendConfigs() {
			key := configured.ID + "/" + backend.ID
			var unavailable error
			report[key], _ = probeBackendTiers(ctx, backend, isolation.Policy{WritableRoots: []string{configured.Workdir}, HostServices: services, AllowLocalBinding: workflow.Config.Worker.EffectiveAllowLocalBinding(), ExtraNetworkDomains: workflow.Config.Worker.ExtraNetworkDomains}, func(ctx context.Context, backend workflowconfig.AgentBackend, policy isolation.Policy) error {
				if policy.Tier == isolation.Sandbox && unavailable != nil {
					return unavailable
				}
				err := probeBackendIsolation(ctx, backend, policy)
				if policy.Tier == isolation.NativeTrusted {
					if err == nil {
						err = probeBackendSignIn(ctx, backend)
					}
					unavailable = err
				}
				if err != nil && (policy.Tier == isolation.NativeTrusted || policy.Tier == tier) {
					p := backendReadinessProblem(key, backend.Kind, policy.Tier, runtime.GOOS, err)
					p.ProjectID = configured.ID
					problems = append(problems, p)
				}
				return err
			})
		}
	}
	return report, runnerauth.MergeProblems(nil, problems, time.Now())
}

func probeBackendTiers(ctx context.Context, backend workflowconfig.AgentBackend, policy isolation.Policy, probe func(context.Context, workflowconfig.AgentBackend, isolation.Policy) error) ([]string, map[string]string) {
	tiers := []string{}
	reasons := map[string]string{}
	for _, tier := range []string{isolation.NativeTrusted, isolation.Sandbox} {
		policy.Tier = tier
		if err := probe(ctx, backend, policy); err == nil {
			tiers = append(tiers, tier)
		} else {
			reasons[tier] = err.Error()
		}
	}
	return tiers, reasons
}

var backendVersionPattern = regexp.MustCompile(`(?:^|\s)(\d+)\.(\d+)\.(\d+)(?:\s|$)`)

func backendVersionAtLeast(output []byte, minimum [3]int) bool {
	match := backendVersionPattern.FindSubmatch(output)
	if len(match) != 4 {
		return false
	}
	for i := range minimum {
		value, err := strconv.Atoi(string(match[i+1]))
		if err != nil {
			return false
		}
		if value != minimum[i] {
			return value > minimum[i]
		}
	}
	return true
}

func probeBackendIsolation(ctx context.Context, backend workflowconfig.AgentBackend, policy isolation.Policy) (probeErr error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := strings.TrimSpace(backend.Command)
	var shell string
	var minimum [3]int
	switch backend.Kind {
	case workflowconfig.AgentBackendCodex:
		prefix, suffix, ok := strings.Cut(command, " app-server")
		if !ok || strings.TrimSpace(suffix) != "" && strings.TrimSpace(suffix) != "--experimental" {
			return errors.New("unsupported Codex sandbox probe command")
		}
		command = prefix
		shell = backend.CodexOptions().Shell
		minimum = [3]int{0, 159, 2}
	case workflowconfig.AgentBackendPiAgent:
		if policy.Tier != isolation.NativeTrusted {
			return isolation.ErrSandboxUnavailable
		}
		shell = backend.PiAgentOptions().Shell
	case workflowconfig.AgentBackendClaudeCode:
		shell = backend.ClaudeCodeOptions().Shell
		minimum = [3]int{2, 1, 285}
		if policy.Tier == isolation.Sandbox && len(backend.ClaudeCodeOptions().ExtraArgs) != 0 {
			return errors.New("claude extra arguments cannot be isolated")
		}
	default:
		return errors.New("backend does not provide isolation")
	}
	if command == "" {
		return errors.New("backend command is empty")
	}
	output, err := runBackendProbe(backendProbeCommand(ctx, command, shell, []string{"--version"}))
	if err != nil {
		return &backendCheckError{Check: command + " --version", Err: err}
	}
	if policy.Tier == isolation.NativeTrusted {
		return nil
	}
	if !backendVersionAtLeast(output, minimum) {
		return &backendCheckError{Check: command + " --version", Err: fmt.Errorf("backend lacks required fail-closed sandbox settings: %s; minimum %d.%d.%d", output, minimum[0], minimum[1], minimum[2])}
	}
	if !isolation.SandboxAvailable() {
		return isolation.ErrSandboxUnavailable
	}
	base, err := os.MkdirTemp("", "detent-isolation-probe-")
	if err != nil {
		return err
	}
	defer func() { probeErr = errors.Join(probeErr, os.RemoveAll(base)) }()
	inside := filepath.Join(base, "worktree")
	if err := os.Mkdir(inside, 0o700); err != nil {
		return err
	}
	policy.WritableRoots = []string{inside}
	if err := policy.Validate(); err != nil {
		return err
	}
	ctx = isolation.WithPolicy(ctx, policy)
	if backend.Kind == workflowconfig.AgentBackendCodex {
		options, settings, err := codex.IsolationSettings(policy)
		if err != nil {
			return err
		}
		permissions, ok := settings["permissions"].(map[string]any)
		if !ok {
			return errors.New("codex isolation permissions are unavailable")
		}
		profile := permissions[options.PermissionProfile]
		args := []string{"-c", "permissions." + options.PermissionProfile + "=" + tomlInline(profile), "-c", "features.network_proxy=true", "sandbox", "-P", options.PermissionProfile, "-C", inside, "/bin/sh", "-c", `touch allowed && ! touch "$1/denied" && test -n "${HTTPS_PROXY:-${https_proxy:-}}"`, "probe", base}
		if _, err := runBackendProbe(backendProbeCommand(ctx, command, shell, args)); err != nil {
			return &backendCheckError{Check: command + " " + strings.Join(args, " "), Err: err}
		}
		return nil
	}
	settings, err := claudecode.IsolationSettings(policy)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("claude Linux Unix socket enforcement is not proven")
	}
	args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--setting-sources", "", "--settings", string(encoded), "--tools", "Bash,Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	cmd := backendProbeCommand(ctx, command, shell, args)
	cmd.Dir = inside
	procgroup.SetTempDir(cmd, inside)
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{"CLAUDE_CODE_TMPDIR": inside}})
	if err := claudecode.VerifySandboxCommand(ctx, cmd, settings); err != nil {
		return &backendCheckError{Check: command + " " + strings.Join(args, " "), Err: err}
	}
	host := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)(deny file-write*)", "/usr/bin/true")
	if err := host.Run(); err != nil {
		return &backendCheckError{Check: strings.Join(host.Args, " "), Err: err}
	}
	return nil
}

func tomlInline(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, strconv.Quote(key)+"="+tomlInline(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []string:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = strconv.Quote(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		return strconv.Quote(typed)
	case bool:
		return strconv.FormatBool(typed)
	default:
		panic("unsupported isolation configuration value")
	}
}

func backendProbeCommand(ctx context.Context, command, shell string, args []string) *exec.Cmd {
	cmd := commandshell.CommandWithArgs(ctx, command, shell, args)
	procgroup.Configure(ctx, cmd)
	cmd.Cancel = func() error { return procgroup.TerminateTree(cmd, procgroup.GroupID(cmd)) }
	cmd.WaitDelay = time.Second
	return cmd
}

func runBackendProbe(cmd *exec.Cmd) ([]byte, error) {
	var output boundedProbeOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	groupID := procgroup.GroupID(cmd)
	err := cmd.Wait()
	err = errors.Join(err, procgroup.Cleanup(groupID))
	if err != nil {
		err = fmt.Errorf("%w: %s", err, output.data)
	}
	return output.data, err
}

type boundedProbeOutput struct{ data []byte }

func (b *boundedProbeOutput) Write(p []byte) (int, error) {
	size := len(p)
	if available := 8192 - len(b.data); available > 0 {
		b.data = append(b.data, p[:min(available, size)]...)
	}
	return size, nil
}

type backendCheckError struct {
	Check     string
	Err       error
	SignedOut bool
}

func (e *backendCheckError) Error() string { return e.Err.Error() }
func (e *backendCheckError) Unwrap() error { return e.Err }

func backendReadinessProblem(subject, kind, tier, goos string, err error) runnerauth.Problem {
	code := "backend_missing"
	if tier == isolation.Sandbox {
		code = "tier_unavailable"
	}
	p := runnerauth.NewProblem(code)
	p.Subject = subject
	if tier == isolation.Sandbox {
		p.Subject += "/sandbox"
	}
	p.Check = "isolation " + tier
	p.ErrorOutput = err.Error()
	var check *backendCheckError
	if errors.As(err, &check) {
		p.Check = check.Check
		if check.SignedOut {
			p.Message = "not signed in"
			p.FixHint = "Sign in on this runner, then wait for its next heartbeat."
		}
	}
	switch kind {
	case workflowconfig.AgentBackendCodex:
		p.FixCommand = "npm install -g @openai/codex"
		if check != nil && check.SignedOut {
			p.FixCommand = "codex login --device-auth"
		}
	case workflowconfig.AgentBackendClaudeCode:
		p.FixCommand = "npm install -g @anthropic-ai/claude-code"
		if check != nil && check.SignedOut {
			p.FixCommand = "claude auth login"
		}
	}
	if tier == isolation.Sandbox && (check == nil || !strings.HasSuffix(check.Check, " --version")) {
		p.FixCommand = ""
	}
	output := strings.ToLower(err.Error())
	missingTool := strings.Contains(output, "bwrap") || strings.Contains(output, "bubblewrap") || strings.Contains(output, "socat")
	missingCommand := strings.Contains(output, "not found") || strings.Contains(output, "not installed") || strings.Contains(output, "no such file")
	if tier == isolation.Sandbox && goos == "linux" && missingTool && missingCommand {
		p.FixCommand = "sudo apt-get install bubblewrap socat"
	}
	return runnerauth.SanitizeProblem(p)
}

func probeBackendSignIn(ctx context.Context, backend workflowconfig.AgentBackend) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := backend.Command
	var shell string
	var args []string
	var environment map[string]string
	switch backend.Kind {
	case workflowconfig.AgentBackendCodex:
		prepared, err := prepareCodexCommandForRuntime(command)
		if err != nil {
			return &backendCheckError{Check: "prepare isolated Codex home", Err: err}
		}
		command, _, _ = strings.Cut(prepared.Command, " app-server")
		environment = prepared.Environment
		shell = backend.CodexOptions().Shell
		args = []string{"login", "status"}
	case workflowconfig.AgentBackendClaudeCode:
		shell = backend.ClaudeCodeOptions().Shell
		args = []string{"auth", "status", "--json"}
	default:
		return nil
	}
	cmd := backendProbeCommand(ctx, command, shell, args)
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: environment})
	output, err := runBackendProbe(cmd)
	if err == nil && backend.Kind == workflowconfig.AgentBackendClaudeCode {
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if decodeErr := json.Unmarshal(output, &status); decodeErr != nil {
			err = decodeErr
		} else if !status.LoggedIn {
			err = fmt.Errorf("%s", output)
		}
	}
	if err != nil {
		return &backendCheckError{Check: command + " " + strings.Join(args, " "), Err: err, SignedOut: strings.Contains(strings.ToLower(string(output)), "not logged in") || strings.Contains(strings.ToLower(string(output)), "not signed in") || strings.Contains(string(output), `"loggedIn":false`) || strings.Contains(string(output), `"loggedIn": false`)}
	}
	return nil
}
