package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

func runDoctorStartupPreflight(ctx context.Context, cfg doctorConfig, opts options, deps doctorDeps) doctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = deps.withDefaults()
	report := doctorReport{}
	boot, err := resolveBootConfig(ctx, cfg.ConfigPath, cfg.Host, cfg.Flags, opts)
	if err != nil {
		report.Add(doctorCheck{
			Name:   "Candidate startup",
			Status: doctorFail,
			Detail: err.Error(),
			Hint:   "Keep the current binary and fix the candidate compatibility failure before updating.",
		})
		return report
	}
	report.Add(doctorCheck{
		Name:   "Candidate startup",
		Status: doctorOK,
		Detail: fmt.Sprintf("candidate resolved %s via %s", boot.Global.Path, boot.ConfigPathRule),
	})
	if boot.Mode != BootModeRunning {
		return report
	}

	githubToken := runtimeGlobalGitHubToken(boot.Runtime.GitHubToken)
	for _, configuredProject := range boot.Global.Projects {
		id := doctorProjectID(configuredProject)
		workflow, loadErr := loadDoctorProjectWorkflow(ctx, configuredProject, deps)
		var migrationErr error
		if loadErr == nil {
			workflow.Config = projectpkg.MapNativeTracker(workflow.Config, boot.Global.Client.NativeProjects[id] != "")
			workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, githubToken)
			workflow.Config = projectpkg.EffectivePolicyConfig(configuredProject, workflow.Config)
			migrationErr = projectpkg.ValidateNativeTrackerFeatures(workflow.Config)
			loadErr = errors.Join(migrationErr, workflow.Config.Validate(), workflowconfig.ValidateWorkflowAdmission(workflow))
		}
		if loadErr != nil {
			if migrationErr != nil {
				report.Add(doctorCheck{
					Name:   "Project " + id + " startup",
					Status: doctorFail,
					Detail: "mapped native project cannot start: " + strings.TrimSpace(loadErr.Error()),
					Hint:   "Migrate unsupported intake or scheduled routines before approving this native project policy.",
				})
				continue
			}
			report.Add(doctorCheck{
				Name:   "Project " + id + " startup",
				Status: doctorWarn,
				Detail: "candidate will isolate this project as degraded: " + strings.TrimSpace(loadErr.Error()),
				Hint:   "Fix the project definition independently; it does not prevent the Detent host from booting.",
			})
			continue
		}
		report.Add(doctorCheck{
			Name:   "Project " + id + " startup",
			Status: doctorOK,
			Detail: "candidate loaded and validated the effective project definition",
		})
	}
	return report
}
