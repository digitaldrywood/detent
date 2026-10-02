package codex

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
)

func IsolationSettings(p isolation.Policy) (Options, map[string]any, error) {
	if err := p.Validate(); err != nil {
		return Options{}, nil, err
	}
	options := Options{ApprovalPolicy: "never", ThreadSandbox: "danger-full-access", TurnSandboxPolicy: map[string]any{"type": "dangerFullAccess"}}
	settings := map[string]any{"features.network_proxy": false}
	if p.Tier == isolation.NativeTrusted {
		return options, settings, nil
	}
	options.ThreadSandbox = "workspace-write"
	options.PermissionProfile = "detent-runner"
	options.TurnSandboxPolicy = map[string]any{"type": "workspaceWrite", "writableRoots": p.WritableRoots, "networkAccess": true, "excludeSlashTmp": true, "excludeTmpdirEnvVar": true}
	domains := map[string]any{}
	for _, domain := range isolation.Domains() {
		domains[domain] = "allow"
	}
	for _, domain := range p.ExtraNetworkDomains {
		domains[domain] = "allow"
	}
	if p.AllowLocalBinding {
		for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
			domains[host] = "allow"
		}
	}
	sockets := map[string]any{}
	for _, service := range p.HostServices {
		sockets[strings.TrimPrefix(service, "unix:")] = "allow"
	}
	network := map[string]any{"enabled": true, "mode": "limited", "domains": domains, "unix_sockets": sockets, "allow_local_binding": p.AllowLocalBinding, "allow_upstream_proxy": false, "dangerously_allow_all_unix_sockets": false, "dangerously_allow_non_loopback_proxy": false, "enable_socks5_udp": false}
	roots := map[string]any{}
	for _, root := range p.WritableRoots {
		roots[root] = true
	}
	settings["features.network_proxy"] = true
	settings["default_permissions"] = options.PermissionProfile
	settings["permissions"] = map[string]any{options.PermissionProfile: map[string]any{"filesystem": map[string]any{"/": "read", ":workspace_roots": "write"}, "workspace_roots": roots, "network": network}}
	return options, settings, nil
}

func threadConfig(req RunTurnRequest) map[string]any {
	settings := make(map[string]any, len(req.Config)+1)
	for key, value := range req.Config {
		settings[key] = value
	}
	if req.TerminalWaitTimeout > 0 {
		settings["background_terminal_max_timeout"] = req.TerminalWaitTimeout.Milliseconds()
	}
	return settings
}

func isolationRuntimeRoots(profile string, req runner.AgentTurnRequest) []string {
	if profile == "" {
		return nil
	}
	return appendUniqueStrings([]string{req.Workspace}, append([]string{req.TempDir}, req.ExtraWritableRoots...)...)
}

func setIsolationProfile(params map[string]any, req RunTurnRequest) {
	if req.Permissions == "" {
		return
	}
	params["permissions"] = req.Permissions
	params["runtimeWorkspaceRoots"] = req.RuntimeWorkspaceRoots
	delete(params, "sandbox")
	delete(params, "sandboxPolicy")
}
