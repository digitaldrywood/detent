package cli

import (
	"os"
	"path/filepath"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func codexTranscriptHomes(cfg workflowconfig.Config, lookup func(string) (string, bool)) ([]string, error) {
	var homes []string
	seen := map[string]bool{}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.Kind != workflowconfig.AgentBackendCodex {
			continue
		}
		credential, err := codexCredentialPath(backend.Command, lookup, os.UserHomeDir)
		if err != nil {
			return nil, err
		}
		home := filepath.Dir(credential)
		if !seen[home] {
			homes = append(homes, home)
			seen[home] = true
		}
	}
	return homes, nil
}
