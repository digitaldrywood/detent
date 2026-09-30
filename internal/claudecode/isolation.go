package claudecode

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/isolation"
)

func IsolationSettings(p isolation.Policy) (map[string]any, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	sandbox := map[string]any{"enabled": false}
	settings := map[string]any{"sandbox": sandbox}
	if p.Tier == isolation.NativeTrusted {
		return settings, nil
	}
	sockets := []string{}
	for _, service := range p.HostServices {
		sockets = append(sockets, strings.TrimPrefix(service, "unix:"))
	}
	sandbox["enabled"] = true
	sandbox["failIfUnavailable"] = true
	sandbox["allowUnsandboxedCommands"] = false
	sandbox["autoAllowBashIfSandboxed"] = true
	sandbox["excludedCommands"] = []string{}
	sandbox["enableWeakerNestedSandbox"] = false
	sandbox["filesystem"] = map[string]any{"allowWrite": p.WritableRoots, "denyRead": []string{"~/.ssh", "~/.aws", "~/Library/Keychains", "/Library/Keychains"}, "disabled": false}
	sandbox["network"] = map[string]any{"allowedDomains": isolation.Domains(), "allowUnixSockets": sockets, "allowAllUnixSockets": false, "allowLocalBinding": false, "strictAllowlist": true}
	settings["permissions"] = map[string]any{"disableBypassPermissionsMode": "disable", "deny": []string{"Read(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/Library/Keychains/**)", "Read(//Library/Keychains/**)"}}
	return settings, nil
}
