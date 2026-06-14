package devruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestBuildCreatesIsolatedRuntimeDefaults(t *testing.T) {
	t.Parallel()

	runtime, err := Build(Config{Home: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if runtime.DBPath != defaultDBPath || runtime.DBMode != "memory" {
		t.Fatalf("DB = %q mode %q, want :memory: memory", runtime.DBPath, runtime.DBMode)
	}
	if runtime.TrackerMode != TrackerMemory {
		t.Fatalf("TrackerMode = %q, want %q", runtime.TrackerMode, TrackerMemory)
	}
	if runtime.Global.Path != runtime.ConfigPath {
		t.Fatalf("Global.Path = %q, want %q", runtime.Global.Path, runtime.ConfigPath)
	}
	for _, path := range []string{runtime.Home, runtime.ConfigPath, runtime.WorkflowPath, runtime.WorkspaceRoot, runtime.SourceRoot} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
	}
	if samePath(runtime.Home, filepath.Dir(runtime.ConfigPath)) != true {
		t.Fatalf("config path %s is not under home %s", runtime.ConfigPath, runtime.Home)
	}

	workflow, err := workflowconfig.LoadWorkflow(runtime.WorkflowPath)
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if workflow.Config.Tracker.Kind != workflowconfig.TrackerMemory {
		t.Fatalf("workflow tracker = %q, want memory", workflow.Config.Tracker.Kind)
	}
	if workflow.Config.Workspace.Root != runtime.WorkspaceRoot {
		t.Fatalf("workspace root = %q, want %q", workflow.Config.Workspace.Root, runtime.WorkspaceRoot)
	}
	if len(workflow.Config.Tracker.Issues) < 4 {
		t.Fatalf("fixture issues = %d, want default dogfood coverage", len(workflow.Config.Tracker.Issues))
	}
}

func TestBuildLoadsFixtureIssues(t *testing.T) {
	t.Parallel()

	fixture := filepath.Join(t.TempDir(), "fixture.yaml")
	raw, err := yaml.Marshal(struct {
		Issues []connector.Issue `yaml:"issues"`
	}{
		Issues: []connector.Issue{{
			ID:         "fixture-issue",
			Identifier: "digitaldrywood/detent#42",
			State:      "Human Review",
			PullRequest: &connector.PullRequest{
				Number:   42,
				State:    "OPEN",
				CIStatus: "success",
			},
		}},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	runtime, err := Build(Config{Home: t.TempDir(), FixturePath: fixture})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.FixturePath == "" {
		t.Fatal("FixturePath is blank, want resolved fixture path")
	}
	if len(runtime.Issues) != 1 || runtime.Issues[0].ID != "fixture-issue" {
		t.Fatalf("Issues = %#v, want fixture issue", runtime.Issues)
	}
}

func TestBuildRejectsUnsafeRuntimeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{
			name: "live dogfood port",
			cfg:  Config{Home: t.TempDir(), Port: liveDogfoodPort},
			want: ErrLivePort,
		},
		{
			name: "unsupported tracker",
			cfg:  Config{Home: t.TempDir(), TrackerMode: "github"},
			want: ErrUnsupportedTracker,
		},
		{
			name: "live database",
			cfg:  Config{Home: t.TempDir(), DBPath: liveDatabasePath()},
			want: ErrLiveDatabase,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Build(tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestBuildAllowsExplicitUnsafeOverrides(t *testing.T) {
	t.Parallel()

	runtime, err := Build(Config{
		Home:                t.TempDir(),
		Port:                liveDogfoodPort,
		DBPath:              liveDatabasePath(),
		AllowProductionPort: true,
		AllowLiveDatabase:   true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.Port != liveDogfoodPort {
		t.Fatalf("Port = %d, want %d", runtime.Port, liveDogfoodPort)
	}
}
