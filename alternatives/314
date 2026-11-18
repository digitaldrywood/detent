package cli

import (
	"log/slog"
	"strings"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runner"
)

func buildChatProvider(registry *project.Registry, logger *slog.Logger) chatpkg.Provider {
	if registry == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, tracked := range registry.List() {
		if tracked == nil {
			continue
		}
		workflow := tracked.Workflow().Config
		for _, backendConfig := range workflow.AgentBackendConfigs() {
			if backendConfig.Kind != workflowconfig.AgentBackendCodex {
				continue
			}
			backend, err := buildAgentBackend(backendConfig)
			if err != nil {
				logger.Warn("chat backend configuration failed", "project_id", tracked.ID(), "backend_id", backendConfig.ID, "error", err)
				continue
			}
			toolBackend, ok := backend.(runner.AgentToolBackend)
			if !ok {
				continue
			}
			workspacePath := strings.TrimSpace(projectSourceRoot(tracked.Config(), workflow))
			provider, err := chatpkg.NewAgentProvider(toolBackend, workspacePath, logger)
			if err != nil {
				logger.Warn("chat provider configuration failed", "project_id", tracked.ID(), "backend_id", backendConfig.ID, "error", err)
				continue
			}
			logger.Info("chat provider configured", "project_id", tracked.ID(), "backend_id", backendConfig.ID, "backend_kind", backendConfig.Kind)
			return provider
		}
	}
	logger.Warn("chat provider unavailable", "reason", "no compatible Codex backend is configured")
	return nil
}
