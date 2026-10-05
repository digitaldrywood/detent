package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/policy"
)

func localBindingWorkflow(ctx context.Context, cfg globalconfig.Project, enabled bool) (workflowconfig.Workflow, string, []byte, error) {
	current, err := LoadWorkflowContext(ctx, cfg)
	if err != nil || current.Definition.Layout != workflowconfig.ProjectDefinitionSplit {
		return workflowconfig.Workflow{}, "", nil, errors.New("a configured split project definition is required")
	}
	path := current.Definition.LocalConfigPath
	var source workflowGitRefSource
	if cfg.WorkflowRef != "" {
		source, err = newWorkflowGitRefSource(cfg)
		if err != nil {
			return workflowconfig.Workflow{}, "", nil, err
		}
		path = source.localConfigPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return workflowconfig.Workflow{}, "", nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return workflowconfig.Workflow{}, "", nil, errors.New("the configured local definition must be a regular file")
	}
	if len(raw) == 0 {
		raw = []byte("schema: 1\n")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return workflowconfig.Workflow{}, "", nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return workflowconfig.Workflow{}, "", nil, errors.New("the configured local definition must be a mapping")
	}
	root := document.Content[0]
	var worker *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "worker" {
			worker = root.Content[i+1]
		}
	}
	if worker == nil {
		worker = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "worker"}, worker)
	}
	if worker.Kind != yaml.MappingNode {
		return workflowconfig.Workflow{}, "", nil, errors.New("the configured worker must be a mapping")
	}
	var setting *yaml.Node
	for i := 0; i < len(worker.Content); i += 2 {
		if worker.Content[i].Value == "allow_local_binding" {
			setting = worker.Content[i+1]
		}
	}
	if setting == nil {
		setting = &yaml.Node{}
		worker.Content = append(worker.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "allow_local_binding"}, setting)
	}
	setting.Kind, setting.Tag, setting.Value = yaml.ScalarNode, "!!bool", strconv.FormatBool(enabled)
	updated, err := yaml.Marshal(&document)
	if err != nil {
		return workflowconfig.Workflow{}, "", nil, err
	}
	if cfg.WorkflowRef == "" {
		candidate, err := workflowconfig.LoadProjectDefinitionWithLocalConfig(cfg.Workflow, updated)
		return candidate, path, updated, err
	}
	source.readFile = func(selected string) ([]byte, error) {
		if selected == path {
			return updated, nil
		}
		return os.ReadFile(selected)
	}
	candidate, _, err := source.load(ctx)
	return candidate, path, updated, err
}

func (o *ConfigurationOwner) applyLocalBinding(ctx context.Context, cfg globalconfig.Config, revision string, p *Project, request ManagedConfigRequest, view ManagedConfigView) ManagedConfigView {
	if view.UnsettledAttempts != 0 {
		view.Constraint = "Active or deferred work must settle through its existing completion owner before applying policy."
		return view
	}
	current := p.Config()
	candidate, path, raw, err := localBindingWorkflow(ctx, current, *request.AllowLocalBinding)
	if err != nil {
		view.Constraint = "The configured committed workflow revision is unavailable or has local overlays."
		return view
	}
	for _, other := range ManagerConfigFromGlobal(cfg).Projects {
		if other.ID == current.ID {
			continue
		}
		otherPath := workflowconfig.LocalDefinitionPath(other.Workflow)
		if source, err := newWorkflowGitRefSource(other); err == nil {
			otherPath = source.localConfigPath()
		}
		if filepath.Clean(otherPath) == filepath.Clean(path) {
			view.Constraint = "The configuration operation is unsupported."
			return view
		}
	}
	candidate.Config = WithMappedNativeTracker(candidate.Config, p.policyScheduling, p.ID())
	descriptor, err := ResolvePolicy(current, candidate)
	checker, ok := p.policyScheduling.(policyChecker)
	if err != nil || descriptor.ID != request.PolicyID || descriptor.SourceRevision != request.SourceRevision || !ok || checker.CheckProjectPolicy(ctx, current.ID, candidate.Config.Tracker.Repository, descriptor) != nil {
		view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
		return view
	}
	if latest := o.observe(ctx, cfg, revision, current.ID); latest.Constraint != "" || latest.ConfigRevision != request.ExpectedConfigRevision || ctx.Err() != nil {
		view.Constraint = "The selected configuration revision changed; read it before retrying."
		return view
	}
	globalRaw, err := os.ReadFile(o.selected.Path)
	if err != nil || policy.Digest(globalRaw) != revision {
		view.Constraint = "The selected configuration revision changed; read it before retrying."
		return view
	}
	if err := pauseSettledConfiguration(ctx, p, view); err != nil {
		view.Constraint = "Active or deferred work must settle through its existing completion owner before applying policy."
		return view
	}
	p.configMu.Lock()
	defer p.configMu.Unlock()
	if latest := o.observe(ctx, cfg, revision, current.ID); latest.Constraint != "" || latest.ConfigRevision != request.ExpectedConfigRevision || latest.EffectivePolicy == nil || latest.EffectivePolicy.ID != request.ExpectedPolicyID || ctx.Err() != nil {
		view.Constraint = "The selected configuration revision changed; read it before retrying."
		return view
	}
	file, err := os.CreateTemp("", "detent-project-config-*")
	if err != nil {
		view.Constraint = "The selected local configuration could not be validated or written."
		return view
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Chmod(temporary, 0600) != nil || os.Rename(temporary, path) != nil {
		view.Constraint = "The selected local configuration could not be validated or written."
		return view
	}
	if err := p.applyWorkflowUpdate(ctx, configwatcher.Update{Workflow: candidate}, true); err != nil {
		view.Saved = true
		view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
		return view
	}
	view = o.observe(ctx, cfg, revision, current.ID)
	view.Saved = true
	view.Applied = view.Constraint == "" && view.EffectivePolicy != nil && view.EffectivePolicy.ID == request.PolicyID
	return view
}
