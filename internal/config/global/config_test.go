package global

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/selector"
)

func TestResolvePathPrecedence(t *testing.T) {
	clearPathEnv(t)

	root := t.TempDir()
	home := configurePathTestHome(t, root)
	nativeRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}

	flagPath := filepath.Join(root, "flag.yaml")
	envPath := filepath.Join(root, "env.yaml")
	detentHome := filepath.Join(root, "detent-home")
	deprecatedEnvPath := filepath.Join(root, "deprecated-env.yaml")
	deprecatedHome := filepath.Join(root, "deprecated-home")
	homePath := filepath.Join(detentHome, "global.yaml")
	deprecatedHomePath := filepath.Join(deprecatedHome, "global.yaml")
	nativePath := filepath.Join(nativeRoot, "detent", "global.yaml")
	legacyPath := filepath.Join(home, ".detent", "global.yaml")
	for _, path := range []string{flagPath, envPath, homePath, deprecatedEnvPath, deprecatedHomePath, nativePath, legacyPath} {
		writeFile(t, path, "# config\n")
	}
	t.Setenv("CONFIG", envPath)
	t.Setenv("CONFIG_HOME", detentHome)
	t.Setenv("DETENT_CONFIG", deprecatedEnvPath)
	t.Setenv("DETENT_HOME", deprecatedHome)

	tests := []struct {
		name     string
		flagPath string
		setup    func(*testing.T)
		wantPath string
		wantRule PathRule
	}{
		{
			name:     "flag wins",
			flagPath: flagPath,
			wantPath: flagPath,
			wantRule: PathRuleFlag,
		},
		{
			name: "config env wins after flag",
			setup: func(t *testing.T) {
				t.Setenv("CONFIG", envPath)
				t.Setenv("CONFIG_HOME", detentHome)
			},
			wantPath: envPath,
			wantRule: PathRuleEnvConfig,
		},
		{
			name: "config home wins before deprecated direct config env",
			setup: func(t *testing.T) {
				t.Setenv("CONFIG", "")
				t.Setenv("CONFIG_HOME", detentHome)
			},
			wantPath: homePath,
			wantRule: PathRuleEnvHome,
		},
		{
			name: "deprecated direct config env wins before deprecated home env",
			setup: func(t *testing.T) {
				t.Setenv("CONFIG", "")
				t.Setenv("CONFIG_HOME", "")
			},
			wantPath: deprecatedEnvPath,
			wantRule: PathRuleDeprecatedEnvConfig,
		},
		{
			name: "native config wins before legacy",
			setup: func(t *testing.T) {
				clearPathEnv(t)
			},
			wantPath: nativePath,
			wantRule: PathRuleUserConfigDir,
		},
		{
			name: "legacy config wins when native is missing",
			setup: func(t *testing.T) {
				clearPathEnv(t)
				if err := os.Remove(nativePath); err != nil {
					t.Fatalf("Remove() error = %v", err)
				}
			},
			wantPath: legacyPath,
			wantRule: PathRuleLegacyHome,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}

			got, err := ResolvePath(tt.flagPath)
			if err != nil {
				t.Fatalf("ResolvePath() error = %v", err)
			}
			if got.Path != tt.wantPath {
				t.Fatalf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Rule != tt.wantRule {
				t.Fatalf("Rule = %q, want %q", got.Rule, tt.wantRule)
			}
		})
	}
}

func TestResolvePathUsesNativeConfigDirWhenNoConfigExists(t *testing.T) {
	clearPathEnv(t)
	configurePathTestHome(t, t.TempDir())

	nativeRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}

	got, err := ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath() error = %v", err)
	}

	want := filepath.Join(nativeRoot, "detent", "global.yaml")
	if got.Path != want {
		t.Fatalf("Path = %q, want %q", got.Path, want)
	}
	if got.Rule != PathRuleUserConfigDir {
		t.Fatalf("Rule = %q, want %q", got.Rule, PathRuleUserConfigDir)
	}
}

func TestDefaultPath(t *testing.T) {
	clearPathEnv(t)
	configurePathTestHome(t, t.TempDir())

	nativeRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(nativeRoot, "detent", "global.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathHonorsConfigHome(t *testing.T) {
	root := t.TempDir()
	home := configurePathTestHome(t, root)
	clearPathEnv(t)
	t.Setenv("CONFIG_HOME", "~/custom")

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(home, "custom", "global.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathFallsBackToDeprecatedDetentHome(t *testing.T) {
	root := t.TempDir()
	home := configurePathTestHome(t, root)
	clearPathEnv(t)
	t.Setenv("DETENT_HOME", "~/custom")

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(home, "custom", "global.yaml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfig(t *testing.T) {
	clearPathEnv(t)

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
	if cfg.TrustLoopbackPeerRead {
		t.Fatal("TrustLoopbackPeerRead = true, want default false")
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
	if cfg.Global.Identity.Configured() {
		t.Fatalf("Global.Identity = %#v, want omitted default", cfg.Global.Identity)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
}

func TestReadParsesRuntimeSettings(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
env: dev
log_level: debug
log_max_size_bytes: 4096
log_max_backups: 3
github_token: gh
api_token: detent_test_api_token
trust_loopback_peer_read: true
port: 4100
instance_name: " buildbox "
update:
  auto_check_enabled: true
  check_interval_hours: 12
  auto_apply_enabled: true
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workflow_ref: origin/main
    workdir: `+paths.workdir+`
    color: "#1192e8"
    weight: 1
    priority: 0
`)

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if cfg.Env != "dev" {
		t.Fatalf("Env = %q, want dev", cfg.Env)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.LogMaxSizeBytes == nil || *cfg.LogMaxSizeBytes != 4096 {
		t.Fatalf("LogMaxSizeBytes = %v, want 4096", cfg.LogMaxSizeBytes)
	}
	if cfg.LogMaxBackups == nil || *cfg.LogMaxBackups != 3 {
		t.Fatalf("LogMaxBackups = %v, want 3", cfg.LogMaxBackups)
	}
	if cfg.GitHubToken != "gh" {
		t.Fatalf("GitHubToken = %q, want gh", cfg.GitHubToken)
	}
	if cfg.APIToken != "detent_test_api_token" {
		t.Fatalf("APIToken = %q, want detent_test_api_token", cfg.APIToken)
	}
	if !cfg.TrustLoopbackPeerRead {
		t.Fatal("TrustLoopbackPeerRead = false, want true")
	}
	if cfg.Port == nil || *cfg.Port != 4100 {
		t.Fatalf("Port = %v, want 4100", cfg.Port)
	}
	if cfg.InstanceName != "buildbox" {
		t.Fatalf("InstanceName = %q, want buildbox", cfg.InstanceName)
	}
	if !cfg.Update.AutoCheckEnabled || !cfg.Update.AutoApplyEnabled || cfg.Update.NormalizedCheckIntervalHours() != 12 {
		t.Fatalf("Update = %#v, want enabled 12-hour auto-apply", cfg.Update)
	}
	if cfg.Projects[0].WorkflowRef != "origin/main" {
		t.Fatalf("Project.WorkflowRef = %q, want origin/main", cfg.Projects[0].WorkflowRef)
	}
	if cfg.Projects[0].Color != "#1192e8" {
		t.Fatalf("Project.Color = %q, want #1192e8", cfg.Projects[0].Color)
	}
}

func TestReadParsesAndValidatesActiveHours(t *testing.T) {
	paths := createProjectFiles(t)
	tests := []struct {
		name        string
		global      string
		project     string
		wantProblem string
	}{
		{
			name: "global default and project override",
			global: `
  active_hours:
    timezone: America/Chicago
    windows:
      - Mon-Sun 22:00-06:00`,
			project: `
    active_hours:
      timezone: America/New_York
      windows:
        - Mon-Fri 21:00-05:00
    active_hours_override_until: 2026-08-08T02:00:00Z`,
		},
		{
			name: "windows without timezone",
			project: `
    active_hours:
      windows:
        - Mon-Sun 22:00-06:00`,
			wantProblem: "projects[0].active_hours.timezone: is required",
		},
		{
			name: "malformed window",
			global: `
  active_hours:
    timezone: UTC
    windows:
      - weekdays overnight`,
			wantProblem: "global.active_hours.windows[0]",
		},
		{
			name: "invalid override timestamp",
			project: `
    active_hours_override_until: tomorrow`,
			wantProblem: "active_hours_override_until: must be an RFC 3339 timestamp",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(paths.root, strings.ReplaceAll(test.name, " ", "-")+".yaml")
			writeFile(t, path, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted`+test.global+`
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 1
    priority: 0`+test.project+`
`)

			cfg, err := Read(path, WithHome(paths.home))
			if test.wantProblem != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantProblem) {
					t.Fatalf("Read() error = %v, want containing %q", err, test.wantProblem)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if cfg.Global.ActiveHours == nil || cfg.Global.ActiveHours.Timezone != "America/Chicago" {
				t.Fatalf("Global.ActiveHours = %#v", cfg.Global.ActiveHours)
			}
			if cfg.Projects[0].ActiveHours == nil || cfg.Projects[0].ActiveHours.Timezone != "America/New_York" {
				t.Fatalf("Project.ActiveHours = %#v", cfg.Projects[0].ActiveHours)
			}
			if got := cfg.Projects[0].ActiveHoursOverrideUntil; got != "2026-08-08T02:00:00Z" {
				t.Fatalf("ActiveHoursOverrideUntil = %q", got)
			}
		})
	}
}

func TestReadValidatesUpdateSettings(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	tests := []struct {
		name   string
		update string
		want   string
	}{
		{name: "mapping", update: "disabled", want: "update: must be a mapping"},
		{name: "enabled boolean", update: "\n  auto_check_enabled: yes-please", want: "update.auto_check_enabled: must be a boolean"},
		{name: "positive interval", update: "\n  check_interval_hours: 0", want: "update.check_interval_hours: must be a positive integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(paths.root, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
update: `+tt.update+`
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 1
    priority: 0
`)

			_, err := Read(configPath, WithHome(paths.home))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Read() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestUpdateUsesSixHourDefaultInterval(t *testing.T) {
	t.Parallel()

	if got := (Update{}).NormalizedCheckIntervalHours(); got != 6 {
		t.Fatalf("NormalizedCheckIntervalHours() = %d, want 6", got)
	}
}

func TestReadAllowsRefBackedRelativeWorkflow(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: WORKFLOW.md
    workflow_ref: origin/main
    workdir: `+paths.workdir+`
    weight: 1
    priority: 0
`)

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	got := cfg.Projects[0]
	if got.Workflow != "WORKFLOW.md" {
		t.Fatalf("Project.Workflow = %q, want WORKFLOW.md", got.Workflow)
	}
	if got.WorkflowRef != "origin/main" {
		t.Fatalf("Project.WorkflowRef = %q, want origin/main", got.WorkflowRef)
	}
}

func TestReadRejectsPlainRelativeWorkflowPath(t *testing.T) {
	root := t.TempDir()
	daemonDir := filepath.Join(root, "daemon")
	projectDir := filepath.Join(root, "project")
	writeFile(t, filepath.Join(daemonDir, "WORKFLOW.md"), "---\ntracker:\n  kind: memory\n---\ndaemon\n")
	writeFile(t, filepath.Join(projectDir, "WORKFLOW.md"), "---\ntracker:\n  kind: memory\n---\nproject\n")
	configPath := filepath.Join(root, "global.yaml")
	writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: WORKFLOW.md
    workdir: `+projectDir+`
    weight: 1
    priority: 0
`)

	t.Chdir(daemonDir)

	_, err := Read(configPath)
	if err == nil {
		t.Fatal("Read() error = nil, want relative workflow validation error")
	}
	want := "projects[0].workflow: must be absolute or home-relative when workflow_ref is empty"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Read() error = %q, want substring %q", err, want)
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

func TestWriteRoundTripsConfig(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "written", "global.yaml")
	port := 4100
	logMaxSizeBytes := 4096
	logMaxBackups := 3

	cfg := Config{
		Path:                  configPath,
		APIVersion:            APIVersion,
		Kind:                  Kind,
		Env:                   "dev",
		LogLevel:              "debug",
		LogMaxSizeBytes:       &logMaxSizeBytes,
		LogMaxBackups:         &logMaxBackups,
		GitHubToken:           "gh",
		APIToken:              "detent_test_api_token",
		TrustLoopbackPeerRead: true,
		Port:                  &port,
		Global: Settings{
			MaxConcurrentAgents: 3,
			Scheduling:          SchedulingStrict,
			AgentPools: []AgentPool{
				{Name: "code", MaxConcurrentAgents: 5},
				{Name: "video", MaxConcurrentAgents: 10, BurstTo: 15, Scheduling: SchedulingRoundRobin},
			},
			FairShare: map[string]any{"half_life": "30m"},
			Startup:   map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
			Identity: Identity{
				Name:          "release-captain",
				GitHubLogin:   "detent-bot",
				OwnershipMode: "field",
				OwnerField:    "Owner",
			},
		},
		Projects: []Project{
			{
				ID:               "detent",
				Pool:             "code",
				Workflow:         paths.workflowPath,
				Workdir:          paths.workdirPath,
				Color:            "#1192e8",
				Weight:           5,
				Priority:         2,
				Paused:           true,
				PausedReason:     "release hold",
				PausedAt:         "2026-07-20T12:00:00Z",
				PausedUntilIssue: "digitaldrywood/detent#1499",
				CredentialRef:    "github-default",
				Authorization: selector.Selector{
					Labels: selector.Labels{Include: []string{"release"}},
					Fields: []selector.FieldEquals{{Name: "Track", Value: "multi-instance"}},
				},
			},
		},
	}

	if err := Write(configPath, cfg, WithHome(paths.home)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if !reflect.DeepEqual(got.Global, cfg.Global) {
		t.Fatalf("Global = %#v, want %#v", got.Global, cfg.Global)
	}
	if got.Env != cfg.Env {
		t.Fatalf("Env = %q, want %q", got.Env, cfg.Env)
	}
	if got.LogLevel != cfg.LogLevel {
		t.Fatalf("LogLevel = %q, want %q", got.LogLevel, cfg.LogLevel)
	}
	if got.LogMaxSizeBytes == nil || *got.LogMaxSizeBytes != *cfg.LogMaxSizeBytes {
		t.Fatalf("LogMaxSizeBytes = %v, want %d", got.LogMaxSizeBytes, *cfg.LogMaxSizeBytes)
	}
	if got.LogMaxBackups == nil || *got.LogMaxBackups != *cfg.LogMaxBackups {
		t.Fatalf("LogMaxBackups = %v, want %d", got.LogMaxBackups, *cfg.LogMaxBackups)
	}
	if got.GitHubToken != cfg.GitHubToken {
		t.Fatalf("GitHubToken = %q, want %q", got.GitHubToken, cfg.GitHubToken)
	}
	if got.APIToken != cfg.APIToken {
		t.Fatalf("APIToken = %q, want %q", got.APIToken, cfg.APIToken)
	}
	if got.TrustLoopbackPeerRead != cfg.TrustLoopbackPeerRead {
		t.Fatalf("TrustLoopbackPeerRead = %t, want %t", got.TrustLoopbackPeerRead, cfg.TrustLoopbackPeerRead)
	}
	if got.Port == nil || *got.Port != *cfg.Port {
		t.Fatalf("Port = %v, want %d", got.Port, *cfg.Port)
	}
	if !reflect.DeepEqual(got.Projects, cfg.Projects) {
		t.Fatalf("Projects = %#v, want %#v", got.Projects, cfg.Projects)
	}
}

func TestWritePreservesCommentsAndKeyOrder(t *testing.T) {
	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	raw := `apiVersion: detent/v1
kind: GlobalConfig
instance_name: prometheus
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    # Keep this project note.
    weight: 1
    priority: 0
# Keep this update note.
update:
  auto_check_enabled: true
`
	writeFile(t, path, raw)

	cfg, err := Read(path, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	cfg.Projects[0].Paused = true
	cfg.Projects[0].PausedReason = "release hold"
	cfg.Projects[0].PausedAt = "2026-07-20T12:00:00Z"
	cfg.Projects[0].PausedUntil = "2026-08-01T12:00:00Z"
	if err := Write(path, cfg, WithHome(paths.home)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(written)
	for _, want := range []string{
		"# Keep this project note.",
		"# Keep this update note.",
		"paused_reason: release hold",
		`paused_at: "2026-07-20T12:00:00Z"`,
		`paused_until: "2026-08-01T12:00:00Z"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("written config missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "update:") < strings.Index(text, "projects:") {
		t.Fatalf("update block moved before projects:\n%s", text)
	}
}

func TestWritePreservesDuplicateKnowledgeSourceNames(t *testing.T) {
	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	writeFile(t, path, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  knowledge:
    sources:
      - name: Standards
        path: `+filepath.Join(paths.root, "first.md")+`
      - name: Standards
        path: `+filepath.Join(paths.root, "second.md")+`
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 1
    priority: 0
`)

	cfg, err := Read(path, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if err := Write(path, cfg, WithHome(paths.home)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	roundTripped, err := Read(path, WithHome(paths.home))
	if err != nil {
		t.Fatalf("second Read() error = %v", err)
	}
	sources := roundTripped.Global.Knowledge.Sources
	if len(sources) != 2 || sources[0].Path != filepath.Join(paths.root, "first.md") ||
		sources[1].Path != filepath.Join(paths.root, "second.md") {
		t.Fatalf("knowledge sources = %#v, want distinct duplicate-name entries", sources)
	}
}

func TestWriteMatchesAgentPoolsByName(t *testing.T) {
	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	writeFile(t, path, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  agent_pools:
    - name: code
      # Code pool.
      max_concurrent_agents: 1
    - name: video
      # Video pool.
      max_concurrent_agents: 2
projects:
  - id: detent
    pool: code
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 1
    priority: 0
`)

	cfg, err := Read(path, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	cfg.Global.AgentPools[0], cfg.Global.AgentPools[1] = cfg.Global.AgentPools[1], cfg.Global.AgentPools[0]
	if err := Write(path, cfg, WithHome(paths.home)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(written)
	videoIndex := strings.Index(text, "- name: video")
	codeIndex := strings.Index(text, "- name: code")
	videoComment := strings.Index(text, "# Video pool.")
	codeComment := strings.Index(text, "# Code pool.")
	if videoIndex < 0 || codeIndex < 0 || videoComment < videoIndex || videoComment > codeIndex ||
		codeIndex < videoIndex || codeComment < codeIndex {
		t.Fatalf("agent pool comments did not follow names:\n%s", text)
	}
}

func TestParsePauseMetadataValidatesExitConditions(t *testing.T) {
	paths := createProjectFiles(t)
	base := minimalYAML(paths, "  max_concurrent_agents: 8\n  scheduling: weighted\n")
	raw := strings.Replace(
		base,
		"    priority: 50\n",
		"    priority: 50\n    paused: true\n    paused_reason: release hold\n    paused_at: 2026-07-20T12:00:00Z\n    paused_until_issue: digitaldrywood/detent#1499\n    paused_until: 2026-08-01T12:00:00Z\n",
		1,
	)

	_, err := Parse([]byte(raw), filepath.Join(paths.root, "global.yaml"), WithHome(paths.home))
	if err == nil {
		t.Fatal("Parse() error = nil, want mutually exclusive exit condition error")
	}
	want := "projects[0].paused_until_issue and projects[0].paused_until: must not both be set"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse() error = %q, want substring %q", err, want)
	}
}

func TestParsePauseMetadataAcceptsUnquotedRFC3339Timestamp(t *testing.T) {
	paths := createProjectFiles(t)
	raw := strings.Replace(
		minimalYAML(paths, "  max_concurrent_agents: 8\n  scheduling: weighted\n"),
		"    priority: 50\n",
		"    priority: 50\n    paused: true\n    paused_reason: release hold\n    paused_at: 2026-07-20T12:00:00Z\n    paused_until: 2026-08-01T12:00:00Z\n",
		1,
	)

	cfg, err := Parse([]byte(raw), filepath.Join(paths.root, "global.yaml"), WithHome(paths.home))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	project := cfg.Projects[0]
	if project.PausedReason != "release hold" ||
		project.PausedAt != "2026-07-20T12:00:00Z" ||
		project.PausedUntil != "2026-08-01T12:00:00Z" {
		t.Fatalf("pause metadata = %#v", project)
	}
}

func TestWriteRestrictsExistingConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows does not expose POSIX file modes")
	}

	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	writeFile(t, path, "old")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	err := Write(path, Config{
		APIVersion:  APIVersion,
		Kind:        Kind,
		GitHubToken: "ghp_secret",
		Global: Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          SchedulingWeighted,
		},
		Projects: []Project{
			{ID: "detent", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != configFileMode {
		t.Fatalf("mode = %o, want %o", got, configFileMode)
	}
}

func TestWriteValidatesConfigBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.yaml")

	err := Write(path, Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Global: Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          SchedulingWeighted,
		},
		Projects: []Project{
			{ID: "detent", Weight: 0},
		},
	})
	if err == nil {
		t.Fatal("Write() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "projects[0].workflow") {
		t.Fatalf("Write() error = %v, want workflow validation", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Stat() error = %v, want not exist", statErr)
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
	if project.ID != "detent" {
		t.Fatalf("Project.ID = %q, want detent", project.ID)
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
	if project.Authorization.Configured() {
		t.Fatalf("Project.Authorization = %#v, want authorize all default", project.Authorization)
	}
}

func TestReadParsesIdentityAndAuthorization(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  identity:
    name: release-captain
    github_login: detent-bot
    ownership_mode: field
    owner_field: Owner
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 5
    priority: 50
    authorization:
      author_in:
        - "@me"
      labels:
        exclude:
          - blocked
      and:
        - fields:
            - name: Track
              value: multi-instance
`)

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if cfg.Global.Identity.Name != "release-captain" {
		t.Fatalf("Global.Identity.Name = %q, want release-captain", cfg.Global.Identity.Name)
	}
	if cfg.Global.Identity.GitHubLogin != "detent-bot" {
		t.Fatalf("Global.Identity.GitHubLogin = %q, want detent-bot", cfg.Global.Identity.GitHubLogin)
	}
	if cfg.Global.Identity.OwnershipMode != "field" {
		t.Fatalf("Global.Identity.OwnershipMode = %q, want field", cfg.Global.Identity.OwnershipMode)
	}
	if cfg.Global.Identity.OwnerField != "Owner" {
		t.Fatalf("Global.Identity.OwnerField = %q, want Owner", cfg.Global.Identity.OwnerField)
	}

	wantAuthorization := selector.Selector{
		AuthorIn: []string{"@me"},
		Labels:   selector.Labels{Exclude: []string{"blocked"}},
		And: []selector.Selector{
			{Fields: []selector.FieldEquals{{Name: "Track", Value: "multi-instance"}}},
		},
	}
	if got := cfg.Projects[0].Authorization; !reflect.DeepEqual(got, wantAuthorization) {
		t.Fatalf("Project.Authorization = %#v, want %#v", got, wantAuthorization)
	}
}

func TestReadParsesKnowledgeWithoutRequiringFiles(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  knowledge:
    max_bytes: 1024
    sources:
      - name: Global standards
        path: ~/detent-knowledge/global.md
projects:
  - id: detent
    workflow: `+paths.workflow+`
    workdir: `+paths.workdir+`
    weight: 5
    priority: 50
    knowledge:
      sources:
        - name: Project standards
          path: ~/detent-knowledge/project.md
`)

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if cfg.Global.Knowledge.MaxBytes != 1024 {
		t.Fatalf("Global.Knowledge.MaxBytes = %d, want 1024", cfg.Global.Knowledge.MaxBytes)
	}
	if len(cfg.Global.Knowledge.Sources) != 1 {
		t.Fatalf("Global.Knowledge.Sources len = %d, want 1", len(cfg.Global.Knowledge.Sources))
	}
	if source := cfg.Global.Knowledge.Sources[0]; source.Name != "Global standards" || source.Path != filepath.Join(paths.home, "detent-knowledge", "global.md") {
		t.Fatalf("Global.Knowledge.Sources[0] = %#v", source)
	}
	if len(cfg.Projects[0].Knowledge.Sources) != 1 {
		t.Fatalf("Project.Knowledge.Sources len = %d, want 1", len(cfg.Projects[0].Knowledge.Sources))
	}
	if source := cfg.Projects[0].Knowledge.Sources[0]; source.Name != "Project standards" || source.Path != filepath.Join(paths.home, "detent-knowledge", "project.md") {
		t.Fatalf("Project.Knowledge.Sources[0] = %#v", source)
	}
}

func TestReadWithProjectPathLiteralsPreservesYAMLLiterals(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, validYAML(paths, nil))

	cfg, err := Read(configPath, WithHome(paths.home), WithProjectPathLiterals())
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects length = %d, want 1", len(cfg.Projects))
	}
	project := cfg.Projects[0]
	if project.Workflow != paths.workflow {
		t.Fatalf("Project.Workflow = %q, want %q", project.Workflow, paths.workflow)
	}
	if project.Workdir != paths.workdir {
		t.Fatalf("Project.Workdir = %q, want %q", project.Workdir, paths.workdir)
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

func TestReadParsesAgentPools(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	raw := strings.Replace(
		validYAML(paths, map[string]string{"scheduling": SchedulingStrict}),
		"  scheduling: strict\n",
		`  scheduling: strict
  agent_pools:
    - name: code
      max_concurrent_agents: 5
    - name: video
      max_concurrent_agents: 10
      burst_to: 15
      scheduling: round_robin
`,
		1,
	)
	raw = strings.Replace(raw, `  - id: " detent "`+"\n", `  - id: " detent "
    pool: video
`, 1)
	writeFile(t, configPath, raw)

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	wantPools := []AgentPool{
		{Name: "code", MaxConcurrentAgents: 5},
		{Name: "video", MaxConcurrentAgents: 10, BurstTo: 15, Scheduling: SchedulingRoundRobin},
	}
	if !reflect.DeepEqual(cfg.Global.AgentPools, wantPools) {
		t.Fatalf("Global.AgentPools = %#v, want %#v", cfg.Global.AgentPools, wantPools)
	}
	if cfg.Projects[0].Pool != "video" {
		t.Fatalf("Projects[0].Pool = %q, want video", cfg.Projects[0].Pool)
	}
}

func TestReadReportsAgentPoolValidationErrors(t *testing.T) {
	paths := createProjectFiles(t)
	base := validYAML(paths, nil)
	withPools := func(pools string) string {
		return strings.Replace(base, "  scheduling: weighted\n", "  scheduling: weighted\n"+pools, 1)
	}
	withProjectPool := func(raw string, pool string) string {
		return strings.Replace(raw, `  - id: " detent "`+"\n", `  - id: " detent "
    pool: `+pool+`
`, 1)
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate name",
			raw: withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 1
    - name: code
      max_concurrent_agents: 2
`),
			want: `global.agent_pools[1].name: duplicates global.agent_pools[0].name "code"`,
		},
		{
			name: "blank name",
			raw: withPools(`  agent_pools:
    - name: "  "
      max_concurrent_agents: 1
`),
			want: "global.agent_pools[0].name: must not be blank",
		},
		{
			name: "reserved default",
			raw: withPools(`  agent_pools:
    - name: default
      max_concurrent_agents: 1
`),
			want: "global.agent_pools[0].name: default is reserved",
		},
		{
			name: "missing capacity",
			raw: withPools(`  agent_pools:
    - name: code
`),
			want: "global.agent_pools[0].max_concurrent_agents: is required",
		},
		{
			name: "nonpositive capacity",
			raw: withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 0
`),
			want: "global.agent_pools[0].max_concurrent_agents: must be a positive integer",
		},
		{
			name: "nonpositive burst",
			raw: withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 1
      burst_to: 0
`),
			want: "global.agent_pools[0].burst_to: must be a positive integer",
		},
		{
			name: "burst below guarantee",
			raw: withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 2
      burst_to: 1
`),
			want: "global.agent_pools[0].burst_to: must be greater than or equal to max_concurrent_agents",
		},
		{
			name: "invalid scheduling",
			raw: withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 1
      scheduling: random
`),
			want: "global.agent_pools[0].scheduling: must be one of weighted, strict, round_robin, fair_share",
		},
		{
			name: "undefined project pool",
			raw: withProjectPool(withPools(`  agent_pools:
    - name: code
      max_concurrent_agents: 1
`), "video"),
			want: `projects[0].pool: project "detent" references undefined agent pool "video"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(paths.root, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeFile(t, configPath, tt.raw)
			_, err := Read(configPath, WithHome(paths.home))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Read() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReadWithoutAgentPoolsPreservesLegacyDefaults(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeFile(t, configPath, validYAML(paths, nil))

	cfg, err := Read(configPath, WithHome(paths.home))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if cfg.Global.AgentPools != nil {
		t.Fatalf("Global.AgentPools = %#v, want nil", cfg.Global.AgentPools)
	}
	if cfg.Projects[0].Pool != "" {
		t.Fatalf("Projects[0].Pool = %q, want empty legacy value", cfg.Projects[0].Pool)
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
			raw: `apiVersion: detent/v2
kind: SomethingElse
instance_name: []
trust_loopback_peer_read: maybe
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
    workflow_ref: 789
    workdir: 789
    color: []
    weight: 0
    priority: high
    paused: maybe
    credential_ref: []
`,
			want: []string{
				"apiVersion: must equal detent/v1",
				"kind: must equal GlobalConfig",
				"instance_name: must be a string",
				"trust_loopback_peer_read: must be a boolean",
				"global.max_concurrent_agents: must be a positive integer",
				"global.scheduling: must be one of weighted, strict, round_robin, fair_share",
				"global.fair_share: must be a mapping",
				"global.startup.jitter_seconds: must be an integer greater than or equal to 0",
				"global.startup.max_spawn_per_second: must be a positive integer",
				"projects[0].id: must be a string",
				"projects[0].workflow: must be a string",
				"projects[0].workflow_ref: must be a string",
				"projects[0].workdir: must be a string",
				"projects[0].color: must be a string",
				"projects[0].weight: must be a positive integer",
				"projects[0].priority: must be an integer",
				"projects[0].paused: must be a boolean",
				"projects[0].credential_ref: must be a string",
			},
		},
		{
			name: "invalid instance name",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
instance_name: |-
  buildbox
  prod
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects: []
`,
			want: []string{
				"instance_name: must be a single line",
			},
		},
		{
			name: "missing project fields",
			raw: `apiVersion: detent/v1
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
			name: "invalid project color",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    color: "1192e8"
    weight: 5
    priority: 50
`,
			want: []string{
				"projects[0].color: must be an opaque CSS hex color like #1192e8",
			},
		},
		{
			name: "invalid identity and authorization",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  identity:
    github_login: detent-bot
    ownership_mode: field
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 5
    priority: 50
    authorization:
      fields:
        - value: multi-instance
`,
			want: []string{
				"global.identity.name must not be blank",
				"global.identity.owner_field is required when global.identity.ownership_mode is field",
				"projects[0].authorization.fields[0].name must not be blank",
			},
		},
		{
			name: "invalid knowledge",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  knowledge:
    enabled: yes
    max_bytes: 0
    sources:
      - name: |-
          global
          source
        path: ""
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 5
    priority: 50
    knowledge:
      sources: {}
`,
			want: []string{
				"global.knowledge.enabled: must be a boolean",
				"global.knowledge.max_bytes: must be a positive integer",
				"global.knowledge.sources[0].name: must be a single line",
				"global.knowledge.sources[0].path: must not be blank",
				"projects[0].knowledge.sources: must be a list",
			},
		},
		{
			name: "missing paths and duplicate project ids",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: " detent "
    workflow: ~/missing/WORKFLOW.md
    workdir: ~/missing
    weight: 5
    priority: 50
  - id: detent
    workflow: ~/missing/WORKFLOW.md
    workdir: ~/missing
    weight: 10
    priority: 0
`,
			want: []string{
				"projects[0].workflow: path does not exist",
				"projects[0].workdir: path does not exist",
				"projects.id: duplicate id detent",
			},
		},
		{
			name: "containers must have expected shapes",
			raw: `apiVersion: detent/v1
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

func TestBuildReturnsErrorsForDecodedConfigMismatches(t *testing.T) {
	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")

	tests := []struct {
		name   string
		mutate func(map[string]any)
		opts   options
		want   string
	}{
		{
			name: "api version type mismatch",
			mutate: func(attrs map[string]any) {
				attrs["apiVersion"] = 123
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "apiVersion: must be a string",
		},
		{
			name: "global type mismatch",
			mutate: func(attrs map[string]any) {
				attrs["global"] = []any{}
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "global: must be a mapping",
		},
		{
			name: "projects type mismatch",
			mutate: func(attrs map[string]any) {
				attrs["projects"] = map[string]any{}
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "projects: must be a list",
		},
		{
			name: "setting type mismatch",
			mutate: func(attrs map[string]any) {
				global := attrs["global"].(map[string]any)
				global["max_concurrent_agents"] = "8"
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "global.max_concurrent_agents: must be an integer",
		},
		{
			name: "project optional bool mismatch",
			mutate: func(attrs map[string]any) {
				project := attrs["projects"].([]any)[0].(map[string]any)
				project["paused"] = "false"
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "projects[0].paused: must be a boolean",
		},
		{
			name: "project color mismatch",
			mutate: func(attrs map[string]any) {
				project := attrs["projects"].([]any)[0].(map[string]any)
				project["color"] = "#fffff"
			},
			opts: options{home: paths.home, relativeTo: paths.root},
			want: "projects[0].color: must be an opaque CSS hex color like #1192e8",
		},
		{
			name: "project path expansion mismatch",
			mutate: func(attrs map[string]any) {
				project := attrs["projects"].([]any)[0].(map[string]any)
				project["workflow"] = "~/WORKFLOW.md"
			},
			opts: options{relativeTo: paths.root},
			want: "projects[0].workflow: expand path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := validDecodedConfig(paths)
			tt.mutate(attrs)

			_, err := build(attrs, configPath, tt.opts)
			if err == nil {
				t.Fatal("build() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("build() error = %q, want substring %q", err, tt.want)
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
	workdir := filepath.Join(home, "projects", "digitaldrywood", "detent-orchestration")
	workflow := filepath.Join(workdir, "WORKFLOW.md")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, workflow, "# workflow\n")

	return projectPaths{
		root:         root,
		home:         home,
		workflow:     "~/projects/digitaldrywood/detent-orchestration/WORKFLOW.md",
		workdir:      "~/projects/digitaldrywood/detent-orchestration",
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

func configurePathTestHome(t *testing.T, root string) string {
	t.Helper()

	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	switch runtime.GOOS {
	case "windows":
		t.Setenv("USERPROFILE", home)
		t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	default:
		t.Setenv("XDG_CONFIG_HOME", "")
	}
	return home
}

func clearPathEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{"CONFIG", "CONFIG_HOME", "DETENT_CONFIG", "DETENT_HOME"} {
		t.Setenv(key, "")
	}
}

func assertMap(t *testing.T, name string, got map[string]any, want map[string]any) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
}

func minimalYAML(paths projectPaths, global string) string {
	return `apiVersion: detent/v1
kind: GlobalConfig
global:
` + global + `projects:
  - id: detent
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

	return `apiVersion: detent/v1
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
  - id: " detent "
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 5
    priority: 50
    credential_ref: " github-default "
`
}

func validDecodedConfig(paths projectPaths) map[string]any {
	return map[string]any{
		"apiVersion": APIVersion,
		"kind":       Kind,
		"global": map[string]any{
			"max_concurrent_agents": 8,
			"scheduling":            SchedulingWeighted,
			"fair_share": map[string]any{
				"half_life": "1h",
			},
			"startup": map[string]any{
				"jitter_seconds":       10,
				"max_spawn_per_second": 2,
			},
		},
		"projects": []any{
			map[string]any{
				"id":             " detent ",
				"workflow":       paths.workflow,
				"workdir":        paths.workdir,
				"weight":         5,
				"priority":       50,
				"credential_ref": " github-default ",
			},
		},
	}
}
