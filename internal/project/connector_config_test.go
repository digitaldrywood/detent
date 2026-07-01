package project

import (
	"path/filepath"
	"reflect"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestTrackerStateMapConvertsWorkflowMap(t *testing.T) {
	t.Parallel()

	got := trackerStateMap(workflowconfig.MapValue(map[string]any{
		"Cancelled": "Done",
		" ":         "Ignored",
		"Blocked":   12,
	}))
	want := map[string]string{"Cancelled": "Done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trackerStateMap() = %#v, want %#v", got, want)
	}

	if got := trackerStateMap(workflowconfig.StringValue("$STATE_MAP_JSON")); got != nil {
		t.Fatalf("trackerStateMap(string) = %#v, want nil", got)
	}
}

func TestWorkflowConfigWithProjectPathsResolvesArtifactWorkflowPaths(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerLocalSQLite
	cfg.Tracker.LocalSQLite.Path = ".detent/work-items.db"
	cfg.Workspace.Kind = workflowconfig.WorkspaceFilesystem
	cfg.Workspace.Root = ".detent/workspaces"
	cfg.Workspace.SourceRoot = "assets"
	cfg.Workspace.OutputRoot = ".detent/output"
	cfg.Deliverable.Kind = workflowconfig.DeliverableArtifact
	cfg.Deliverable.OutputRoot = ".detent/deliverables"

	got := workflowConfigWithProjectIdentity(globalconfig.Project{Workdir: workdir}, cfg)
	if got.Tracker.LocalSQLite.Path != filepath.Join(workdir, ".detent", "work-items.db") {
		t.Fatalf("Tracker.LocalSQLite.Path = %q", got.Tracker.LocalSQLite.Path)
	}
	if got.Workspace.Root != filepath.Join(workdir, ".detent", "workspaces") {
		t.Fatalf("Workspace.Root = %q", got.Workspace.Root)
	}
	if got.Workspace.SourceRoot != filepath.Join(workdir, "assets") {
		t.Fatalf("Workspace.SourceRoot = %q", got.Workspace.SourceRoot)
	}
	if got.Workspace.OutputRoot != filepath.Join(workdir, ".detent", "output") {
		t.Fatalf("Workspace.OutputRoot = %q", got.Workspace.OutputRoot)
	}
	if got.Deliverable.OutputRoot != filepath.Join(workdir, ".detent", "deliverables") {
		t.Fatalf("Deliverable.OutputRoot = %q", got.Deliverable.OutputRoot)
	}
}

func TestTrackerPriorityMapConvertsWorkflowMap(t *testing.T) {
	t.Parallel()

	got := trackerPriorityMap(workflowconfig.MapValue(map[string]any{
		"P0":          1,
		"No priority": nil,
		" ":           2,
		"Pbad":        "1",
	}))
	wantP0 := 1
	want := map[string]*int{
		"P0":          &wantP0,
		"No priority": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trackerPriorityMap() = %#v, want %#v", got, want)
	}

	if got := trackerPriorityMap(workflowconfig.StringValue("$PRIORITY_MAP_JSON")); got != nil {
		t.Fatalf("trackerPriorityMap(string) = %#v, want nil", got)
	}
}
