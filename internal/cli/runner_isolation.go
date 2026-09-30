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
		snapshot, err := runnerauth.LoadRoutingCache(identityPath)
		if err != nil {
			return isolation.Policy{}, fmt.Errorf("load runner isolation policy: %w", err)
		}
		return isolation.Policy{Tier: snapshot.Routing.IsolationTier, HostServices: slices.Clone(snapshot.Routing.HostServices)}, nil
	}
}

func probeRunnerIsolation(ctx context.Context, cfg globalconfig.Config) isolation.Report {
	report := isolation.Report{}
	var services []string
	if cfg.Client.IdentityFile != "" {
		if snapshot, err := runnerauth.LoadRoutingCache(cfg.Client.IdentityFile); err == nil {
			services = snapshot.Routing.HostServices
		}
	}
	for _, configured := range cfg.Projects {
		workflow, err := project.LoadWorkflow(configured)
		if err != nil {
			report[configured.ID+"/workflow"] = []string{}
			continue
		}
		for _, backend := range workflow.Config.AgentBackendConfigs() {
			key := configured.ID + "/" + backend.ID
			report[key] = probeBackendTiers(ctx, backend, isolation.Policy{WritableRoots: []string{configured.Workdir}, HostServices: services}, probeBackendIsolation)
		}
	}
	return report
}

func probeBackendTiers(ctx context.Context, backend workflowconfig.AgentBackend, policy isolation.Policy, probe func(context.Context, workflowconfig.AgentBackend, isolation.Policy) error) []string {
	tiers := []string{}
	for _, tier := range []string{isolation.NativeTrusted, isolation.Sandbox} {
		policy.Tier = tier
		if err := probe(ctx, backend, policy); err == nil {
			tiers = append(tiers, tier)
		}
	}
	return tiers
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

func probeBackendIsolation(ctx context.Context, backend workflowconfig.AgentBackend, policy isolation.Policy) error {
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
	case workflowconfig.AgentBackendClaudeCode:
		shell = backend.ClaudeCodeOptions().Shell
		minimum = [3]int{2, 1, 285}
		if policy.Tier == isolation.Sandbox && len(backend.ClaudeCodeOptions().ExtraArgs) != 0 {
			return errors.New("Claude extra arguments cannot be isolated")
		}
	default:
		return errors.New("backend does not provide isolation")
	}
	if command == "" {
		return errors.New("backend command is empty")
	}
	output, err := commandshell.CommandWithArgs(ctx, command, shell, []string{"--version"}).Output()
	if err != nil {
		return errors.New("backend version probe failed")
	}
	if policy.Tier == isolation.NativeTrusted {
		return nil
	}
	if !backendVersionAtLeast(output, minimum) {
		return errors.New("backend lacks required fail-closed sandbox settings")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return errors.New("backend sandbox is unavailable on this platform")
	}
	base, err := os.MkdirTemp("", "detent-isolation-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(base) }()
	inside := filepath.Join(base, "worktree")
	if err := os.Mkdir(inside, 0o700); err != nil {
		return err
	}
	policy.WritableRoots = []string{inside}
	if err := policy.Validate(); err != nil {
		return err
	}
	if backend.Kind == workflowconfig.AgentBackendCodex {
		options, settings, err := codex.IsolationSettings(policy)
		if err != nil {
			return err
		}
		profile := settings["permissions"].(map[string]any)[options.PermissionProfile]
		args := []string{"-c", "permissions." + options.PermissionProfile + "=" + tomlInline(profile), "-c", "features.network_proxy=true", "sandbox", "-P", options.PermissionProfile, "-C", inside, "/bin/sh", "-c", `touch allowed && ! touch "$1/denied" && test -n "${HTTPS_PROXY:-${https_proxy:-}}"`, "probe", base}
		if err := commandshell.CommandWithArgs(ctx, command, shell, args).Run(); err != nil {
			return errors.New("Codex sandbox enforcement probe failed")
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
	if err := commandshell.CommandWithArgs(ctx, command, shell, []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--setting-sources", "", "--settings", string(encoded), "--tools", "Bash,Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}).Run(); err != nil {
		return errors.New("Claude sandbox settings probe failed")
	}
	var host *exec.Cmd
	if runtime.GOOS == "darwin" {
		host = exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)(deny file-write*)", "/usr/bin/true")
	} else {
		return errors.New("Claude Linux Unix socket enforcement is not proven")
	}
	if err := host.Run(); err != nil {
		return errors.New("Claude host sandbox probe failed")
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
