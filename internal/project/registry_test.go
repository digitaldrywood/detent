package project_test

import (
	"testing"

	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/project"
)

func TestRegistryStoresAndListsProjectsByID(t *testing.T) {
	t.Parallel()

	registry := project.NewRegistry()
	second := newTestProject(t, "beta")
	first := newTestProject(t, "alpha")

	if err := registry.Set(second); err != nil {
		t.Fatalf("Set(second) error = %v", err)
	}
	if err := registry.Set(first); err != nil {
		t.Fatalf("Set(first) error = %v", err)
	}

	got, ok := registry.Get(project.ProjectID("alpha"))
	if !ok {
		t.Fatal("Get(alpha) ok = false, want true")
	}
	if got != first {
		t.Fatalf("Get(alpha) = %p, want %p", got, first)
	}

	projects := registry.List()
	if len(projects) != 2 {
		t.Fatalf("List() len = %d, want 2", len(projects))
	}
	if projects[0] != first || projects[1] != second {
		t.Fatalf("List() = [%s %s], want [alpha beta]", projects[0].ID(), projects[1].ID())
	}
	if registry.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", registry.Len())
	}
}

func TestRegistryDeletesProjects(t *testing.T) {
	t.Parallel()

	registry := project.NewRegistry()
	existing := newTestProject(t, "alpha")
	if err := registry.Set(existing); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if !registry.Delete(existing.ID()) {
		t.Fatal("Delete(alpha) = false, want true")
	}
	if registry.Delete(existing.ID()) {
		t.Fatal("Delete(alpha) after removal = true, want false")
	}
	if _, ok := registry.Get(existing.ID()); ok {
		t.Fatal("Get(alpha) ok = true after Delete, want false")
	}
}

func TestRegistryRejectsInvalidProject(t *testing.T) {
	t.Parallel()

	registry := project.NewRegistry()

	if err := registry.Set(nil); err != project.ErrMissingProject {
		t.Fatalf("Set(nil) error = %v, want %v", err, project.ErrMissingProject)
	}

	invalid := &project.Project{}
	if err := registry.Set(invalid); err != project.ErrMissingProjectID {
		t.Fatalf("Set(invalid) error = %v, want %v", err, project.ErrMissingProjectID)
	}
}

func newTestProject(t *testing.T, id string) *project.Project {
	t.Helper()

	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     id,
			Weight: 1,
		},
	}, project.Dependencies{})
	if err != nil {
		t.Fatalf("New(%q) error = %v", id, err)
	}
	return got
}
