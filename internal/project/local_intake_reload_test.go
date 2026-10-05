package project_test

import (
	"context"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestManagerReloadsLocalIntakeWithoutRestart(t *testing.T) {
	global := globalconfig.Config{Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}, {ID: "beta", Weight: 1, LocalIntakeEnabled: new(true)}}}
	created := 0
	manager, err := project.NewManager(project.ManagerConfigFromGlobal(global), project.ManagerDependencies{ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
		created++
		return project.New(project.Config{Project: cfg, Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")}}, project.Dependencies{Runner: blockingRunner{}})
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range manager.Registry().List() {
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	alpha, _ := manager.Registry().Get("alpha")
	beta, _ := manager.Registry().Get("beta")
	for _, step := range []struct {
		name                   string
		enabled, invalid, want bool
	}{
		{name: "disable", want: false},
		{name: "invalid keeps intake off", enabled: true, invalid: true, want: false},
		{name: "resume", enabled: true, want: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			next := global
			next.Global.LocalIntakeEnabled = new(step.enabled)
			if step.invalid {
				next.Global.Agents.Routes = []workflowconfig.AgentRoute{{Name: "broken", Backend: "missing"}}
			}
			_, err := manager.Reconcile(t.Context(), project.ManagerConfigFromGlobal(next))
			if (err != nil) != step.invalid {
				t.Fatalf("Reconcile error=%v", err)
			}
			after, _ := manager.Registry().Get("alpha")
			other, _ := manager.Registry().Get("beta")
			if after != alpha || other != beta || created != 2 {
				t.Fatal("intake reload restarted a project")
			}
			if alpha.Orchestrator().LocalIntakeEnabled() != step.want || !beta.Orchestrator().LocalIntakeEnabled() {
				t.Fatal("effective local intake or unrelated project override changed")
			}
			if alpha.Paused() || !alpha.Running() {
				t.Fatal("intake policy paused the project")
			}
		})
	}
}
