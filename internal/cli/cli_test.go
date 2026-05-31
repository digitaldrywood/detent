package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/symphony-go/internal/cli"
	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/project"
)

func TestRootCommandHelpListsAdminCommands(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{"symphony", "agent orchestrator", "init", "add-project", "pause", "unpause", "promote", "remove-project"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestRootCommandBootsFromGlobalConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	booted := make(chan globalconfig.Config, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg globalconfig.Config) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := <-booted
	if got.Path != path {
		t.Fatalf("booted config path = %q, want %q", got.Path, path)
	}
}

func TestInitWritesDefaultGlobalConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".symphony", "global.yaml")
	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "init"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	cfg, err := globalconfig.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if cfg.APIVersion != globalconfig.APIVersion || cfg.Kind != globalconfig.Kind {
		t.Fatalf("config identity = %s/%s", cfg.APIVersion, cfg.Kind)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
}

func TestInitRefusesExistingConfigWithoutForce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "init"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Execute() error = %v, want already exists", err)
	}
}

func TestAddProjectWritesConfigAndSignalsManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	signals := make(chan cli.Signal, 1)

	cmd := cli.NewRootCommand(context.Background(), cli.WithSignalFunc(func(_ context.Context, signal cli.Signal) error {
		signals <- signal
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", " symphony ",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
		"--weight", "5",
		"--priority", "50",
		"--paused",
		"--credential-ref", " github-default ",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects length = %d, want 1", len(cfg.Projects))
	}
	got := cfg.Projects[0]
	want := globalconfig.Project{
		ID:            "symphony",
		Workflow:      paths.workflowPath,
		Workdir:       paths.workdirPath,
		Weight:        5,
		Priority:      50,
		Paused:        true,
		CredentialRef: "github-default",
	}
	if got != want {
		t.Fatalf("project = %#v, want %#v", got, want)
	}

	signal := <-signals
	if signal.Operation != cli.OperationAddProject || signal.Project != want {
		t.Fatalf("signal = %#v, want add project %#v", signal, want)
	}
}

func TestProjectAdminCommandsEditConfigAndSignalManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{
			ID:       "symphony",
			Workflow: paths.workflowPath,
			Workdir:  paths.workdirPath,
			Weight:   2,
			Priority: 4,
		},
	})

	signals := make(chan cli.Signal, 4)
	runCommand := func(args ...string) {
		t.Helper()

		allArgs := append([]string{"--config", configPath}, args...)
		cmd := cli.NewRootCommand(context.Background(), cli.WithSignalFunc(func(_ context.Context, signal cli.Signal) error {
			signals <- signal
			return nil
		}))
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(allArgs)

		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) error = %v", args, err)
		}
	}

	runCommand("pause", "symphony")
	assertProject(t, configPath, "symphony", func(project globalconfig.Project) {
		if !project.Paused {
			t.Fatal("Paused = false, want true")
		}
	})
	assertSignal(t, signals, cli.OperationPauseProject, "symphony")

	runCommand("unpause", "symphony")
	assertProject(t, configPath, "symphony", func(project globalconfig.Project) {
		if project.Paused {
			t.Fatal("Paused = true, want false")
		}
	})
	assertSignal(t, signals, cli.OperationUnpauseProject, "symphony")

	runCommand("promote", "symphony", "--priority", "1")
	assertProject(t, configPath, "symphony", func(project globalconfig.Project) {
		if project.Priority != 1 {
			t.Fatalf("Priority = %d, want 1", project.Priority)
		}
	})
	assertSignal(t, signals, cli.OperationPromoteProject, "symphony")

	runCommand("remove-project", "symphony")
	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
	assertSignal(t, signals, cli.OperationRemoveProject, "symphony")
}

func TestProjectAdminCommandsPreserveProjectPathLiterals(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	if err := os.WriteFile(configPath, []byte(`apiVersion: symphony/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: symphony
    workflow: cli.go
    workdir: .
    weight: 1
    priority: 0
  - id: docs
    workflow: cli.go
    workdir: .
    weight: 2
    priority: 1
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "pause", "symphony"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var written struct {
		Projects []struct {
			ID       string `yaml:"id"`
			Workflow string `yaml:"workflow"`
			Workdir  string `yaml:"workdir"`
			Paused   bool   `yaml:"paused"`
		} `yaml:"projects"`
	}
	if err := yaml.Unmarshal(raw, &written); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(written.Projects) != 2 {
		t.Fatalf("Projects length = %d, want 2", len(written.Projects))
	}
	for _, project := range written.Projects {
		if project.Workflow != "cli.go" {
			t.Fatalf("project %s workflow = %q, want cli.go", project.ID, project.Workflow)
		}
		if project.Workdir != "." {
			t.Fatalf("project %s workdir = %q, want .", project.ID, project.Workdir)
		}
	}
	if !written.Projects[0].Paused {
		t.Fatal("symphony Paused = false, want true")
	}
	if written.Projects[1].Paused {
		t.Fatal("docs Paused = true, want false")
	}
}

func TestProjectAdminCommandsRejectMissingProject(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, configPath, nil)

	cmd := cli.NewRootCommand(context.Background(), cli.WithSignalFunc(func(context.Context, cli.Signal) error {
		t.Fatal("signal should not be emitted")
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "pause", "missing"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !errors.Is(err, cli.ErrProjectNotFound) {
		t.Fatalf("Execute() error = %v, want %v", err, cli.ErrProjectNotFound)
	}
}

func TestAddProjectRejectsDuplicateID(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "symphony", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
	})

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", "symphony",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !errors.Is(err, cli.ErrProjectExists) {
		t.Fatalf("Execute() error = %v, want %v", err, cli.ErrProjectExists)
	}
}

func TestWithProjectManagerSignalsLiveManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	manager := &projectManagerProbe{}
	cmd := cli.NewRootCommand(context.Background(), cli.WithProjectManager(manager))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", "symphony",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if manager.added.ID != "symphony" {
		t.Fatalf("added project = %#v, want symphony", manager.added)
	}
}

type projectPaths struct {
	root         string
	workflowPath string
	workdirPath  string
}

func createProjectFiles(t *testing.T) projectPaths {
	t.Helper()

	root := t.TempDir()
	workdir := filepath.Join(root, "project")
	workflow := filepath.Join(workdir, "WORKFLOW.md")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(workflow, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	return projectPaths{
		root:         root,
		workflowPath: workflow,
		workdirPath:  workdir,
	}
}

func writeGlobalConfig(t *testing.T, path string, projects []globalconfig.Project) {
	t.Helper()

	cfg := globalconfig.Config{
		Path:       path,
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          globalconfig.SchedulingWeighted,
			FairShare:           map[string]any{"half_life": "1h"},
			Startup:             map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
		},
		Projects: projects,
	}
	if cfg.Projects == nil {
		cfg.Projects = []globalconfig.Project{}
	}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func assertProject(t *testing.T, configPath string, id string, assert func(globalconfig.Project)) {
	t.Helper()

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for _, project := range cfg.Projects {
		if project.ID == id {
			assert(project)
			return
		}
	}
	t.Fatalf("project %q not found in %#v", id, cfg.Projects)
}

func assertSignal(t *testing.T, signals <-chan cli.Signal, operation cli.Operation, projectID string) {
	t.Helper()

	signal := <-signals
	if signal.Operation != operation || signal.ProjectID != projectID {
		t.Fatalf("signal = %#v, want %s %s", signal, operation, projectID)
	}
}

type projectManagerProbe struct {
	added globalconfig.Project
}

func (p *projectManagerProbe) Add(_ context.Context, cfg globalconfig.Project) error {
	p.added = cfg
	return nil
}

func (p *projectManagerProbe) Remove(context.Context, project.ProjectID) error {
	return nil
}

func (p *projectManagerProbe) Pause(context.Context, project.ProjectID) error {
	return nil
}

func (p *projectManagerProbe) Unpause(context.Context, project.ProjectID) error {
	return nil
}
