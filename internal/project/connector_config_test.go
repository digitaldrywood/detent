package project

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
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
		{name: "native without repository", kind: workflowconfig.TrackerHubNative, want: source},
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

func TestBuildNativeScheduleOwnership(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		enabled    bool
		repository string
		wantError  bool
	}{
		{name: "disabled"},
		{name: "implicit tracker repository", enabled: true, wantError: true},
		{name: "explicit coordination repository", enabled: true, repository: "example/coordination"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerHubNative
			cfg.Tracker.Repository = "example/project"
			cfg.ScheduleOwnership.Enabled = tt.enabled
			cfg.ScheduleOwnership.Key = "example/production"
			cfg.ScheduleOwnership.Repository = tt.repository
			deps := Dependencies{}
			if tt.repository != "" {
				deps.ScheduleStore = &constructorOnlyScheduleStore{t: t}
			}
			manager, _, ownership, err := buildScheduleOwnership(cfg, deps, slog.Default(), nil)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "explicit schedule_ownership.repository") || manager != nil {
					t.Fatalf("buildScheduleOwnership() = (%v, %v), want explicit repository error", manager, err)
				}
				return
			}
			if err != nil || (manager != nil) != tt.enabled {
				t.Fatalf("buildScheduleOwnership() = (%v, %v), want manager enabled = %v", manager, err, tt.enabled)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if ownership != cfg.ScheduleOwnership {
				t.Fatalf("startup ownership=%+v reload ownership=%+v", ownership, cfg.ScheduleOwnership)
			}
		})
	}
}

type constructorOnlyScheduleStore struct{ t *testing.T }

func (s *constructorOnlyScheduleStore) Get(context.Context, string) (coordination.Record, bool, error) {
	s.t.Fatal("schedule ownership construction must not read the store")
	return coordination.Record{}, false, nil
}

func (s *constructorOnlyScheduleStore) CompareAndSwap(context.Context, string, string, []byte) (coordination.Record, bool, error) {
	s.t.Fatal("schedule ownership construction must not write the store")
	return coordination.Record{}, false, nil
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

func TestProjectOrchestratorConfigIncludesHostPressure(t *testing.T) {
	t.Parallel()

	project := globalconfig.Project{
		ID:           "detent",
		GlobalMemory: globalconfig.Memory{PressureSomeAvg60Threshold: 10, PollIntervalMS: 1000},
		GlobalIO:     globalconfig.IO{PressureFullAvg10Threshold: 5, DegradedMaxConcurrentAgents: 1, PollIntervalMS: 500},
		GlobalCPU:    globalconfig.CPU{PressureSomeAvg10Threshold: 80, DegradedMaxConcurrentAgents: 2, PollIntervalMS: 750},
	}
	got := projectOrchestratorConfig(project, workflowconfig.Default())

	if got.MemoryPressureSomeAvg60Max != 10 || got.MemoryPressurePollInterval != time.Second {
		t.Fatalf("memory pressure config = %.2f %s", got.MemoryPressureSomeAvg60Max, got.MemoryPressurePollInterval)
	}
	if got.IOPressureFullAvg10Max != 5 || got.IOPressureDegradedMaxAgents != 1 || got.IOPressurePollInterval != 500*time.Millisecond {
		t.Fatalf("IO pressure config = %.2f %d %s", got.IOPressureFullAvg10Max, got.IOPressureDegradedMaxAgents, got.IOPressurePollInterval)
	}
	if got.CPUPressureSomeAvg10Max != 80 || got.CPUPressureDegradedMaxAgents != 2 || got.CPUPressurePollInterval != 750*time.Millisecond {
		t.Fatalf("CPU pressure config = %.2f %d %s", got.CPUPressureSomeAvg10Max, got.CPUPressureDegradedMaxAgents, got.CPUPressurePollInterval)
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

type mappedSchedulingSource struct {
	testSchedulingSource
	projects map[string]bool
}

func (s *mappedSchedulingSource) ConnectorForProject(project string) (connector.Connector, bool) {
	return nil, s.projects[project]
}

func TestWithMappedNativeTrackerUsesTheHubForMappedProjects(t *testing.T) {
	t.Parallel()

	mapped := &mappedSchedulingSource{projects: map[string]bool{"site": true}}
	tests := []struct {
		name       string
		scheduling orchestrator.SchedulingSource
		project    ID
		kind       string
		want       string
	}{
		{name: "mapped github project", scheduling: mapped, project: "site", kind: workflowconfig.TrackerGitHub, want: workflowconfig.TrackerHubNative},
		{name: "mapped github_local project", scheduling: mapped, project: "site", kind: workflowconfig.TrackerGitHubLocal, want: workflowconfig.TrackerHubNative},
		{name: "mapped native project", scheduling: mapped, project: "site", kind: workflowconfig.TrackerHubNative, want: workflowconfig.TrackerHubNative},
		{name: "mapped project with an explicit local tracker", scheduling: mapped, project: "site", kind: workflowconfig.TrackerLocalSQLite, want: workflowconfig.TrackerLocalSQLite},
		{name: "unmapped project", scheduling: mapped, project: "other", kind: workflowconfig.TrackerGitHub, want: workflowconfig.TrackerGitHub},
		{name: "no hub client", scheduling: nil, project: "site", kind: workflowconfig.TrackerGitHub, want: workflowconfig.TrackerGitHub},
		{name: "scheduling without native projects", scheduling: &testSchedulingSource{}, project: "site", kind: workflowconfig.TrackerMemory, want: workflowconfig.TrackerMemory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workflow := workflowconfig.Config{}
			workflow.Tracker.Kind = test.kind
			workflow.Tracker.ActiveStates = []string{"Todo", "Repair"}
			workflow.Tracker.ObservedStates = []string{"Backlog", "Plan Review", "Customer QA"}
			workflow.Tracker.TerminalStates = []string{"Done", "Cancelled"}
			got := WithMappedNativeTracker(workflow, test.scheduling, test.project)
			if got.Tracker.Kind != test.want {
				t.Fatalf("tracker kind = %q, want %q", got.Tracker.Kind, test.want)
			}
			if !reflect.DeepEqual(got.Tracker.ActiveStates, workflow.Tracker.ActiveStates) || !reflect.DeepEqual(got.Tracker.ObservedStates, workflow.Tracker.ObservedStates) || !reflect.DeepEqual(got.Tracker.TerminalStates, workflow.Tracker.TerminalStates) {
				t.Fatalf("native mapping discarded repository states: %#v", got.Tracker)
			}
		})
	}
}
