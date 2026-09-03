package project

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

type testSchedulingSource struct{}

func (*testSchedulingSource) HeartbeatInterval() time.Duration { return time.Second }
func (*testSchedulingSource) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return nil, nil
}
func (*testSchedulingSource) AdoptClaim(context.Context, connector.Issue, time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{}, nil
}
func (*testSchedulingSource) RenewClaim(context.Context, string, time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{}, nil
}
func (*testSchedulingSource) ReleaseClaim(context.Context, string, string) error { return nil }

func TestProjectSchedulingSourceRequiresGitHubRepository(t *testing.T) {
	t.Parallel()

	source := &testSchedulingSource{}
	tests := []struct {
		name       string
		kind       string
		repository string
		want       orchestrator.SchedulingSource
	}{
		{name: "GitHub", kind: workflowconfig.TrackerGitHub, repository: "acme/widgets", want: source},
		{name: "GitHub missing repository", kind: workflowconfig.TrackerGitHub},
		{name: "GitHub local", kind: workflowconfig.TrackerGitHubLocal, repository: "acme/widgets"},
		{name: "Linear", kind: workflowconfig.TrackerLinear},
		{name: "memory", kind: workflowconfig.TrackerMemory},
		{name: "local SQLite", kind: workflowconfig.TrackerLocalSQLite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow := workflowconfig.Default()
			workflow.Tracker.Kind = test.kind
			workflow.Tracker.Repository = test.repository
			if got := projectSchedulingSource(source, workflow); got != test.want {
				t.Fatalf("projectSchedulingSource() = %#v, want %#v", got, test.want)
			}
		})
	}
}

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

func TestWorkflowConfigWithGitHubTokenSupportsScheduleOwnership(t *testing.T) {
	t.Parallel()
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.ScheduleOwnership.Enabled = true

	got := workflowConfigWithGitHubToken(cfg, "runtime-token")
	if got.Tracker.APIKey != "runtime-token" {
		t.Fatalf("Tracker.APIKey = %q, want runtime-token", got.Tracker.APIKey)
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

func TestProjectOrchestratorConfigEnablesLessonCapture(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		lessonPath string
		maxEntries int
		wantPath   string
		wantMax    int
	}{
		{
			name:     "defaults",
			wantPath: filepath.Join(".detent", "lessons.md"),
			wantMax:  50,
		},
		{
			name:       "workflow override",
			lessonPath: filepath.Join("ops", "lessons.md"),
			maxEntries: 12,
			wantPath:   filepath.Join("ops", "lessons.md"),
			wantMax:    12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workdir := t.TempDir()
			cfg := workflowconfig.Default()
			cfg.Agent.Lessons.Enabled = false
			cfg.Agent.Lessons.Path = tt.lessonPath
			cfg.Agent.Lessons.MaxEntries = tt.maxEntries

			got := projectOrchestratorConfig(globalconfig.Project{ID: "detent", Workdir: workdir}, cfg)
			if !got.Lessons.Enabled {
				t.Fatal("Lessons.Enabled = false, want automatic capture enabled")
			}
			if got.Lessons.Path != filepath.Join(workdir, tt.wantPath) {
				t.Fatalf("Lessons.Path = %q, want %q", got.Lessons.Path, filepath.Join(workdir, tt.wantPath))
			}
			if got.Lessons.MaxEntries != tt.wantMax {
				t.Fatalf("Lessons.MaxEntries = %d, want %d", got.Lessons.MaxEntries, tt.wantMax)
			}
		})
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
