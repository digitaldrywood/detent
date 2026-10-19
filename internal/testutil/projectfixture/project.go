package projectfixture

import (
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/testutil/fakehubscheduling"
)

func New(cfg project.Config, deps project.Dependencies) (*project.Project, error) {
	if cfg.Workflow.Config.Tracker.Kind == "" {
		if cfg.Project.Workflow != "" || cfg.Project.WorkflowRef != "" {
			workflow, err := project.LoadWorkflow(cfg.Project)
			if err != nil {
				return nil, err
			}
			cfg.Workflow = workflow
		} else {
			cfg.Workflow.Config = workflowconfig.Default()
			cfg.Workflow.Config.Tracker.Kind = workflowconfig.TrackerHubNative
		}
	}
	if cfg.Workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative && deps.Scheduling == nil {
		conn := deps.Connector
		if conn == nil {
			if deps.ConnectorFactory != nil {
				var err error
				conn, err = deps.ConnectorFactory(cfg.Workflow.Config)
				if err != nil {
					return nil, err
				}
			} else {
				conn = fakehub.New(fakehub.Config{Issues: cfg.Workflow.Config.Tracker.Issues})
			}
		}
		deps.Scheduling = &fakehubscheduling.Source{Connector: conn}
	}
	if deps.Connector == nil && deps.ConnectorFactory == nil && cfg.Workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
		deps.ConnectorFactory = func(cfg workflowconfig.Config) (connector.Connector, error) {
			return fakehub.New(fakehub.Config{Issues: cfg.Tracker.Issues}), nil
		}
	}
	return project.New(cfg, deps)
}

func NewManager(cfg project.ManagerConfig, deps project.ManagerDependencies) (*project.Manager, error) {
	if deps.ProjectFactory == nil {
		deps.ProjectFactory = func(selected globalconfig.Project) (*project.Project, error) {
			workflow, err := project.LoadWorkflow(selected)
			if err != nil {
				return nil, err
			}
			return New(project.Config{Project: selected, Workflow: workflow}, project.Dependencies{})
		}
	}
	return project.NewManager(cfg, deps)
}
