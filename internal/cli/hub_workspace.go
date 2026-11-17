package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func workspaceLaneEnabled(capabilities workspacesession.Capabilities) bool {
	return capabilities.Files
}

func workspaceLaneCapabilities(ctx context.Context, cfg globalconfig.Config, support workspacerunner.Support) workspacesession.Capabilities {
	client := cfg.Client
	capabilities := workspacerunner.Capabilities(support)
	if strings.TrimSpace(client.IdentityFile) == "" || len(client.NativeProjects) == 0 || !workspaceLaneEnabled(capabilities) {
		return workspacesession.Capabilities{}
	}
	capabilities.Git = capabilities.Git && nativeProjectsUseGit(ctx, cfg)
	return capabilities
}

func nativeProjectsUseGit(ctx context.Context, cfg globalconfig.Config) bool {
	projects := project.ManagerConfigFromGlobal(cfg).Projects
	for name := range cfg.Client.NativeProjects {
		selected, found := workspaceLaneProject(projects, name)
		if !found {
			return false
		}
		workflow, err := project.LoadWorkflowContext(ctx, selected)
		if err != nil {
			return false
		}
		if kind := strings.TrimSpace(workflow.Config.Workspace.Kind); kind != "" && kind != workspace.KindLocalGit {
			return false
		}
	}
	return true
}

type workspaceLaneScheduler interface {
	NativeClient(project string) (*hubclient.NativeClient, bool)
	MachineID() tracker.MachineID
}

func workspaceSessionIDs(machineID tracker.MachineID, generation string) func() (string, error) {
	var counter atomic.Uint64
	return func() (string, error) {
		return fmt.Sprintf("%s-workspace-g%s-%d", machineID, generation, counter.Add(1)), nil
	}
}

func newWorkspaceLanes(ctx context.Context, cfg globalconfig.Config, scheduling orchestrator.SchedulingSource, generation string, logger *slog.Logger, support workspacerunner.Support) []*workspacerunner.Lane {
	if logger == nil {
		logger = slog.Default()
	}
	if !workspaceLaneEnabled(workspaceLaneCapabilities(ctx, cfg, support)) {
		return nil
	}
	scheduler, ok := scheduling.(workspaceLaneScheduler)
	if !ok {
		return nil
	}
	machineID := scheduler.MachineID()
	if machineID == "" {
		return nil
	}
	sessionIDs := workspaceSessionIDs(machineID, generation)
	projects := project.ManagerConfigFromGlobal(cfg).Projects
	lanes := make([]*workspacerunner.Lane, 0, len(cfg.Client.NativeProjects))
	for name := range cfg.Client.NativeProjects {
		native, found := scheduler.NativeClient(name)
		if !found {
			logger.Warn("workspace.lane_skipped", "project_id", name, "reason", "no hub client for the project")
			continue
		}
		lane, err := newWorkspaceLane(ctx, projects, name, native, machineID, sessionIDs, logger, support)
		if err != nil {
			logger.Warn("workspace.lane_skipped", "project_id", name, "error", err)
			continue
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

func newWorkspaceLane(
	ctx context.Context,
	projects []globalconfig.Project,
	projectID string,
	native *hubclient.NativeClient,
	machineID tracker.MachineID,
	sessionIDs func() (string, error),
	logger *slog.Logger,
	support workspacerunner.Support,
) (*workspacerunner.Lane, error) {
	selected, found := workspaceLaneProject(projects, projectID)
	if !found {
		return nil, fmt.Errorf("configured project %q was not found", projectID)
	}
	workflow, err := project.LoadWorkflowContext(ctx, selected)
	if err != nil {
		return nil, fmt.Errorf("load workflow for %s: %w", projectID, err)
	}
	workflow.Config = project.MapNativeTracker(workflow.Config, true)
	workflow, err = native.ResolveProjectWorkflow(ctx, workflow, nil)
	if err != nil {
		return nil, fmt.Errorf("load shared configuration for %s: %w", projectID, err)
	}
	descriptor, err := project.ResolvePolicy(selected, workflow)
	if err != nil {
		return nil, fmt.Errorf("resolve policy for %s: %w", projectID, err)
	}
	claimer, err := hubclient.NewWorkspaceClaimer(native, hubclient.WorkspaceLaneConfig{
		PolicyID: descriptor.ID, MachineID: machineID, SessionID: sessionIDs,
	})
	if err != nil {
		return nil, err
	}
	backend, err := buildWorkspaceBackendContext(ctx, workflow.Config, selected.Workdir, logger)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		logger.Debug("workspace.hostname_unknown", "project_id", projectID, "error", err)
	}
	return workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer:  claimer,
		Hub:      claimer.Native(),
		Worktree: &workspacerunner.GitWorktree{Backend: backend, ProjectID: projectID, Resolve: claimer.RunIdentifier},
		Logger:   logger,
		Hostname: hostname,
		Support:  support,
		Shell:    workflow.Config.Hooks.Shell,
		Reporter: workspaceActionRunReporter{native: claimer.Native()},
	})
}

type workspaceActionRunReporter struct {
	native *hubclient.NativeClient
}

func (r workspaceActionRunReporter) ReportActionRun(ctx context.Context, workspaceID string, identity hubclient.WorkspaceIdentity, run workspacerunner.ActionRun) (workspacesession.Run, error) {
	return r.native.ReportWorkspaceActionRun(ctx, workspaceID, hubclient.WorkspaceActionRunReport{
		WorkspaceIdentity: identity, ActionID: run.ActionID, RunID: run.RunID, Status: run.Status,
		ExitCode: run.ExitCode, Reason: run.Reason, StartedAt: run.StartedAt,
		FinishedAt: run.FinishedAt, Output: run.Output, Truncated: run.Truncated,
	})
}

func workspaceLaneProject(projects []globalconfig.Project, projectID string) (globalconfig.Project, bool) {
	for _, selected := range projects {
		if selected.ID == projectID {
			return selected, true
		}
	}
	return globalconfig.Project{}, false
}

func runWorkspaceLane(ctx context.Context, lane *workspacerunner.Lane, logger *slog.Logger) {
	if err := lane.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Warn("workspace.lane_stopped", "error", err)
	}
}
