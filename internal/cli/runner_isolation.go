package cli

import (
	"bytes"
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
	var services []string
	tier := isolation.Sandbox
	if cfg.Client.IdentityFile != "" {
		if snapshot, err := runnerauth.LoadRoutingCache(cfg.Client.IdentityFile); err == nil {
			services = snapshot.Routing.HostServices
			tier = snapshot.Routing.IsolationTier
		}
	}
	failures := []string{}
	for _, configured := range cfg.Projects {
		workflow, err := project.LoadWorkflowContext(ctx, configured)
		if err != nil {
			report[configured.ID+"/workflow"] = []string{}
			failures = append(failures, configured.ID+"/workflow: "+err.Error())
			continue
		}
		for _, backend := range workflow.Config.AgentBackendConfigs() {
			key := configured.ID + "/" + backend.ID
			tiers, reasons := probeBackendTiers(ctx, backend, isolation.Policy{WritableRoots: []string{configured.Workdir}, HostServices: services, AllowLocalBinding: workflow.Config.Worker.EffectiveAllowLocalBinding(), ExtraNetworkDomains: workflow.Config.Worker.ExtraNetworkDomains}, probeBackendIsolation)
			report[key] = tiers
			if reason := reasons[tier]; reason != "" {
				failures = append(failures, key+": "+reason)
			}
		}
	}
	if len(failures) == 0 {
		return report, nil
	}
	slices.Sort(failures)
	problem := runnerauth.NewProblem("tier_unavailable")
	problem.Message = fmt.Sprintf("Agent access %s is unavailable: %s", tier, strings.Join(failures, "; "))
	problem.Message = strings.ReplaceAll(problem.Message, "\x00", "")
	if len(problem.Message) > 1000 {
		problem.Message = strings.ToValidUTF8(problem.Message[:997], "") + "..."
	}
	return report, []runnerauth.Problem{problem}
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
		return errors.New("backend version probe failed")
	}
	if policy.Tier == isolation.NativeTrusted {
		return nil
	}
	if !backendVersionAtLeast(output, minimum) {
		return errors.New("backend lacks required fail-closed sandbox settings")
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
			return errors.New("codex sandbox enforcement probe failed")
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
		return errors.New("claude effective sandbox settings probe failed")
	}
	host := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)(deny file-write*)", "/usr/bin/true")
	if err := host.Run(); err != nil {
		return errors.New("claude host sandbox probe failed")
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
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	groupID := procgroup.GroupID(cmd)
	err := cmd.Wait()
	return output.Bytes(), errors.Join(err, procgroup.Cleanup(groupID))
}
