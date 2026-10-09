package project

import (
	"context"
	"errors"
	"slices"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
)

type ManagedConfigRequest = runnerauth.ProjectConfigurationRequest

type ManagedConfigView = runnerauth.ProjectConfiguration

type CutoverVerifier func(context.Context, globalconfig.Config, string, string, string) error

type ConfigurationOwner struct {
	selected globalconfig.Config
	runtime  func() globalconfig.Config
	manager  *Manager
	attempts store.WorkAttemptStore
	handoff  CutoverVerifier
	settings store.LocalConfigurationStore
}

func NewConfigurationOwner(ctx context.Context, selected globalconfig.Config, runtime func() globalconfig.Config, manager *Manager, attempts store.WorkAttemptStore, handoff CutoverVerifier) *ConfigurationOwner {
	owner := &ConfigurationOwner{selected: selected, runtime: runtime, manager: manager, attempts: attempts, handoff: handoff}
	if !selected.Client.Configured() {
		if settings, ok := attempts.(store.LocalConfigurationStore); ok {
			if _, err := settings.LocalConfiguration(ctx, selected); err == nil {
				owner.settings = settings
			}
		}
	}
	return owner
}

func MissingConfigurationOwner(id string) ManagedConfigView {
	return ManagedConfigView{ProjectID: id, Authority: "local_global_configuration", Source: "unavailable", Constraint: "The selected local configuration owner is not installed or is stopped. Connect to its supported configuration service; Cloud routing does not edit a local board or start it.", ObservedAt: time.Now().UTC()}
}

func (o *ConfigurationOwner) Read(ctx context.Context, id string) ManagedConfigView {
	if o == nil || o.manager == nil || o.runtime == nil || o.selected.Path == "" {
		return MissingConfigurationOwner(id)
	}
	o.manager.operationMu.Lock()
	defer o.manager.operationMu.Unlock()
	view := MissingConfigurationOwner(id)
	err := o.mutate(ctx, func(cfg *globalconfig.Config, revision string) bool {
		view = o.observe(ctx, *cfg, revision, id)
		return false
	})
	if err != nil {
		view.Constraint = "The selected local configuration cannot be read or validated."
	}
	return view
}

func (o *ConfigurationOwner) Apply(ctx context.Context, operation string, request ManagedConfigRequest) ManagedConfigView {
	if o == nil || o.manager == nil || o.runtime == nil || o.selected.Path == "" {
		return MissingConfigurationOwner(request.ProjectID)
	}
	o.manager.operationMu.Lock()
	defer o.manager.operationMu.Unlock()
	view := MissingConfigurationOwner(request.ProjectID)
	saved := false
	var resume *Project
	var resumed *Project
	var wasPaused bool
	var draining bool
	err := o.mutate(ctx, func(cfg *globalconfig.Config, revision string) bool {
		view = o.observe(ctx, *cfg, revision, request.ProjectID)
		if view.Constraint != "" || ctx.Err() != nil {
			return false
		}
		if view.ConfigRevision != request.ExpectedConfigRevision {
			view.Constraint = "The selected configuration revision changed; read it before retrying."
			return false
		}
		p, ok := o.manager.registry.Get(ID(request.ProjectID))
		if !ok || p == nil {
			view.Constraint = "The selected runtime project is unavailable; no project was started."
			return false
		}
		if view.EffectivePolicy == nil || view.EffectivePolicy.ID != request.ExpectedPolicyID {
			view.Constraint = "The effective policy identity changed; read it before retrying."
			return false
		}
		switch operation {
		case "resume_local_project":
			wasPaused = view.Paused
			if err := o.manager.unpauseLocked(ctx, p.ID(), false); err != nil {
				view = o.observe(ctx, *cfg, revision, request.ProjectID)
				view.Constraint = "The selected project could not be resumed through its configuration owner. Active or deferred work must settle before resuming a drained project; then read its configuration and retry."
				return false
			}
			resumed = p
			for i := range cfg.Projects {
				if cfg.Projects[i].ID == request.ProjectID {
					cfg.Projects[i].Paused = false
					cfg.Projects[i].PausedReason = ""
					cfg.Projects[i].PausedAt = ""
					cfg.Projects[i].PausedUntilIssue = ""
					cfg.Projects[i].PausedUntil = ""
				}
			}
			saved = true
			return true
		case "apply_local_project_policy":
			if p.Running() && !view.Paused {
				resume, draining = p, view.Draining
			}
			if request.AllowLocalBinding != nil {
				view = o.applyLocalBinding(ctx, *cfg, revision, p, request, view)
				return false
			}
			if view.UnsettledAttempts != 0 {
				view.Constraint = "Active or deferred work must settle through its existing completion owner before applying policy."
				return false
			}
			workflow, loadErr := p.loadManagedWorkflow(ctx, request.SourceRevision)
			if loadErr != nil {
				view.Constraint = loadErr.Error()
				return false
			}
			candidate := workflow
			candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
			descriptor, err := ResolvePolicy(p.Config(), candidate)
			if err != nil || descriptor.ID != request.PolicyID || descriptor.SourceRevision != request.SourceRevision {
				view.Constraint = "The selected source policy identity changed; read it before retrying."
				return false
			}
			checker, ok := p.policyScheduling.(policyChecker)
			if !ok || checker.CheckProjectPolicy(ctx, p.Config().ID, candidate.Config.Tracker.Repository, descriptor) != nil {
				view.Constraint = "The selected approved-policy owner is unavailable."
				return false
			}
			if err := pauseSettledConfiguration(ctx, p, view); err != nil {
				view.Constraint = "Active or deferred work must settle through its existing completion owner before applying policy."
				return false
			}
			p.configMu.Lock()
			defer p.configMu.Unlock()
			if latest := o.observe(ctx, *cfg, revision, request.ProjectID); latest.Constraint != "" || latest.ConfigRevision != request.ExpectedConfigRevision || latest.EffectivePolicy == nil || latest.EffectivePolicy.ID != request.ExpectedPolicyID {
				view.Constraint = "The selected configuration revision changed; read it before retrying."
				return false
			}
			if err := p.applyWorkflowUpdate(ctx, configwatcher.Update{Workflow: workflow, At: time.Now().UTC()}, true); err != nil {
				view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
				return false
			}
			view = o.observe(ctx, *cfg, revision, request.ProjectID)
			view.Applied = view.Constraint == "" && view.EffectivePolicy != nil && view.EffectivePolicy.ID == request.PolicyID && view.EffectivePolicy.SourceRevision == request.SourceRevision
			return false
		case "drain_local_project", "detach_local_project":
			workflow := p.Workflow().Config
			if workflow.Intake.Enabled() || len(workflow.Routines) != 0 || workflow.BacklogAdmission.Enabled || workflow.Retro.Enabled {
				view.Constraint = "This workflow has local intake or schedules; migrate them through the existing workflow owner before cutover."
				return false
			}
			if orch := p.Orchestrator(); operation == "drain_local_project" && orch != nil && p.Running() {
				if err := orch.Drain(ctx); err != nil {
					view.Constraint = "The selected project drain could not be acknowledged."
					return false
				}
			}
			view = o.observe(ctx, *cfg, revision, request.ProjectID)
			if operation == "drain_local_project" {
				view.Applied = view.Constraint == "" && (view.Draining || view.Paused)
				return false
			}
			if view.Constraint != "" || view.UnsettledAttempts != 0 || !view.Draining && !view.Paused {
				view.Constraint = "Active or deferred work must settle through its existing completion owner before detach."
				return false
			}
			if o.handoff == nil || o.handoff(ctx, *cfg, request.ProjectID, workflow.Tracker.Repository, request.Checkpoint) != nil {
				view.Constraint = "Durable handoff is incomplete or unavailable; verify the exact mapped Cloud cutover receipt."
				return false
			}
			if ctx.Err() != nil {
				return false
			}
			cfg.Projects = slices.DeleteFunc(slices.Clone(cfg.Projects), func(p globalconfig.Project) bool { return p.ID == request.ProjectID })
			saved = true
			return true
		default:
			view.Constraint = "The configuration operation is unsupported."
			return false
		}
	})
	if err != nil {
		if resumed != nil && wasPaused {
			if pauseErr := resumed.Pause(ctx); pauseErr != nil {
				view.Constraint = "The selected configuration could not be saved and the project could not be paused again."
				view.Applied = false
				return view
			}
		}
		view.Applied = false
		view.Constraint = "The selected local configuration could not be validated or written."
		return view
	}
	if resume != nil {
		applied, constraint := view.Applied, view.Constraint
		if err := o.manager.unpauseLocked(ctx, resume.ID(), draining); err != nil {
			view.Applied = false
			view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
			return view
		}
		savedBinding := view.Saved
		readErr := o.mutate(ctx, func(cfg *globalconfig.Config, revision string) bool {
			view = o.observe(ctx, *cfg, revision, request.ProjectID)
			view.Saved = savedBinding
			view.Applied = applied && view.Constraint == "" && view.EffectivePolicy != nil && view.EffectivePolicy.ID == request.PolicyID && view.EffectivePolicy.SourceRevision == request.SourceRevision
			if constraint != "" {
				view.Constraint = constraint
			}
			return false
		})
		if readErr != nil {
			view.Applied = false
			view.Constraint = "The saved configuration receipt cannot be read."
		}
	}
	if saved {
		readErr := o.mutate(ctx, func(cfg *globalconfig.Config, revision string) bool {
			if resumed != nil {
				resumed.mu.Lock()
				resumed.cfg.PausedReason = ""
				resumed.cfg.PausedAt = ""
				resumed.cfg.PausedUntilIssue = ""
				resumed.cfg.PausedUntil = ""
				resumed.mu.Unlock()
				view = o.observe(ctx, *cfg, revision, request.ProjectID)
				view.Saved = view.Registered
				view.Applied = view.Constraint == "" && !view.Paused && !view.Draining && resumed.Running()
				return false
			}
			view = o.observe(ctx, *cfg, revision, request.ProjectID)
			view.Saved = !view.Registered
			if view.Registered {
				view.Constraint = "The selected registration changed after removal was saved; read the current configuration before retrying."
			}
			return false
		})
		if readErr != nil {
			view.Constraint = "The saved configuration receipt cannot be read."
			return view
		}
	}
	return view
}

func (o *ConfigurationOwner) observe(ctx context.Context, cfg globalconfig.Config, revision, id string) ManagedConfigView {
	view := ManagedConfigView{ProjectID: id, Authority: "local_global_configuration", ConfigRevision: revision, Source: "unavailable", ObservedAt: time.Now().UTC()}
	o.manager.mu.Lock()
	running := o.manager.running
	o.manager.mu.Unlock()
	if !running || o.runtime().Path != o.selected.Path {
		return MissingConfigurationOwner(id)
	}
	var selected globalconfig.Project
	for _, p := range ManagerConfigFromGlobal(cfg).Projects {
		if p.ID == id {
			selected, view.Registered = p, true
			view.LocalIntakeEnabled = p.LocalIntakeOn()
			break
		}
	}
	p, ok := o.manager.registry.Get(ID(id))
	view.RuntimeRegistered = ok && p != nil
	if view.RuntimeRegistered {
		view.Paused = p.Paused()
	}
	if !view.Registered {
		if view.RuntimeRegistered {
			view.Constraint = "Removal is saved; the existing configuration reload must confirm runtime detach."
		}
		return view
	}
	if !view.RuntimeRegistered {
		view.Constraint = "The selected runtime project is unavailable; no project was started."
		return view
	}
	view.Paused = p.Paused()
	current := p.Config()
	selected.Paused = current.Paused
	if !sameProjectConfig(selected, current) {
		view.Constraint = "The existing configuration reload has not applied the selected project settings."
		return view
	}
	workflow := p.Workflow()
	view.AllowLocalBinding = workflow.Config.Worker.EffectiveAllowLocalBinding()
	view.EffectivePolicy = &workflow.Config.Policy
	if workflow.Config.Policy.ID == "" {
		descriptor, err := ResolvePolicy(current, workflow)
		if err == nil {
			view.EffectivePolicy = &descriptor
		}
	}
	if workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud {
		view.Source = "cloud_workflow"
	} else if current.WorkflowRef != "" {
		view.Source = "configured_committed_workflow"
	} else {
		view.Source = "configured_local_workflow_read_only"
	}
	if loaded, err := loadWorkflowForScheduling(ctx, current, p.policyScheduling); err == nil {
		selected := loaded
		selected.Config = WithMappedNativeTracker(selected.Config, p.policyScheduling, p.ID())
		if source, ok := p.policyScheduling.(interface {
			ResolveProjectWorkflow(context.Context, string, workflowconfig.Workflow, *policy.RepositorySource) (workflowconfig.Workflow, error)
		}); ok && selected.Config.Tracker.Kind == workflowconfig.TrackerHubNative && selected.Definition.Layout != workflowconfig.ProjectDefinitionCloud {
			var provenance *policy.RepositorySource
			selected, provenance = repositoryPolicySource(ctx, current, selected)
			resolved, err := source.ResolveProjectWorkflow(ctx, current.ID, selected, provenance)
			if err != nil {
				view.Constraint = "The approved shared project configuration cannot be loaded; retry through the existing policy owner."
				return view
			}
			selected = resolved
		}
		if descriptor, err := ResolvePolicy(current, selected); err == nil {
			view.SelectedPolicy = &descriptor
		}
		view.ConfigRevision = policy.Digest([]byte(revision + "\x00" + loaded.SourceHash))
		for _, enabled := range []bool{true, false} {
			candidate, _, _, err := localBindingWorkflow(ctx, current, enabled)
			if err != nil {
				continue
			}
			candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
			descriptor, err := ResolvePolicy(current, candidate)
			if err != nil {
				continue
			}
			if enabled {
				view.LocalBindingPolicy = &descriptor
			} else {
				view.RestrictedBindingPolicy = &descriptor
			}
		}
	} else {
		view.Constraint = "The selected local configuration cannot be read or validated."
		return view
	}
	var diagnosticState orchestrator.State
	if orch := p.Orchestrator(); orch != nil && p.Running() {
		state, err := orch.State(ctx)
		if err != nil {
			view.Constraint = "The selected runtime work state is unavailable."
			return view
		}
		diagnosticState = state
		view.Draining, view.UnsettledAttempts = state.Draining, state.UnsettledWork()
		view.LocalIntakeEnabled = state.LocalIntake.Enabled
		view.LocalIntakeRemaining = state.LocalIntake.Remaining
		view.LocalIntakeBlocked = state.LocalIntake.Blocked
	}
	if o.attempts == nil {
		view.Constraint = "The durable local attempt owner is unavailable."
		return view
	}
	attempts, err := o.attempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: id})
	if err != nil {
		view.Constraint = "The durable local attempt handoff could not be read."
		return view
	}
	view.Diagnostics = diagnosticState.ProjectDiagnostics(attempts, time.Now().UTC())
	if !p.Running() {
		view.Diagnostics.Unavailable["runtime"] = "stopped"
	}
	view.UnsettledAttempts = max(view.UnsettledAttempts, len(attempts))
	return view
}

func (p *Project) loadManagedWorkflow(ctx context.Context, revision string) (workflowconfig.Workflow, error) {
	cfg := p.Config()
	workflow, err := loadWorkflowForScheduling(ctx, cfg, p.policyScheduling)
	if err != nil {
		p.logger.WarnContext(ctx, "load managed project definition", "project_id", cfg.ID, "error", err)
		return workflowconfig.Workflow{}, errors.New("the configured project definition cannot be read or validated; repair it through its configuration owner before retrying")
	}
	workflow.Config = WithMappedNativeTracker(workflow.Config, p.policyScheduling, p.ID())
	if source, ok := p.policyScheduling.(interface {
		ResolveProjectWorkflow(context.Context, string, workflowconfig.Workflow, *policy.RepositorySource) (workflowconfig.Workflow, error)
	}); ok && workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative && workflow.Definition.Layout != workflowconfig.ProjectDefinitionCloud {
		var provenance *policy.RepositorySource
		workflow, provenance = repositoryPolicySource(ctx, cfg, workflow)
		workflow, err = source.ResolveProjectWorkflow(ctx, cfg.ID, workflow, provenance)
		if err != nil {
			p.logger.WarnContext(ctx, "load managed approved policy", "project_id", cfg.ID, "error", err)
			return workflowconfig.Workflow{}, errors.New("the approved shared project configuration cannot be loaded; retry through the existing policy owner")
		}
	}
	candidate := workflow
	candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
	descriptor, err := ResolvePolicy(cfg, candidate)
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	if revision != "" && descriptor.SourceRevision != revision {
		return workflowconfig.Workflow{}, errors.New("the selected source policy revision changed; read the approved policy and configuration before retrying")
	}
	workflow.Config.Policy = descriptor
	return workflow, nil
}

func (p *Project) requireSettledWork(ctx context.Context) error {
	if p.Running() {
		return errors.New("project is running")
	}
	return p.requireSettledAttempts(ctx)
}

func (p *Project) requireSettledAttempts(ctx context.Context) error {
	if p.orchDeps.WorkAttempts == nil {
		return errors.New("durable attempt owner is unavailable")
	}
	attempts, err := p.orchDeps.WorkAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: string(p.ID())})
	if err != nil {
		return err
	}
	if len(attempts) != 0 {
		return errors.New("project has unsettled attempts")
	}
	return nil
}

func pauseSettledConfiguration(ctx context.Context, p *Project, view ManagedConfigView) error {
	if orch := p.Orchestrator(); orch != nil && p.Running() {
		if err := orch.Drain(ctx); err != nil {
			return err
		}
		state, err := orch.State(ctx)
		if err != nil {
			return err
		}
		view.UnsettledAttempts = max(view.UnsettledAttempts, state.UnsettledWork())
	}
	if view.UnsettledAttempts != 0 {
		return errors.New("project work must settle before configuration application")
	}
	if err := p.Pause(ctx); err != nil {
		return err
	}
	return p.requireSettledWork(ctx)
}

func (o *ConfigurationOwner) mutate(ctx context.Context, mutate func(*globalconfig.Config, string) bool) error {
	if o.settings == nil {
		return globalconfig.Mutate(o.selected.Path, mutate, globalconfig.WithProjectPathLiterals())
	}
	cfg, err := globalconfig.Read(o.selected.Path, globalconfig.WithoutProjectConfiguration(), globalconfig.WithProjectPathLiterals())
	if err != nil {
		return err
	}
	return o.settings.MutateLocalConfiguration(ctx, cfg, mutate)
}
