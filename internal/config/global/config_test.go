package global

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
	if cfg.Global.Memory.MaxAgentRSSBytes != DefaultMaxAgentRSSBytes ||
		cfg.Global.Memory.PressureSomeAvg60Threshold != DefaultMemoryPressureSomeAvg60Threshold ||
		cfg.Global.Memory.PollIntervalMS != DefaultMemoryPollIntervalMS {
		t.Fatalf("Global.Memory = %#v, want defaults", cfg.Global.Memory)
	}
	if cfg.Global.IO.PressureFullAvg10Threshold != DefaultIOPressureFullAvg10Threshold ||
		cfg.Global.IO.PollIntervalMS != DefaultIOPressurePollIntervalMS {
		t.Fatalf("Global.IO = %#v, want defaults", cfg.Global.IO)
	}
	if cfg.Global.CPU.PressureSomeAvg10Threshold != DefaultCPUPressureSomeAvg10Threshold ||
		cfg.Global.CPU.PollIntervalMS != DefaultCPUPressurePollIntervalMS {
		t.Fatalf("Global.CPU = %#v, want defaults", cfg.Global.CPU)
	}
	if cfg.Global.Identity.Configured() {
		t.Fatalf("Global.Identity = %#v, want omitted default", cfg.Global.Identity)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
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
		{name: "positive max deferral", update: "\n  max_deferral_hours: 0", want: "update.max_deferral_hours: must be a positive integer"},
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
	if got := (Update{}).NormalizedMaxDeferralHours(); got != 6 {
		t.Fatalf("NormalizedMaxDeferralHours() = %d, want 6", got)
	}
}

func TestHubRunnerUpdateDefaultsAndOptOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		hub       bool
		update    string
		wantCheck bool
		wantApply bool
	}{
		{name: "standalone disabled"},
		{name: "Hub defaults", hub: true, wantCheck: true, wantApply: true},
		{name: "Hub interval only", hub: true, update: "update:\n  check_interval_hours: 2\n", wantCheck: true, wantApply: true},
		{name: "Hub check opt out", hub: true, update: "update:\n  auto_check_enabled: false\n", wantApply: true},
		{name: "Hub apply opt out", hub: true, update: "update:\n  auto_apply_enabled: false\n", wantCheck: true},
		{name: "Hub both opt out", hub: true, update: "update:\n  auto_check_enabled: false\n  auto_apply_enabled: false\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			paths := createProjectFiles(t)
			configPath := filepath.Join(paths.root, "global.yaml")
			client := ""
			if test.hub {
				client = "client:\n  hub_url: https://hub.example.test\n"
			}
			writeFile(t, configPath, "apiVersion: detent/v1\nkind: GlobalConfig\n"+client+test.update+"global:\n  max_concurrent_agents: 4\n  scheduling: weighted\n")
			cfg, err := Read(configPath, WithHome(paths.home))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Update.AutoCheckEnabled != test.wantCheck || cfg.Update.AutoApplyEnabled != test.wantApply {
				t.Fatalf("Update = %#v, want check %t, apply %t", cfg.Update, test.wantCheck, test.wantApply)
			}
			if err := Write(configPath, cfg, WithHome(paths.home)); err != nil {
				t.Fatal(err)
			}
			reloaded, err := Read(configPath, WithHome(paths.home))
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Update.AutoCheckEnabled != test.wantCheck || reloaded.Update.AutoApplyEnabled != test.wantApply {
				t.Fatalf("reloaded Update = %#v, want check %t, apply %t", reloaded.Update, test.wantCheck, test.wantApply)
			}
		})
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
		Path:            configPath,
		APIVersion:      APIVersion,
		Kind:            Kind,
		Env:             "dev",
		LogLevel:        "debug",
		LogMaxSizeBytes: &logMaxSizeBytes,
		LogMaxBackups:   &logMaxBackups,
		GitHubToken:     "github_test_token",
		APIToken:        "detent_test_api_token",
		Port:            &port,
		Global: Settings{
			MaxConcurrentAgents: 3,
			RateWindowPacing:    workflowconfig.DefaultRateWindowPacing(),
			Scheduling:          SchedulingStrict,
			AgentPools: []AgentPool{
				{Name: "code", MaxConcurrentAgents: 5},
				{Name: "video", MaxConcurrentAgents: 10, BurstTo: 15, Scheduling: SchedulingRoundRobin},
			},
			FairShare: map[string]any{"half_life": "30m"},
			Startup:   map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
			Cache:     defaultSettings().Cache,
			Memory:    defaultSettings().Memory,
			IO:        defaultSettings().IO,
			CPU:       defaultSettings().CPU,
			Identity: Identity{
				Name:          "release-captain",
				GitHubLogin:   "detent-bot",
				OwnershipMode: "field",
				OwnerField:    "Owner",
			},
		},
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{cfg.GitHubToken, cfg.APIToken, "not-a-config-secret"} {
		if strings.Contains(string(raw), credential) {
			t.Fatal("JSON configuration contains a credential")
		}
	}
	var public Config
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	if public.GitHubToken != "" || public.APIToken != "" || public.DashboardAccess.Token != "" {
		t.Fatal("JSON configuration includes credential fields")
	}
	if public.LogLevel != cfg.LogLevel || public.Global.MaxConcurrentAgents != cfg.Global.MaxConcurrentAgents || !reflect.DeepEqual(public.Projects, cfg.Projects) {
		t.Fatal("JSON configuration changed non-credential settings")
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
	if got.Port == nil || *got.Port != *cfg.Port {
		t.Fatalf("Port = %v, want %d", got.Port, *cfg.Port)
	}
}

func TestWritePreservesCommentsAndKeyOrder(t *testing.T) {
	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	writeFile(t, path, "apiVersion: detent/v1\nkind: GlobalConfig\n# Host name.\ninstance_name: build-host\nglobal:\n  max_concurrent_agents: 8\n  scheduling: weighted\n# Update policy.\nupdate:\n  auto_check_enabled: true\n")
	cfg, err := Read(path, WithHome(paths.home))
	if err != nil {
		t.Fatal(err)
	}
	cfg.InstanceName = "new-host"
	if err := Write(path, cfg, WithHome(paths.home)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Host name.", "# Update policy.", "instance_name: new-host"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %q in %s", want, raw)
		}
	}
	if strings.Index(string(raw), "update:") < strings.Index(string(raw), "global:") {
		t.Fatalf("update moved before global: %s", raw)
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
}

func TestReadReportsAgentPoolValidationErrors(t *testing.T) {
	paths := createProjectFiles(t)
	base := validYAML(paths, nil)
	withPools := func(pools string) string {
		return strings.Replace(base, "  scheduling: weighted\n", "  scheduling: weighted\n"+pools, 1)
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
			},
		},
		{
			name: "invalid shapes",
			raw: `apiVersion: detent/v2
kind: SomethingElse
instance_name: []
global:
  max_concurrent_agents: 0
  scheduling: random
  fair_share: []
  startup:
    jitter_seconds: -1
    max_spawn_per_second: 0
`,
			want: []string{
				"apiVersion: must equal detent/v1",
				"kind: must equal GlobalConfig",
				"instance_name: must be a string",
				"global.max_concurrent_agents: must be a positive integer",
				"global.scheduling: must be one of weighted, strict, round_robin, fair_share",
				"global.fair_share: must be a mapping",
				"global.startup.jitter_seconds: must be an integer greater than or equal to 0",
				"global.startup.max_spawn_per_second: must be a positive integer",
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
`,
			want: []string{
				"instance_name: must be a single line",
			},
		},
		{
			name: "invalid host resource settings",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  memory:
    max_agent_rss_bytes: 0
    pressure_some_avg60_threshold: -1
    poll_interval_ms: fast
  io:
    pressure_full_avg10_threshold: -1
    degraded_max_concurrent_agents: -1
    poll_interval_ms: never
  cpu:
    pressure_some_avg10_threshold: false
    degraded_max_concurrent_agents: many
    poll_interval_ms: 0
    go_build_budget: -2
`,
			want: []string{
				"global.memory.max_agent_rss_bytes: must be a positive integer",
				"global.memory.pressure_some_avg60_threshold: must be a positive number",
				"global.memory.poll_interval_ms: must be a positive integer",
				"global.io.pressure_full_avg10_threshold: must be a positive number",
				"global.io.degraded_max_concurrent_agents: must be an integer greater than or equal to 0",
				"global.io.poll_interval_ms: must be a positive integer",
				"global.cpu.pressure_some_avg10_threshold: must be a positive number",
				"global.cpu.degraded_max_concurrent_agents: must be an integer greater than or equal to 0",
				"global.cpu.poll_interval_ms: must be a positive integer",
				"global.cpu.go_build_budget: must be an integer greater than or equal to 0",
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
`,
			want: []string{
				"global.identity.name must not be blank",
				"global.identity.owner_field is required when global.identity.ownership_mode is field",
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
`,
			want: []string{
				"global.knowledge.enabled: must be a boolean",
				"global.knowledge.max_bytes: must be a positive integer",
				"global.knowledge.sources[0].name: must be a single line",
				"global.knowledge.sources[0].path: must not be blank",
			},
		},
		{
			name: "containers must have expected shapes",
			raw: `apiVersion: detent/v1
kind: GlobalConfig
global: []
`,
			want: []string{
				"global: must be a mapping",
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
		},
		{
			name: "project color mismatch",
			mutate: func(attrs map[string]any) {
				project := attrs["projects"].([]any)[0].(map[string]any)
				project["color"] = "#fffff"
			},
			opts: options{home: paths.home, relativeTo: paths.root},
		},
		{
			name: "project path expansion mismatch",
			mutate: func(attrs map[string]any) {
				project := attrs["projects"].([]any)[0].(map[string]any)
				project["workflow"] = "~/WORKFLOW.md"
			},
			opts: options{relativeTo: paths.root},
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
` + global + ``
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

func TestServiceName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{name: "detent", valid: true},
		{name: "detent.runner", valid: true},
		{name: "detent-runner-2", valid: true},
		{name: "", valid: false},
		{name: "Detent", valid: false},
		{name: "detent runner", valid: false},
		{name: "detent/runner", valid: false},
		{name: ".detent", valid: false},
		{name: "-detent", valid: false},
		{name: "detent.", valid: false},
		{name: "detent..runner", valid: false},
		{name: strings.Repeat("a", 65), valid: false},
	} {
		if got := ValidServiceName(test.name); got != test.valid {
			t.Errorf("ValidServiceName(%q) = %v, want %v", test.name, got, test.valid)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	for _, test := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "", want: ""},
		{value: "service_name: detent.runner\n", want: "detent.runner"},
		{value: "service_name: Bad Name\n", wantErr: true},
		{value: "service_name: [a]\n", wantErr: true},
	} {
		raw := "apiVersion: detent/v1\nkind: GlobalConfig\n" + test.value + "global:\n  scheduling: weighted\n  max_concurrent_agents: 1\n"
		cfg, err := Parse([]byte(raw), path)
		if test.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) accepted an invalid service name", test.value)
			}
			continue
		}
		if err != nil || cfg.ServiceName != test.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", test.value, cfg.ServiceName, err, test.want)
		}
	}
}
