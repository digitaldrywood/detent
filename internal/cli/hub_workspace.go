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

// Wiring for the runner's workspace lane (decisions section 18.1, sequenced by
// 18.11 step 2).
//
// A workspace is its own work item kind and is claimed in a lane of its own,
// beside dispatch rather than through it, because it "does not end when a turn
// ends, and it stays claimed until the workspace closes". Everything that lane
// needs is per project: its own hub client, the project's approved policy, and
// the worktree backend the project's own runs use.

// workspaceLaneEnabled reports whether this runner should run a workspace lane
// at all.
//
// Section 18.1 lets a runner claim a workspace "only if it reports every
// capability in requires with a fresh heartbeat", and a request that names no
// capability still gets `files` and `diff`. A runner that cannot serve files
// would therefore be refused every claim it made, so it starts no lane and
// never asks: the gate here is the same condition the heartbeat reports, so
// the two can never disagree.
func workspaceLaneEnabled(capabilities workspacesession.Capabilities) bool {
	return capabilities.Files
}

// workspaceLaneCapabilities is what this runner reports for workspace sessions
// on its machine heartbeat, and the zero value when it runs no workspace lane.
// The hub's claim gate reads the report from the enrolled runner's identity, so
// a runner without an enrolled identity or without native projects could never
// pass it: it reports nothing and starts no lane. The heartbeat report and the
// lane start both read this, so the two can never disagree. Git is reported
// only when every native project's worktree backend produces git worktrees,
// because the report is one answer for the whole runner.
func workspaceLaneCapabilities(ctx context.Context, cfg globalconfig.Config) workspacesession.Capabilities {
	client := cfg.Client
	capabilities := workspacerunner.Capabilities(workspacerunner.DefaultSupport())
	if strings.TrimSpace(client.IdentityFile) == "" || len(client.NativeProjects) == 0 || !workspaceLaneEnabled(capabilities) {
		return workspacesession.Capabilities{}
	}
	capabilities.Git = capabilities.Git && nativeProjectsUseGit(ctx, cfg)
	return capabilities
}

// nativeProjectsUseGit reports whether every native project resolves to a
// git worktree backend. A project that cannot be resolved counts as not git.
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

// workspaceLaneScheduler is the part of the hub scheduler a workspace lane
// needs. It is an interface so the boot path depends on the two accessors
// rather than on the concrete scheduler.
type workspaceLaneScheduler interface {
	NativeClient(project string) (*hubclient.NativeClient, bool)
	MachineID() tracker.MachineID
}

// workspaceSessionIDs hands every claim a session id of its own.
//
// The hub pins a policy per lease session and answers a session that already
// holds a lease with that lease rather than a new one. Sharing the issue
// lane's sessions would therefore hand the workspace lane a run's lease, so
// the ids are both unique and visibly distinct.
func workspaceSessionIDs(machineID tracker.MachineID) func() (string, error) {
	var counter atomic.Uint64
	return func() (string, error) {
		return fmt.Sprintf("%s-workspace-%d", machineID, counter.Add(1)), nil
	}
}

// newWorkspaceLanes builds one lane per configured native project.
//
// A project whose policy or worktree backend cannot be resolved is skipped
// with a warning rather than failing the daemon: the workspace surfaces are an
// addition to a runner whose real job is dispatch, and a misconfigured one
// must not stop it from running.
func newWorkspaceLanes(ctx context.Context, cfg globalconfig.Config, scheduling orchestrator.SchedulingSource, logger *slog.Logger) []*workspacerunner.Lane {
	if logger == nil {
		logger = slog.Default()
	}
	if !workspaceLaneEnabled(workspaceLaneCapabilities(ctx, cfg)) {
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
	sessionIDs := workspaceSessionIDs(machineID)
	projects := project.ManagerConfigFromGlobal(cfg).Projects
	lanes := make([]*workspacerunner.Lane, 0, len(cfg.Client.NativeProjects))
	for name := range cfg.Client.NativeProjects {
		native, found := scheduler.NativeClient(name)
		if !found {
			logger.Warn("workspace.lane_skipped", "project_id", name, "reason", "no hub client for the project")
			continue
		}
		lane, err := newWorkspaceLane(ctx, projects, name, native, machineID, sessionIDs, logger)
		if err != nil {
			logger.Warn("workspace.lane_skipped", "project_id", name, "error", err)
			continue
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

// newWorkspaceLane resolves one project's policy and worktree backend and
// builds its lane.
func newWorkspaceLane(
	ctx context.Context,
	projects []globalconfig.Project,
	projectID string,
	native *hubclient.NativeClient,
	machineID tracker.MachineID,
	sessionIDs func() (string, error),
	logger *slog.Logger,
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
	// LeaseTTL is left to the claimer's default on purpose: a workspace claim
	// is held for the 90 seconds section 18.1 gives a workspace before
	// lease_lost, which is the workspace heartbeat's clock rather than the
	// runner's configured issue-lane TTL.
	claimer, err := hubclient.NewWorkspaceClaimer(native, hubclient.WorkspaceLaneConfig{
		PolicyID: descriptor.ID, MachineID: machineID, SessionID: sessionIDs,
	})
	if err != nil {
		return nil, err
	}
	// The workspace uses the project's own worktree backend, so a fresh
	// workspace lands under the configured workspace root beside every other
	// worktree rather than inside the repository. GitWorktree keeps a fresh
	// workspace off the issue's identifier, which is what stops it holding the
	// branch an ordinary run of the same issue needs (section 18.11 step 2).
	backend, err := buildWorkspaceBackend(workflow.Config, selected.Workdir, logger)
	if err != nil {
		return nil, err
	}
	// The hostname is best effort: a host that cannot name itself still serves
	// every channel, and the picker simply cannot claim the worktree is local.
	hostname, err := os.Hostname()
	if err != nil {
		logger.Debug("workspace.hostname_unknown", "project_id", projectID, "error", err)
	}
	// The shell is the project's own, the one its workspace hooks already run
	// through (runner.go builds workspace.Hooks from the same field), so a
	// terminal opens the shell the project's commands were written for.
	return workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer:  claimer,
		Hub:      claimer.Native(),
		Worktree: &workspacerunner.GitWorktree{Backend: backend, ProjectID: projectID, Resolve: claimer.RunIdentifier},
		Logger:   logger,
		Hostname: hostname,
		// The same Support the heartbeat reports (hub_client.go), so the
		// capability the hub gates a claim on and the one this lane's sessions
		// bind with can never disagree.
		Support: workspacerunner.DefaultSupport(),
		Shell:   workflow.Config.Hooks.Shell,
		// The reporter turns a run nobody watched into a row somebody can read
		// (section 18.12).
		Reporter: workspaceActionRunReporter{native: claimer.Native()},
	})
}

// workspaceActionRunReporter adapts the native client onto the lane's
// ActionRunReporter seam.
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

// workspaceLaneProject finds a configured project by id.
func workspaceLaneProject(projects []globalconfig.Project, projectID string) (globalconfig.Project, bool) {
	for _, selected := range projects {
		if selected.ID == projectID {
			return selected, true
		}
	}
	return globalconfig.Project{}, false
}

// runWorkspaceLane runs one lane until ctx is cancelled. A lane that stops on
// its own is a bug rather than a shutdown, so it is logged; the caller waits
// for this to return before the process exits, which is what gives every open
// workspace the chance to unbind instead of timing out unreachable.
func runWorkspaceLane(ctx context.Context, lane *workspacerunner.Lane, logger *slog.Logger) {
	if err := lane.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Warn("workspace.lane_stopped", "error", err)
	}
}
