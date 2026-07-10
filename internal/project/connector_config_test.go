package project

import (
	"path/filepath"
	"reflect"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/intake"
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

func TestWorkflowConfigWithProjectIntakeOverride(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Default()
	workflow.Intake = intake.Config{Sources: []intake.Source{{Name: "workflow", Kind: intake.KindWebhook, Secret: "workflow-secret"}}}
	projectIntake := intake.Config{Sources: []intake.Source{{Name: "global", Kind: intake.KindSlack, Secret: "global-secret"}}}
	got := workflowConfigWithProjectIdentity(globalconfig.Project{
		Intake:           projectIntake,
		IntakeConfigured: true,
	}, workflow)

	if len(got.Intake.Sources) != 1 || got.Intake.Sources[0].Name != "global" {
		t.Fatalf("Intake = %#v, want global project override", got.Intake)
	}
}

func TestWorkflowConfigWithProjectKnowledgeMergesGlobalProjectAndWorkflowSources(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Agent.Knowledge = workflowconfig.Knowledge{
		Enabled:  true,
		MaxBytes: 4096,
		Sources: []workflowconfig.KnowledgeSource{{
			Name: "Workflow",
			Path: "docs/workflow.md",
		}},
	}

	got := workflowConfigWithProjectIdentity(globalconfig.Project{
		Workdir: workdir,
		GlobalKnowledge: workflowconfig.Knowledge{
			Enabled:  true,
			MaxBytes: 1024,
			Sources: []workflowconfig.KnowledgeSource{{
				Name: "Global",
				Path: "/shared/global.md",
			}},
		},
		Knowledge: workflowconfig.Knowledge{
			Enabled:  true,
			MaxBytes: 2048,
			Sources: []workflowconfig.KnowledgeSource{{
				Name: "Project",
				Path: "/shared/project.md",
			}},
		},
	}, cfg)

	if got.Agent.Knowledge.MaxBytes != 4096 {
		t.Fatalf("Knowledge.MaxBytes = %d, want 4096", got.Agent.Knowledge.MaxBytes)
	}
	want := []workflowconfig.KnowledgeSource{
		{Name: "Global", Path: "/shared/global.md"},
		{Name: "Project", Path: "/shared/project.md"},
		{Name: "Workflow", Path: filepath.Join(workdir, "docs", "workflow.md")},
	}
	if !reflect.DeepEqual(got.Agent.Knowledge.Sources, want) {
		t.Fatalf("Knowledge.Sources = %#v, want %#v", got.Agent.Knowledge.Sources, want)
	}
}

func TestWorkflowConfigWithProjectKnowledgeAllowsWorkflowOptOut(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Agent.Knowledge = workflowconfig.Knowledge{Enabled: false}

	got := workflowConfigWithProjectIdentity(globalconfig.Project{
		GlobalKnowledge: workflowconfig.Knowledge{
			Enabled: true,
			Sources: []workflowconfig.KnowledgeSource{{
				Name: "Global",
				Path: "/shared/global.md",
			}},
		},
	}, cfg)

	if got.Agent.Knowledge.Enabled {
		t.Fatal("Knowledge.Enabled = true, want workflow opt-out")
	}
	if len(got.Agent.Knowledge.Sources) != 0 {
		t.Fatalf("Knowledge.Sources = %#v, want none", got.Agent.Knowledge.Sources)
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
