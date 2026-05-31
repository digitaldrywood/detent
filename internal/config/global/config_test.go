package global

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultPath(t *testing.T) {
	t.Setenv("SYMPHONY_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(home, ".symphony", "global.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathHonorsSymphonyHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYMPHONY_HOME", home)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(home, "global.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Setenv("SYMPHONY_HOME", "")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	if cfg.APIVersion != APIVersion {
		t.Fatalf("APIVersion = %q, want %q", cfg.APIVersion, APIVersion)
	}
	if cfg.Kind != Kind {
		t.Fatalf("Kind = %q, want %q", cfg.Kind, Kind)
	}
	if cfg.Global.MaxConcurrentAgents != 8 {
		t.Fatalf("Global.MaxConcurrentAgents = %d, want 8", cfg.Global.MaxConcurrentAgents)
	}
	if cfg.Global.Scheduling != SchedulingWeighted {
		t.Fatalf("Global.Scheduling = %q, want %q", cfg.Global.Scheduling, SchedulingWeighted)
	}
	if got := cfg.Global.FairShare["half_life"]; got != "1h" {
		t.Fatalf("Global.FairShare[half_life] = %v, want 1h", got)
	}
	if got := cfg.Global.Startup["jitter_seconds"]; got != 10 {
		t.Fatalf("Global.Startup[jitter_seconds] = %v, want 10", got)
	}
	if got := cfg.Global.Startup["max_spawn_per_second"]; got != 2 {
		t.Fatalf("Global.Startup[max_spawn_per_second] = %v, want 2", got)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
}

func TestReadOrDefaultUsesDefaultForMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "global.yaml")

	cfg, err := ReadOrDefault(path)
	if err != nil {
		t.Fatalf("ReadOrDefault() error = %v", err)
	}

	if cfg.Path != path {
		t.Fatalf("Path = %q, want %q", cfg.Path, path)
	}
	if cfg.APIVersion != APIVersion {
		t.Fatalf("APIVersion = %q, want %q", cfg.APIVersion, APIVersion)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
}

func TestReadValidConfig(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")

	writeFile(t, configPath, validYAML(paths, nil))

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if cfg.Path != configPath {
		t.Fatalf("Path = %q, want %q", cfg.Path, configPath)
	}
	if cfg.APIVersion != APIVersion {
		t.Fatalf("APIVersion = %q, want %q", cfg.APIVersion, APIVersion)
	}
	if cfg.Kind != Kind {
		t.Fatalf("Kind = %q, want %q", cfg.Kind, Kind)
	}
	if cfg.Global.MaxConcurrentAgents != 8 {
		t.Fatalf("Global.MaxConcurrentAgents = %d, want 8", cfg.Global.MaxConcurrentAgents)
	}
	if cfg.Global.Scheduling != SchedulingWeighted {
		t.Fatalf("Global.Scheduling = %q, want %q", cfg.Global.Scheduling, SchedulingWeighted)
	}
	if got := cfg.Global.FairShare["ratio"]; got != 1.5 {
		t.Fatalf("Global.FairShare[ratio] = %v, want 1.5", got)
	}
	if got := cfg.Global.Startup["max_spawn_per_second"]; got != 2 {
		t.Fatalf("Global.Startup[max_spawn_per_second] = %v, want 2", got)
	}

	tags, ok := cfg.Global.Startup["tags"].([]any)
	if !ok {
		t.Fatalf("Global.Startup[tags] = %#v, want []any", cfg.Global.Startup["tags"])
	}
	if len(tags) != 2 || tags[0] != "fast" || tags[1] != "slow" {
		t.Fatalf("Global.Startup[tags] = %#v, want [fast slow]", tags)
	}

	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects length = %d, want 1", len(cfg.Projects))
	}
	project := cfg.Projects[0]
	if project.ID != "symphony" {
		t.Fatalf("Project.ID = %q, want symphony", project.ID)
	}
	if project.Workflow != paths.workflowPath {
		t.Fatalf("Project.Workflow = %q, want %q", project.Workflow, paths.workflowPath)
	}
	if project.Workdir != paths.workdirPath {
		t.Fatalf("Project.Workdir = %q, want %q", project.Workdir, paths.workdirPath)
	}
	if project.Weight != 5 {
		t.Fatalf("Project.Weight = %d, want 5", project.Weight)
	}
	if project.Priority != 50 {
		t.Fatalf("Project.Priority = %d, want 50", project.Priority)
	}
	if project.Paused {
		t.Fatal("Project.Paused = true, want false")
	}
	if project.CredentialRef != "github-default" {
		t.Fatalf("Project.CredentialRef = %q, want github-default", project.CredentialRef)
	}
}

func TestReadPreservesGlobalDefaultsForOptionalSections(t *testing.T) {
	paths := createProjectFiles(t)

	tests := []struct {
		name          string
		global        string
		wantFairShare map[string]any
		wantStartup   map[string]any
	}{
		{
			name: "omitted sections",
			global: `  max_concurrent_agents: 8
  scheduling: weighted
`,
			wantFairShare: map[string]any{
				"half_life": "1h",
			},
			wantStartup: map[string]any{
				"jitter_seconds":       10,
				"max_spawn_per_second": 2,
			},
		},
		{
			name: "partial sections",
			global: `  max_concurrent_agents: 8
  scheduling: weighted
  fair_share: {ratio: 1.5}
  startup:
    max_spawn_per_second: 4
`,
			wantFairShare: map[string]any{
				"half_life": "1h",
				"ratio":     1.5,
			},
			wantStartup: map[string]any{
				"jitter_seconds":       10,
				"max_spawn_per_second": 4,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(paths.root, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeFile(t, configPath, minimalYAML(paths, tt.global))

			cfg, err := Read(configPath, WithHome(paths.home))
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}

			assertMap(t, "Global.FairShare", cfg.Global.FairShare, tt.wantFairShare)
			assertMap(t, "Global.Startup", cfg.Global.Startup, tt.wantStartup)
		})
	}
}

func TestReadAcceptsSchedulingModes(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")

	for _, mode := range []string{SchedulingWeighted, SchedulingStrict, SchedulingRoundRobin, SchedulingFairShare} {
		t.Run(mode, func(t *testing.T) {
			writeFile(t, configPath, validYAML(paths, map[string]string{"scheduling": mode}))

			cfg, err := Read(configPath, WithHome(paths.home))
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if cfg.Global.Scheduling != mode {
				t.Fatalf("Global.Scheduling = %q, want %q", cfg.Global.Scheduling, mode)
			}
		})
	}
}

func TestReadReportsInvalidConfig(t *testing.T) {
	paths := createProjectFiles(t)

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "malformed yaml",
			raw:  "apiVersion: [\n",
			want: []string{"parse global config"},
		},
		{
			name: "root document shape",
			raw:  "- nope\n",
			want: []string{"root: must be a mapping"},
		},
		{
			name: "missing required fields",
			raw:  "{}\n",
			want: []string{
				"apiVersion: is required",
				"kind: is required",
				"global: is required",
				"projects: is required",
			},
		},
		{
			name: "invalid shapes",
			raw: `apiVersion: symphony/v2
kind: SomethingElse
global:
  max_concurrent_agents: 0
  scheduling: random
  fair_share: []
  startup:
    jitter_seconds: -1
    max_spawn_per_second: 0
projects:
  - id: 123
    workflow: 456
    workdir: 789
    weight: 0
    priority: high
    paused: maybe
    credential_ref: []
`,
			want: []string{
				"apiVersion: must equal symphony/v1",
				"kind: must equal GlobalConfig",
				"global.max_concurrent_agents: must be a positive integer",
				"global.scheduling: must be one of weighted, strict, round_robin, fair_share",
				"global.fair_share: must be a mapping",
				"global.startup.jitter_seconds: must be an integer greater than or equal to 0",
				"global.startup.max_spawn_per_second: must be a positive integer",
				"projects[0].id: must be a string",
				"projects[0].workflow: must be a string",
				"projects[0].workdir: must be a string",
				"projects[0].weight: must be a positive integer",
				"projects[0].priority: must be an integer",
				"projects[0].paused: must be a boolean",
				"projects[0].credential_ref: must be a string",
			},
		},
		{
			name: "missing project fields",
			raw: `apiVersion: symphony/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - {}
`,
			want: []string{
				"projects[0].id: is required",
				"projects[0].workflow: is required",
				"projects[0].workdir: is required",
				"projects[0].weight: is required",
				"projects[0].priority: is required",
			},
		},
		{
			name: "missing paths and duplicate project ids",
			raw: `apiVersion: symphony/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: " symphony "
    workflow: ~/missing/WORKFLOW.md
    workdir: ~/missing
    weight: 5
    priority: 50
  - id: symphony
    workflow: ~/missing/WORKFLOW.md
    workdir: ~/missing
    weight: 10
    priority: 0
`,
			want: []string{
				"projects[0].workflow: path does not exist",
				"projects[0].workdir: path does not exist",
				"projects.id: duplicate id symphony",
			},
		},
		{
			name: "containers must have expected shapes",
			raw: `apiVersion: symphony/v1
kind: GlobalConfig
global: []
projects: {}
`,
			want: []string{
				"global: must be a mapping",
				"projects: must be a list",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(paths.root, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeFile(t, configPath, tt.raw)

			_, err := Read(configPath, WithHome(paths.home))
			if err == nil {
				t.Fatal("Read() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Read() error = %q, want substring %q", err, want)
				}
			}
		})
	}
}

type projectPaths struct {
	root         string
	home         string
	workflow     string
	workdir      string
	workflowPath string
	workdirPath  string
}

func createProjectFiles(t *testing.T) projectPaths {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	workdir := filepath.Join(home, "projects", "digitaldrywood", "symphony-orchestration")
	workflow := filepath.Join(workdir, "WORKFLOW.md")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, workflow, "# workflow\n")

	return projectPaths{
		root:         root,
		home:         home,
		workflow:     "~/projects/digitaldrywood/symphony-orchestration/WORKFLOW.md",
		workdir:      "~/projects/digitaldrywood/symphony-orchestration",
		workflowPath: workflow,
		workdirPath:  workdir,
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func assertMap(t *testing.T, name string, got map[string]any, want map[string]any) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
}

func minimalYAML(paths projectPaths, global string) string {
	return `apiVersion: symphony/v1
kind: GlobalConfig
global:
` + global + `projects:
  - id: symphony
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 5
    priority: 50
`
}

func validYAML(paths projectPaths, overrides map[string]string) string {
	scheduling := SchedulingWeighted
	if overrides != nil && overrides["scheduling"] != "" {
		scheduling = overrides["scheduling"]
	}

	return `apiVersion: symphony/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: ` + scheduling + `
  fair_share: {half_life: 1h, ratio: 1.5}
  startup:
    jitter_seconds: 10
    limits: {burst: 2}
    max_spawn_per_second: 2
    tags: [fast, slow]
projects:
  - id: " symphony "
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 5
    priority: 50
    credential_ref: " github-default "
`
}
