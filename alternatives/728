package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

type stubConversationBackend struct{ command string }

func (stubConversationBackend) RunTurn(context.Context, runnerpkg.AgentTurnRequest, runnerpkg.AgentUpdateHandler) (runnerpkg.AgentTurnResult, error) {
	return runnerpkg.AgentTurnResult{}, nil
}

func TestReadConversationConfig(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	file := filepath.Join(workspace, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	builder := func(command string, _ workflowconfig.CodexOptions, _ string) (runnerpkg.AgentBackend, error) {
		return stubConversationBackend{command: command}, nil
	}
	codexSection := func(command, workspace string) *hostedConversationFileConfig {
		return &hostedConversationFileConfig{
			Enabled: true, Model: " gpt-6-astra ", ReasoningEffort: " low ", Workspace: workspace,
			QuestionTimeout: "2h", SettleWindow: "6h", ControlQueueSize: 8,
			Codex: &hostedConversationCodexFileConfig{Command: command},
		}
	}
	tests := []struct {
		name        string
		section     *hostedConversationFileConfig
		wantEnabled bool
		wantErr     bool
		want        hubserver.ConversationConfig
		wantCommand string
	}{
		{name: "absent section disables", section: nil},
		{name: "disabled section disables", section: &hostedConversationFileConfig{Enabled: false}},
		{name: "enabled with defaults", section: &hostedConversationFileConfig{Enabled: true}, wantEnabled: true, want: hubserver.ConversationConfig{Enabled: true}},
		{
			name:        "enabled with values and no backend",
			section:     &hostedConversationFileConfig{Enabled: true, Model: " gpt-6-astra ", ReasoningEffort: "medium", QuestionTimeout: "2h", SettleWindow: "6h", ControlQueueSize: 8},
			wantEnabled: true,
			want:        hubserver.ConversationConfig{Enabled: true, Model: "gpt-6-astra", ReasoningEffort: "medium", QuestionTimeout: 2 * time.Hour, SettleWindow: 6 * time.Hour, ControlQueueSize: 8},
		},
		{name: "invalid timeout", section: &hostedConversationFileConfig{Enabled: true, QuestionTimeout: "soon"}, wantErr: true},
		{name: "invalid settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "eventually"}, wantErr: true},
		{name: "negative settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "-1h"}, wantErr: true},
		{name: "negative queue size", section: &hostedConversationFileConfig{Enabled: true, ControlQueueSize: -1}, wantErr: true},
		{name: "unsupported reasoning effort", section: &hostedConversationFileConfig{Enabled: true, ReasoningEffort: "xhigh"}, wantErr: true},
		{name: "auto is not a configured reasoning effort", section: &hostedConversationFileConfig{Enabled: true, ReasoningEffort: "auto"}, wantErr: true},
		{name: "misspelled reasoning effort", section: &hostedConversationFileConfig{Enabled: true, ReasoningEffort: "hihg"}, wantErr: true},
		{name: "codex without workspace", section: codexSection("codex", ""), wantErr: true},
		{name: "codex with missing workspace", section: codexSection("codex", filepath.Join(workspace, "missing")), wantErr: true},
		{name: "codex with a file as workspace", section: codexSection("codex", file), wantErr: true},
		{
			name: "codex with the default command", section: codexSection("", workspace), wantEnabled: true, wantCommand: "codex app-server",
			want: hubserver.ConversationConfig{Enabled: true, Model: "gpt-6-astra", ReasoningEffort: "low", Workspace: workspace, QuestionTimeout: 2 * time.Hour, SettleWindow: 6 * time.Hour, ControlQueueSize: 8},
		},
		{
			name: "codex with a named command", section: codexSection(" /opt/codex ", workspace), wantEnabled: true, wantCommand: "/opt/codex",
			want: hubserver.ConversationConfig{Enabled: true, Model: "gpt-6-astra", ReasoningEffort: "low", Workspace: workspace, QuestionTimeout: 2 * time.Hour, SettleWindow: 6 * time.Hour, ControlQueueSize: 8},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, enabled, err := readConversationConfig(test.section, builder, os.Stat, func(string) string { return "" })
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if enabled != test.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, test.wantEnabled)
			}
			backend := config.Backend
			config.Backend = nil
			if !reflect.DeepEqual(config, test.want) {
				t.Fatalf("config = %+v, want %+v", config, test.want)
			}
			if test.wantCommand == "" {
				if backend != nil {
					t.Fatalf("backend = %#v, want none without a codex section", backend)
				}
				return
			}
			stub, ok := backend.(stubConversationBackend)
			if !ok || stub.command != test.wantCommand {
				t.Fatalf("backend = %#v, want command %q", backend, test.wantCommand)
			}
		})
	}
}

func TestReadConversationConfigBackendFailure(t *testing.T) {
	t.Parallel()
	section := &hostedConversationFileConfig{Enabled: true, Workspace: t.TempDir(), Codex: &hostedConversationCodexFileConfig{}}
	want := errors.New("boom")
	_, _, err := readConversationConfig(section, func(string, workflowconfig.CodexOptions, string) (runnerpkg.AgentBackend, error) { return nil, want }, os.Stat, func(string) string { return "" })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestReadConversationConfigSelectsOpenAIFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name      string
		codex     bool
		workspace string
		wantErr   bool
	}{
		{name: "Luna only", wantErr: true},
		{name: "missing Codex workspace stays lazy", codex: true, workspace: "missing", wantErr: true},
		{name: "configured Codex serves model choices", codex: true, workspace: t.TempDir()},
	} {
		t.Run(test.name, func(t *testing.T) {
			section := &hostedConversationFileConfig{Enabled: true, Workspace: test.workspace}
			if test.codex {
				section.Codex = &hostedConversationCodexFileConfig{}
			}
			builds := 0
			config, enabled, err := readConversationConfig(section, func(command string, _ workflowconfig.CodexOptions, workspace string) (runnerpkg.AgentBackend, error) {
				builds++
				if workspace != test.workspace {
					t.Fatalf("workspace = %q, want %q", workspace, test.workspace)
				}
				return stubConversationBackend{command: command}, nil
			}, os.Stat, func(string) string { return "test-key" })
			if err != nil || !enabled {
				t.Fatalf("config enabled=%t error=%v", enabled, err)
			}
			if _, ok := config.Backend.(*genkitbackend.Backend); !ok || config.Model != genkitbackend.Model || config.ReasoningEffort != "low" || builds != 0 {
				t.Fatalf("OpenAI config = %+v builds=%d", config, builds)
			}
			for range 2 {
				backend, workspace, err := config.ModelBackend()
				if test.wantErr {
					if err == nil {
						t.Fatal("expected unavailable model backend")
					}
					continue
				}
				stub, ok := backend.(stubConversationBackend)
				if err != nil || !ok || stub.command != defaultCoordinatorCodex || workspace != test.workspace || builds != 1 {
					t.Fatalf("model backend=%#v workspace=%q builds=%d error=%v", backend, workspace, builds, err)
				}
			}
		})
	}
}

func TestReadHostedConversationConfigFromFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hosted.yaml")
	content := "organization_id: org_test\nworkos_organization_id: org_workos\npublic_url: https://hub.example\nworkos:\n  client_id: client\nconversation:\n  enabled: true\n  question_timeout: 1h\n  settle_window: 12h\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	config, enabled, err := readHostedConversationConfigWithEnv(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled || !config.Enabled || config.QuestionTimeout != time.Hour || config.SettleWindow != 12*time.Hour || config.Backend != nil {
		t.Fatalf("config = %+v enabled = %v", config, enabled)
	}
	if _, enabled, err := readHostedConversationConfigWithEnv("", func(string) string { return "" }); err != nil || enabled {
		t.Fatalf("empty path: enabled = %v err = %v", enabled, err)
	}
	if _, _, err := readHostedConversationConfigWithEnv(filepath.Join(t.TempDir(), "absent"), func(string) string { return "" }); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
