package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestAgentPoolsFixModesAndPreservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		input       string
		contention  bool
		wantApplied bool
		wantOutput  string
	}{
		{
			name:       "dry run",
			args:       []string{"--dry-run"},
			contention: true,
			wantOutput: "Dry run; no files changed.",
		},
		{
			name:       "interactive decline",
			input:      "no\n",
			contention: true,
			wantOutput: "Agent-pool fix cancelled; no files changed.",
		},
		{
			name:        "interactive confirmation",
			input:       "yes\n",
			contention:  true,
			wantApplied: true,
			wantOutput:  "global config reload will take effect without a restart",
		},
		{
			name:        "non-interactive confirmation",
			args:        []string{"--yes"},
			contention:  true,
			wantApplied: true,
			wantOutput:  "global config reload will take effect without a restart",
		},
		{
			name:       "no contention",
			args:       []string{"--yes"},
			wantOutput: "no cross-class pool contention in 7d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newAgentPoolsFixFixture(t, tt.contention)
			before, err := os.ReadFile(fixture.configPath)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd := newAgentPoolsFixCommandWithDeps(&fixture.configPath, defaultOptions(), fixture.deps)
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetIn(strings.NewReader(tt.input))
			cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
				stdoutTTY: func() bool { return true },
			}))
			cmd.SetArgs(tt.args)
			if err := cmd.ExecuteContext(cmd.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("stdout = %q, want containing %q", stdout.String(), tt.wantOutput)
			}
			if strings.Contains(stdout.String(), "secret-token") {
				t.Fatalf("stdout exposed unrelated config secret: %s", stdout.String())
			}

			after, err := os.ReadFile(fixture.configPath)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if !tt.wantApplied {
				if !bytes.Equal(after, before) {
					t.Fatalf("global config changed without application:\n%s", after)
				}
				return
			}
			for _, preserved := range []string{
				"# fleet comment",
				"log_level: debug # keep this setting",
				"max_concurrent_agents: 5 # sized for this box",
			} {
				if !strings.Contains(string(after), preserved) {
					t.Fatalf("global config lost %q:\n%s", preserved, after)
				}
			}
			cfg, err := globalconfig.Read(fixture.configPath)
			if err != nil {
				t.Fatalf("globalconfig.Read() error = %v\n%s", err, after)
			}
			if len(cfg.Global.AgentPools) != 2 ||
				cfg.Global.AgentPools[0].Name != "code" ||
				cfg.Global.AgentPools[0].MaxConcurrentAgents != 5 ||
				cfg.Global.AgentPools[1].Name != "cloud" ||
				cfg.Global.AgentPools[1].MaxConcurrentAgents != 10 {
				t.Fatalf("agent pools = %#v", cfg.Global.AgentPools)
			}
			if len(cfg.Projects) != 2 || cfg.Projects[0].ID != "detent" ||
				cfg.Projects[0].Pool != "code" || cfg.Projects[1].ID != "video" ||
				cfg.Projects[1].Pool != "cloud" {
				t.Fatalf("projects = %#v, want original order and class pools", cfg.Projects)
			}
			info, err := os.Stat(fixture.configPath)
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != configFileMode {
				t.Fatalf("mode = %04o, want %04o", info.Mode().Perm(), configFileMode)
			}
		})
	}
}

func TestAgentPoolsFixDeclinesExistingPools(t *testing.T) {
	t.Parallel()

	fixture := newAgentPoolsFixFixture(t, true)
	runAgentPoolsFixTestCommand(t, fixture, "--yes")

	output := runAgentPoolsFixTestCommand(t, fixture, "--yes")
	if !strings.Contains(output, "global.agent_pools is already configured") {
		t.Fatalf("output = %q, want existing-pool decline", output)
	}
}

func TestAgentPoolsFixHotReloadsScheduler(t *testing.T) {
	fixture := newAgentPoolsFixFixture(t, true)
	initial, err := globalconfig.Read(fixture.configPath)
	if err != nil {
		t.Fatalf("globalconfig.Read() error = %v", err)
	}
	registry, err := buildGlobalDispatchPools(initial, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := startGlobalConfigWatcher(
		ctx,
		initial,
		&globalReloadManager{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		func(next globalconfig.Config) error {
			return applyGlobalRuntimeConfig(registry, nil, nil, next)
		},
		nil,
	)
	if done == nil {
		t.Fatal("startGlobalConfigWatcher() returned nil")
	}

	runAgentPoolsFixTestCommand(t, fixture, "--yes")

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		snapshot := registry.PoolSnapshotFor("video")
		if snapshot.Name == "cloud" && snapshot.Capacity == 10 {
			break
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("hot reload did not apply agent pools: %#v", snapshot)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for global config watcher shutdown")
	}
}

type agentPoolsFixFixture struct {
	configPath string
	deps       doctorDeps
}

func newAgentPoolsFixFixture(t *testing.T, contention bool) agentPoolsFixFixture {
	t.Helper()

	dir := t.TempDir()
	localWorkflowPath := filepath.Join(dir, "detent-WORKFLOW.md")
	cloudWorkflowPath := filepath.Join(dir, "video-WORKFLOW.md")
	for _, path := range []string{localWorkflowPath, cloudWorkflowPath} {
		if err := os.WriteFile(path, []byte("Workflow\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	configPath := filepath.Join(dir, "global.yaml")
	raw := strings.Join([]string{
		"# fleet comment",
		"apiVersion: detent/v1",
		"kind: GlobalConfig",
		"github_token: secret-token",
		"log_level: debug # keep this setting",
		"global:",
		"  max_concurrent_agents: 5 # sized for this box",
		"  scheduling: weighted",
		"projects:",
		"  - id: detent",
		"    workflow: " + localWorkflowPath,
		"    workdir: " + dir,
		"    weight: 1",
		"    priority: 0",
		"  - id: video",
		"    workflow: " + cloudWorkflowPath,
		"    workdir: " + dir,
		"    weight: 1",
		"    priority: 0",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(raw), configFileMode); err != nil {
		t.Fatalf("WriteFile(global.yaml) error = %v", err)
	}
	backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(dir, "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	if contention {
		if _, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
			ProjectID:              "video",
			WorkerType:             "implement",
			StartedAt:              now.Add(-time.Hour),
			WaitReason:             "global_capacity_full",
			CapacitySnapshotJSON:   `{"pool":"default","holders":["detent"]}`,
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

	workflows := map[string]workflowconfig.Workflow{
		localWorkflowPath: {Config: workflowconfig.Config{
			Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"},
		}},
		cloudWorkflowPath: {Config: workflowconfig.Config{
			Gate: gate.Config{Kind: gate.KindArtifact},
		}},
	}
	deps := doctorDeps{
		loadWorkflow: func(path string) (workflowconfig.Workflow, error) {
			return workflows[path], nil
		},
		openSQLiteReadOnly: openDoctorSQLiteReadOnly,
		now:                func() time.Time { return now },
	}.withDefaults()
	return agentPoolsFixFixture{configPath: configPath, deps: deps}
}

func runAgentPoolsFixTestCommand(t *testing.T, fixture agentPoolsFixFixture, args ...string) string {
	t.Helper()

	var stdout bytes.Buffer
	cmd := newAgentPoolsFixCommandWithDeps(&fixture.configPath, defaultOptions(), fixture.deps)
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
		stdoutTTY: func() bool { return true },
	}))
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(cmd.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	return stdout.String()
}
