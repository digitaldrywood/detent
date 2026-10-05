package project

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
)

type ManagedConfigRequest struct {
	ProjectID              string `json:"project_id"`
	ExpectedConfigRevision string `json:"expected_config_revision,omitempty"`
	ExpectedPolicyID       string `json:"expected_policy_id,omitempty"`
	PolicyID               string `json:"policy_id,omitempty"`
	SourceRevision         string `json:"source_revision,omitempty"`
	Checkpoint             string `json:"checkpoint,omitempty"`
}

type ManagedConfigView struct {
	ProjectID         string             `json:"project_id"`
	Authority         string             `json:"authority"`
	ConfigRevision    string             `json:"config_revision,omitempty"`
	Registered        bool               `json:"registered"`
	RuntimeRegistered bool               `json:"runtime_registered"`
	Paused            bool               `json:"paused"`
	Draining          bool               `json:"draining"`
	UnsettledAttempts int                `json:"unsettled_attempts"`
	Source            string             `json:"source"`
	SelectedPolicy    *policy.Descriptor `json:"selected_policy,omitempty"`
	EffectivePolicy   *policy.Descriptor `json:"effective_policy,omitempty"`
	Saved             bool               `json:"saved"`
	Applied           bool               `json:"applied"`
	Constraint        string             `json:"constraint,omitempty"`
	ObservedAt        time.Time          `json:"observed_at"`
}

type CutoverVerifier func(context.Context, globalconfig.Config, string, string, string) error

type ConfigurationOwner struct {
	selected globalconfig.Config
	runtime  func() globalconfig.Config
	manager  *Manager
	attempts store.WorkAttemptStore
	handoff  CutoverVerifier
}

func NewConfigurationOwner(selected globalconfig.Config, runtime func() globalconfig.Config, manager *Manager, attempts store.WorkAttemptStore, handoff CutoverVerifier) *ConfigurationOwner {
	return &ConfigurationOwner{selected: selected, runtime: runtime, manager: manager, attempts: attempts, handoff: handoff}
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
	err := globalconfig.Mutate(o.selected.Path, func(cfg *globalconfig.Config, revision string) bool {
		view = o.observe(ctx, *cfg, revision, id)
		return false
	}, globalconfig.WithProjectPathLiterals())
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
	err := globalconfig.Mutate(o.selected.Path, func(cfg *globalconfig.Config, revision string) bool {
		view = o.observe(ctx, *cfg, revision, request.ProjectID)
		if view.Constraint != "" || ctx.Err() != nil {
			return false
		}
		if revision != request.ExpectedConfigRevision {
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
		case "apply_local_project_policy":
			if !p.Paused() {
				view.Constraint = "Finish current work and pause the selected project through its existing owner before applying policy."
				return false
			}
			p.configMu.Lock()
			defer p.configMu.Unlock()
			workflow, loadErr := p.loadManagedWorkflow(ctx, request.SourceRevision)
			if loadErr != nil {
				view.Constraint = "The configured committed workflow revision is unavailable or has local overlays."
				return false
			}
			candidate := workflow
			candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
			descriptor, err := ResolvePolicy(p.Config(), candidate)
			if err != nil || descriptor.ID != request.PolicyID {
				view.Constraint = "The selected source policy identity changed; read it before retrying."
				return false
			}
			if err := p.requireSettledWork(ctx); err != nil || view.UnsettledAttempts != 0 {
				view.Constraint = "Active or deferred work must settle through its existing completion owner before applying policy."
				return false
			}
			if _, ok := p.policyScheduling.(policyChecker); !ok {
				view.Constraint = "The selected approved-policy owner is unavailable."
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
	}, globalconfig.WithProjectPathLiterals())
	if err != nil {
		view.Applied = false
		view.Constraint = "The selected local configuration could not be validated or written."
		return view
	}
	if saved {
		readErr := globalconfig.Mutate(o.selected.Path, func(cfg *globalconfig.Config, revision string) bool {
			view = o.observe(ctx, *cfg, revision, request.ProjectID)
			view.Saved = !view.Registered
			if view.Registered {
				view.Constraint = "The selected registration changed after removal was saved; read the current configuration before retrying."
			}
			return false
		}, globalconfig.WithProjectPathLiterals())
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
	if !sameProjectConfig(selected, current) {
		view.Constraint = "The existing configuration reload has not applied the selected project settings."
		return view
	}
	workflow := p.Workflow()
	view.EffectivePolicy = &workflow.Config.Policy
	if workflow.Config.Policy.ID == "" {
		descriptor, err := ResolvePolicy(current, workflow)
		if err == nil {
			view.EffectivePolicy = &descriptor
		}
	}
	if current.WorkflowRef != "" || workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud {
		view.Source = "configured_committed_workflow"
		if workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud {
			view.Source = "cloud_workflow"
		}
		candidate, err := p.loadManagedWorkflow(ctx, "")
		if err == nil {
			candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
			descriptor, err := ResolvePolicy(current, candidate)
			if err == nil {
				view.SelectedPolicy = &descriptor
			}
		}
	} else {
		view.Source = "configured_local_workflow_read_only"
	}
	if orch := p.Orchestrator(); orch != nil && p.Running() {
		state, err := orch.State(ctx)
		if err != nil {
			view.Constraint = "The selected runtime work state is unavailable."
			return view
		}
		view.Draining, view.UnsettledAttempts = state.Draining, state.UnsettledWork()
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
	view.UnsettledAttempts = max(view.UnsettledAttempts, len(attempts))
	return view
}

func (p *Project) loadManagedWorkflow(ctx context.Context, revision string) (workflowconfig.Workflow, error) {
	cfg := p.Config()
	if cfg.WorkflowRef == "" && p.Workflow().Definition.Layout == workflowconfig.ProjectDefinitionCloud {
		workflow, err := loadWorkflowForScheduling(ctx, cfg, p.policyScheduling)
		if err == nil && revision != "" && workflow.Definition.Revision != revision {
			return workflowconfig.Workflow{}, errors.New("Cloud workflow revision changed")
		}
		return workflow, err
	}
	workflow, err := loadManagedWorkflow(ctx, cfg, revision)
	if errors.Is(err, workflowconfig.ErrNoProjectDefinition) {
		workflow, err = loadWorkflowForScheduling(ctx, cfg, p.policyScheduling)
		if err == nil && revision != "" && workflow.Definition.Revision != revision {
			return workflowconfig.Workflow{}, errors.New("Cloud workflow revision changed")
		}
	}
	return workflow, err
}

func loadManagedWorkflow(ctx context.Context, cfg globalconfig.Project, revision string) (workflowconfig.Workflow, error) {
	source, err := newWorkflowGitRefSource(cfg)
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	for _, path := range []string{source.localPath(), source.localConfigPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return workflowconfig.Workflow{}, errors.New("local workflow overlays are not a committed policy source")
		}
	}
	workflow, loaded, err := source.loadSource(ctx, false)
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	if revision != "" && loaded != revision {
		return workflowconfig.Workflow{}, errors.New("configured workflow revision changed")
	}
	return workflow, nil
}

func (p *Project) requireSettledWork(ctx context.Context) error {
	if p.Running() {
		return errors.New("project is running")
	}
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
