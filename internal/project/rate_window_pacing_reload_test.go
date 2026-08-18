package project_test

import (
	"context"
	"reflect"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestManagerReconcileHotAppliesGlobalRateWindowPacing(t *testing.T) {
	t.Parallel()

	initialPacing := workflowconfig.DefaultRateWindowPacing()
	initial := globalconfig.Project{ID: "alpha", Weight: 1, GlobalRateWindowPacing: initialPacing}
	created := 0
	manager, err := project.NewManager(project.ManagerConfig{Projects: []globalconfig.Project{initial}}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			created++
			workflow := workflowConfig("memory")
			return project.New(project.Config{Project: cfg, Workflow: workflowconfig.Workflow{Config: workflow}}, project.Dependencies{Runner: blockingRunner{}})
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		for _, item := range manager.Registry().List() {
			if err := item.Close(); err != nil {
				t.Errorf("Close(%s) error = %v", item.ID(), err)
			}
		}
	})

	before, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("project alpha missing before reconcile")
	}
	off := workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}.Normalized()
	result, err := manager.Reconcile(context.Background(), project.ManagerConfig{Projects: []globalconfig.Project{{ID: "alpha", Weight: 1, GlobalRateWindowPacing: off}}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if want := (project.ReconcileResult{Changed: []project.ID{"alpha"}}); !reflect.DeepEqual(result, want) {
		t.Fatalf("Reconcile() = %#v, want %#v", result, want)
	}
	after, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("project alpha missing after reconcile")
	}
	if before != after || created != 1 {
		t.Fatalf("project restarted: before=%p after=%p created=%d", before, after, created)
	}
	state, err := after.Orchestrator().State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if got := state.RateWindowPacing.Mode; got != workflowconfig.RateWindowPacingOff {
		t.Fatalf("State().RateWindowPacing.Mode = %q, want off", got)
	}
}
