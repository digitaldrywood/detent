package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	projectpkg "github.com/digitaldrywood/symphony/internal/project"
	"github.com/digitaldrywood/symphony/internal/web"
)

func TestRegistryRefresherRequestsProjectOrchestrators(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, startRefreshProject(t, "alpha"))
	mustSetProject(t, registry, startRefreshProject(t, "beta"))

	refresher := refresherForRegistry(registry)
	if refresher == nil {
		t.Fatal("refresherForRegistry() = nil, want refresher")
	}

	response, err := refresher.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("RequestRefresh() error = %v", err)
	}
	assertRefresh(t, response)
}

func TestRegistryRefresherSkipsStoppedProjectOrchestrators(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, newRefreshProject(t, "stopped"))

	refresher := refresherForRegistry(registry)
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func TestRegistryRefresherReturnsProjectNotFoundWithoutOrchestrators(t *testing.T) {
	t.Parallel()

	refresher := refresherForRegistry(projectpkg.NewRegistry())
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func newRefreshProject(t *testing.T, id string) *projectpkg.Project {
	t.Helper()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	project, err := projectpkg.New(projectpkg.Config{
		Project: globalconfig.Project{
			ID:      id,
			Workdir: t.TempDir(),
			Weight:  1,
		},
		Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
	}, projectpkg.Dependencies{})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}
	return project
}

func startRefreshProject(t *testing.T, id string) *projectpkg.Project {
	t.Helper()

	project := newRefreshProject(t, id)
	if err := project.Start(context.Background()); err != nil {
		t.Fatalf("Project.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := project.Stop(ctx); err != nil && !errors.Is(err, projectpkg.ErrNotRunning) {
			t.Fatalf("Project.Stop() error = %v", err)
		}
	})
	return project
}

func mustSetProject(t *testing.T, registry *projectpkg.Registry, project *projectpkg.Project) {
	t.Helper()

	if err := registry.Set(project); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func assertRefresh(t *testing.T, response web.RefreshResponse) {
	t.Helper()

	if !response.Queued {
		t.Fatalf("Queued = false, want true; response = %#v", response)
	}
	if response.RequestedAt.IsZero() {
		t.Fatalf("RequestedAt is zero; response = %#v", response)
	}
	if len(response.Operations) != 2 || response.Operations[0] != "poll" || response.Operations[1] != "reconcile" {
		t.Fatalf("Operations = %#v, want poll/reconcile", response.Operations)
	}
}
