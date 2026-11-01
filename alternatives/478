package cli

import (
	"context"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const (
	hubLocalMigrationURL  = "https://github.com/digitaldrywood/detent/blob/develop/docs/hub-local-migration.md"
	standaloneDeprecation = "Running Detent without client.hub_url is deprecated; the standalone local board is no longer supported."
	hubMigrationHint      = "Run a self-hosted Hub (detent hub serve) and enroll a runner, then configure client.hub_url. See " + hubLocalMigrationURL
)

func checkDoctorHubConnection(cfg globalconfig.Config) doctorCheck {
	if cfg.Client.Configured() {
		return doctorCheck{Name: "Hub connection", Status: doctorOK, Detail: "client.hub_url is configured"}
	}
	return doctorCheck{Name: "Hub connection", Status: doctorWarn, Detail: standaloneDeprecation, Hint: hubMigrationHint}
}

func validateHubStartup(ctx context.Context, cfg globalconfig.Config, deps runtimeDeps) error {
	if cfg.Client.Configured() {
		return nil
	}
	deps = deps.withDefaults()
	for _, project := range cfg.Projects {
		workflow, err := loadRuntimeProjectWorkflow(ctx, project, deps)
		if err != nil {
			continue
		}
		if workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
			return NewValidationError("Standalone local board projects are no longer supported without client.hub_url.", hubMigrationHint, nil)
		}
	}
	return nil
}
