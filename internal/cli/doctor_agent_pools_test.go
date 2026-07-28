package cli

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workload"
)

func TestDoctorCapacityConstraintBindingRecommendations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cfg        globalconfig.Config
		projects   []doctorWorkloadProject
		waits      []store.CapacityConstraintWait
		contention []store.CrossClassPoolContention
		wantStatus doctorStatus
		want       []string
		notWant    []string
		wantHint   string
	}{
		{
			name: "mixed class pool contention preserves split",
			cfg: doctorAgentPoolsTestConfig([]globalconfig.Project{
				{ID: "detent"},
				{ID: "video"},
			}),
			projects: []doctorWorkloadProject{
				doctorCapacityTestProject("detent", "default", workload.ClassLocalHeavy, 5),
				doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5),
			},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintPool, WaitCount: 12},
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintProject, WaitCount: 2},
			},
			contention: []store.CrossClassPoolContention{
				{Pool: "default", WaitingClass: "cloud-only", HoldingClass: "local-heavy", WaitCount: 12},
			},
			wantStatus: doctorWarn,
			want: []string{
				"video (cloud-only) blocked 14x in 7d",
				"pool waits",
				"<- binding",
				`2 workload classes share pool "default"`,
				"suggested:",
			},
			wantHint: "detent fix agent-pools",
		},
		{
			name: "single class pool contention raises capacity",
			cfg: func() globalconfig.Config {
				cfg := doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video", Pool: "cloud"}})
				cfg.Global.AgentPools = []globalconfig.AgentPool{{Name: "cloud", MaxConcurrentAgents: 5}}
				return cfg
			}(),
			projects: []doctorWorkloadProject{
				doctorCapacityTestProject("video", "cloud", workload.ClassCloudOnly, 5),
			},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "cloud", Reason: store.CapacityConstraintPool, WaitCount: 9},
			},
			wantStatus: doctorWarn,
			want: []string{
				`raise global.agent_pools["cloud"].max_concurrent_agents`,
				"one workload class",
			},
			notWant: []string{"split pool", "suggested:"},
		},
		{
			name:     "project cap",
			cfg:      doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video"}}),
			projects: []doctorWorkloadProject{doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintProject, WaitCount: 8},
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintPool, WaitCount: 1},
			},
			wantStatus: doctorWarn,
			want:       []string{`raise project "video" agent.max_concurrent_agents`, "pool cap is not throttling"},
		},
		{
			name:     "lane cap names lane",
			cfg:      doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video"}}),
			projects: []doctorWorkloadProject{doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Lane: "In Progress", Reason: store.CapacityConstraintLane, WaitCount: 11},
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintProject, WaitCount: 2},
			},
			wantStatus: doctorWarn,
			want: []string{
				"lane_capacity_full (In Progress)",
				`raise project "video" agent.max_concurrent_agents_by_state.In Progress`,
			},
		},
		{
			name:     "worker host cap",
			cfg:      doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video"}}),
			projects: []doctorWorkloadProject{doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintWorkerHost, WaitCount: 7},
			},
			wantStatus: doctorWarn,
			want:       []string{`raise project "video" worker.max_concurrent_agents_per_host`},
		},
		{
			name:     "rate window recommends no config change",
			cfg:      doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video"}}),
			projects: []doctorWorkloadProject{doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)},
			waits: []store.CapacityConstraintWait{
				{ProjectID: "video", WorkloadClass: "cloud-only", Pool: "default", Reason: store.CapacityConstraintRateWindow, WaitCount: 6},
			},
			wantStatus: doctorWarn,
			want:       []string{"provider rate-window backpressure is binding", "no config change is recommended"},
			notWant:    []string{"raise ", "split "},
		},
		{
			name:       "nothing binding stays silent",
			cfg:        doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video"}}),
			projects:   []doctorWorkloadProject{doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)},
			wantStatus: doctorOK,
			want:       []string{"no binding capacity constraint detected"},
		},
		{
			name: "elastic pools",
			cfg: func() globalconfig.Config {
				cfg := doctorAgentPoolsTestConfig([]globalconfig.Project{
					{ID: "detent", Workflow: "detent", Pool: "code", Weight: 1},
				})
				cfg.Global.AgentPools = []globalconfig.AgentPool{
					{Name: "code", MaxConcurrentAgents: 5, BurstTo: 8},
				}
				return cfg
			}(),
			wantStatus:    doctorOK,
			wantSubstring: "code (guaranteed 5, burst ceiling 8)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			check := newDoctorCapacityCheck(tt.cfg, tt.projects, tt.waits, tt.contention, nil)
			if check.Status != tt.wantStatus {
				t.Fatalf("check status = %s, want %s; detail:\n%s", check.Status, tt.wantStatus, check.Detail)
			}
			for _, want := range tt.want {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("detail missing %q:\n%s", want, check.Detail)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(check.Detail, notWant) {
					t.Fatalf("detail unexpectedly contains %q:\n%s", notWant, check.Detail)
				}
			}
			if tt.wantHint != "" && !strings.Contains(check.Hint, tt.wantHint) {
				t.Fatalf("hint = %q, want containing %q", check.Hint, tt.wantHint)
			}
		})
	}
}

func TestDoctorPoolCoherenceFindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      globalconfig.Config
		projects []doctorWorkloadProject
		want     string
	}{
		{
			name: "pool cannot fill",
			cfg: globalconfig.Config{
				Global: globalconfig.Settings{
					MaxConcurrentAgents: 5,
					AgentPools:          []globalconfig.AgentPool{{Name: "cloud", MaxConcurrentAgents: 10}},
				},
			},
			projects: []doctorWorkloadProject{
				doctorCapacityTestProject("video", "cloud", workload.ClassCloudOnly, 2),
				doctorCapacityTestProject("podcast", "cloud", workload.ClassCloudOnly, 3),
			},
			want: `pool "cloud" capacity 10 exceeds its 2 member project cap total 5`,
		},
		{
			name: "project cap is dead",
			cfg: globalconfig.Config{
				Global: globalconfig.Settings{
					MaxConcurrentAgents: 5,
					AgentPools:          []globalconfig.AgentPool{{Name: "cloud", MaxConcurrentAgents: 5}},
				},
			},
			projects: []doctorWorkloadProject{
				doctorCapacityTestProject("video", "cloud", workload.ClassCloudOnly, 8),
			},
			want: `project "video" agent.max_concurrent_agents=8 exceeds pool "cloud" capacity 5`,
		},
		{
			name: "active lane binds below project",
			cfg: globalconfig.Config{
				Global: globalconfig.Settings{MaxConcurrentAgents: 5},
			},
			projects: []doctorWorkloadProject{
				func() doctorWorkloadProject {
					project := doctorCapacityTestProject("video", "default", workload.ClassCloudOnly, 5)
					project.Workflow.Tracker.ActiveStates = []string{"In Progress", "Merging"}
					project.Workflow.Agent.MaxConcurrentAgentsByState = map[string]int{
						"In Progress": 2,
						"Merging":     1,
					}
					return project
				}(),
			},
			want: `project "video" active lane "In Progress" cap 2 binds below agent.max_concurrent_agents=5`,
		},
		{
			name: "serialized merging lane is coherent",
			cfg: globalconfig.Config{
				Global: globalconfig.Settings{MaxConcurrentAgents: 5},
			},
			projects: []doctorWorkloadProject{
				func() doctorWorkloadProject {
					project := doctorCapacityTestProject("detent", "default", workload.ClassLocalHeavy, 5)
					project.Workflow.Tracker.ActiveStates = []string{"Merging"}
					project.Workflow.Agent.MaxConcurrentAgentsByState = map[string]int{"Merging": 1}
					return project
				}(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			findings := doctorPoolCoherenceFindings(tt.cfg, tt.projects)
			detail := strings.Join(findings, "\n")
			if tt.want == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %#v, want none", findings)
				}
				return
			}
			if !strings.Contains(detail, tt.want) {
				t.Fatalf("findings missing %q:\n%s", tt.want, detail)
			}
		})
	}
}

func TestDoctorCapacityFindingsIgnorePreviousPoolAssignments(t *testing.T) {
	t.Parallel()

	project := doctorCapacityTestProject("video", "cloud", workload.ClassCloudOnly, 5)
	findings := newDoctorCapacityFindings(
		[]doctorWorkloadProject{project},
		[]store.CapacityConstraintWait{
			{
				ProjectID: "video",
				Pool:      "default",
				Reason:    store.CapacityConstraintPool,
				WaitCount: 100,
			},
			{
				ProjectID: "video",
				Pool:      "cloud",
				Reason:    store.CapacityConstraintProject,
				WaitCount: 2,
			},
		},
	)
	if len(findings) != 1 ||
		findings[0].Pool != "cloud" ||
		findings[0].Total != 2 ||
		findings[0].Binding.Reason != store.CapacityConstraintProject {
		t.Fatalf("findings = %#v, want only current cloud-pool telemetry", findings)
	}
}

func TestCheckDoctorAgentPoolsReportsStaticFindingsWithoutTelemetry(t *testing.T) {
	t.Parallel()

	cfg := doctorAgentPoolsTestConfig([]globalconfig.Project{{ID: "video", Workflow: "video"}})
	deps := doctorDeps{
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: workflowconfig.Config{
				Agent: workflowconfig.Agent{MaxConcurrentAgents: 2},
				Gate:  gate.Config{Kind: gate.KindArtifact},
			}}, nil
		},
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) {
			return nil, errDoctorTelemetryStoreUnavailable
		},
	}
	check := checkDoctorAgentPools(
		t.Context(),
		globalconfig.PathResolution{Path: "/config/global.yaml"},
		cfg,
		deps,
	)
	if check.Status != doctorWarn || !strings.Contains(check.Detail, "pool can never fill") {
		t.Fatalf("check = %#v, want static warning without telemetry", check)
	}
}

func TestDoctorWorkflowCacheReusesResolvedWorkflow(t *testing.T) {
	t.Parallel()

	calls := 0
	deps := doctorDeps{
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			calls++
			return workflowconfig.Workflow{}, nil
		},
	}.withDefaults()
	project := globalconfig.Project{ID: "detent", Workflow: "/repo/WORKFLOW.md"}
	for range 2 {
		if _, err := loadDoctorProjectWorkflow(t.Context(), project, deps); err != nil {
			t.Fatalf("loadDoctorProjectWorkflow() error = %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("loadWorkflow calls = %d, want 1", calls)
	}
}

func TestDoctorAgentPoolRecommendationSuggestedYAMLQuotesProjectIDs(t *testing.T) {
	t.Parallel()

	recommendation := doctorAgentPoolRecommendation{
		CurrentCap: 5,
		CloudCap:   10,
		LocalProjects: []doctorWorkloadProject{
			{ID: "123"},
			{ID: "null"},
		},
		CloudProjects: []doctorWorkloadProject{
			{ID: "team: api"},
		},
	}
	var parsed struct {
		Projects []struct {
			ID   string `yaml:"id"`
			Pool string `yaml:"pool"`
		} `yaml:"projects"`
	}
	if err := yaml.Unmarshal([]byte(recommendation.SuggestedYAML()), &parsed); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v\n%s", err, recommendation.SuggestedYAML())
	}
	want := []struct {
		ID   string
		Pool string
	}{
		{ID: "123", Pool: "code"},
		{ID: "null", Pool: "code"},
		{ID: "team: api", Pool: "cloud"},
	}
	if len(parsed.Projects) != len(want) {
		t.Fatalf("projects = %#v, want %#v", parsed.Projects, want)
	}
	for index := range want {
		if parsed.Projects[index].ID != want[index].ID || parsed.Projects[index].Pool != want[index].Pool {
			t.Fatalf("projects[%d] = %#v, want %#v", index, parsed.Projects[index], want[index])
		}
	}
}

func doctorAgentPoolsTestConfig(projects []globalconfig.Project) globalconfig.Config {
	return globalconfig.Config{
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 5,
			Scheduling:          globalconfig.SchedulingWeighted,
		},
		Projects: projects,
	}
}

func doctorCapacityTestProject(
	id string,
	pool string,
	class workload.Class,
	maxConcurrentAgents int,
) doctorWorkloadProject {
	return doctorWorkloadProject{
		ID:    id,
		Pool:  pool,
		Class: class,
		Workflow: workflowconfig.Config{
			Agent: workflowconfig.Agent{MaxConcurrentAgents: maxConcurrentAgents},
		},
	}
}
