package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestCheckDoctorAgentPools(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	localWorkflow := workflowconfig.Workflow{Config: workflowconfig.Config{
		Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"},
	}}
	cloudWorkflow := workflowconfig.Workflow{Config: workflowconfig.Config{
		Gate: gate.Config{Kind: gate.KindArtifact},
	}}

	tests := []struct {
		name          string
		cfg           globalconfig.Config
		workflows     map[string]workflowconfig.Workflow
		waiting       string
		holders       string
		wantStatus    doctorStatus
		wantSubstring string
	}{
		{
			name: "one workload class",
			cfg: doctorAgentPoolsTestConfig([]globalconfig.Project{
				{ID: "detent", Workflow: "detent", Weight: 1},
				{ID: "gopher-ai", Workflow: "gopher-ai", Weight: 1},
			}),
			workflows: map[string]workflowconfig.Workflow{
				"detent":    localWorkflow,
				"gopher-ai": localWorkflow,
			},
			wantStatus:    doctorOK,
			wantSubstring: "one workload class",
		},
		{
			name: "mixed classes without contention",
			cfg: doctorAgentPoolsTestConfig([]globalconfig.Project{
				{ID: "detent", Workflow: "detent", Weight: 1},
				{ID: "video", Workflow: "video", Weight: 1},
			}),
			workflows: map[string]workflowconfig.Workflow{
				"detent": localWorkflow,
				"video":  cloudWorkflow,
			},
			wantStatus:    doctorOK,
			wantSubstring: "no cross-class pool contention",
		},
		{
			name: "same class contention",
			cfg: doctorAgentPoolsTestConfig([]globalconfig.Project{
				{ID: "detent", Workflow: "detent", Weight: 1},
				{ID: "video", Workflow: "video", Weight: 1},
				{ID: "podcast", Workflow: "podcast", Weight: 1},
			}),
			workflows: map[string]workflowconfig.Workflow{
				"detent":  localWorkflow,
				"video":   cloudWorkflow,
				"podcast": cloudWorkflow,
			},
			waiting:       "video",
			holders:       `["podcast"]`,
			wantStatus:    doctorOK,
			wantSubstring: "no cross-class pool contention",
		},
		{
			name: "cross class contention",
			cfg: doctorAgentPoolsTestConfig([]globalconfig.Project{
				{ID: "detent", Workflow: "detent", Weight: 1},
				{ID: "video", Workflow: "video", Weight: 1},
			}),
			workflows: map[string]workflowconfig.Workflow{
				"detent": localWorkflow,
				"video":  cloudWorkflow,
			},
			waiting:       "video",
			holders:       `["detent"]`,
			wantStatus:    doctorWarn,
			wantSubstring: "cloud-only waited 1 time(s); local-heavy held a slot",
		},
		{
			name: "existing pools",
			cfg: func() globalconfig.Config {
				cfg := doctorAgentPoolsTestConfig([]globalconfig.Project{
					{ID: "detent", Workflow: "detent", Pool: "code", Weight: 1},
					{ID: "video", Workflow: "video", Pool: "cloud", Weight: 1},
				})
				cfg.Global.AgentPools = []globalconfig.AgentPool{
					{Name: "code", MaxConcurrentAgents: 5},
					{Name: "cloud", MaxConcurrentAgents: 10},
				}
				return cfg
			}(),
			wantStatus:    doctorOK,
			wantSubstring: "already configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			configPath := filepath.Join(dir, "global.yaml")
			dbPath := filepath.Join(dir, "detent.db")
			backend, err := store.Open(t.Context(), store.Config{Path: dbPath})
			if err != nil {
				t.Fatalf("store.Open() error = %v", err)
			}
			if tt.waiting != "" {
				if _, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
					ProjectID:              tt.waiting,
					WorkerType:             "implement",
					StartedAt:              now.Add(-time.Hour),
					WaitReason:             "global_capacity_full",
					CapacitySnapshotJSON:   `{"pool":"default","holders":` + tt.holders + `}`,
					GitHubRateSnapshotJSON: "{}",
					WorkerMetadataJSON:     "{}",
					MetricsJSON:            "{}",
				}); err != nil {
					t.Fatalf("StartWorkAttempt() error = %v", err)
				}
			}
			if err := backend.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			deps := doctorDeps{
				loadWorkflow: func(path string) (workflowconfig.Workflow, error) {
					return tt.workflows[path], nil
				},
				openSQLiteReadOnly: openDoctorSQLiteReadOnly,
				now:                func() time.Time { return now },
			}.withDefaults()
			check := checkDoctorAgentPools(
				context.Background(),
				globalconfig.PathResolution{Path: configPath},
				tt.cfg,
				deps,
			)
			if check.Status != tt.wantStatus || !strings.Contains(check.Detail, tt.wantSubstring) {
				t.Fatalf("check = %#v, want status %s containing %q", check, tt.wantStatus, tt.wantSubstring)
			}
			if tt.wantStatus == doctorWarn {
				for _, want := range []string{
					"local-heavy: detent",
					"cloud-only:  video",
					"pool capacity 1 times in 7d",
					"max_concurrent_agents: 5",
					"max_concurrent_agents: 10 # tune to your provider limits",
				} {
					if !strings.Contains(check.Detail, want) {
						t.Fatalf("detail missing %q:\n%s", want, check.Detail)
					}
				}
			}
		})
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
