package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

type stubConversationBackend struct{ command string }

func (stubConversationBackend) RunTurn(context.Context, runnerpkg.AgentTurnRequest, runnerpkg.AgentUpdateHandler) (runnerpkg.AgentTurnResult, error) {
	return runnerpkg.AgentTurnResult{}, nil
}

func TestReadConversationConfig(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	stat := os.Stat
	builder := func(command string, _ workflowconfig.CodexOptions) (runnerpkg.AgentBackend, error) {
		return stubConversationBackend{command: command}, nil
	}
	codexSection := func(command, workspace string) *hostedConversationFileConfig {
		section := &hostedConversationFileConfig{Enabled: true, Workspace: workspace, QuestionTimeout: "2h", SettleWindow: "6h", ControlQueueSize: 8}
		section.Codex = &struct {
			Command         string                      `yaml:"command"`
			Model           string                      `yaml:"model"`
			ReasoningEffort string                      `yaml:"reasoning_effort"`
			Options         workflowconfig.CodexOptions `yaml:"options"`
		}{Command: command, Model: "gpt-6-astra", ReasoningEffort: "low"}
		return section
	}
	tests := []struct {
		name        string
		section     *hostedConversationFileConfig
		wantEnabled bool
		wantErr     bool
		check       func(t *testing.T, config any)
	}{
		{name: "absent section disables", section: nil},
		{name: "disabled section disables", section: &hostedConversationFileConfig{Enabled: false}},
		{name: "enabled without codex has no backend", section: &hostedConversationFileConfig{Enabled: true}, wantEnabled: true},
		{name: "invalid timeout", section: &hostedConversationFileConfig{Enabled: true, QuestionTimeout: "soon"}, wantErr: true},
		{name: "invalid settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "eventually"}, wantErr: true},
		{name: "negative settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "-1h"}, wantErr: true},
		{name: "codex without workspace", section: codexSection("codex", ""), wantErr: true},
		{name: "codex with missing workspace", section: codexSection("codex", filepath.Join(workspace, "missing")), wantErr: true},
		{name: "codex configured", section: codexSection("", workspace), wantEnabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, enabled, err := readConversationConfig(test.section, builder, stat)
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
			if !enabled {
				return
			}
			if test.section.Codex == nil {
				if config.Backend != nil {
					t.Fatal("expected no backend without a codex section")
				}
				return
			}
			backend, ok := config.Backend.(stubConversationBackend)
			if !ok || backend.command != "codex" {
				t.Fatalf("backend = %#v, want the default codex command", config.Backend)
			}
			if config.SettleWindow != 6*time.Hour {
				t.Fatalf("settle window = %s, want 6h", config.SettleWindow)
			}
			if config.Workspace != workspace || config.Model != "gpt-6-astra" || config.ReasoningEffort != "low" || config.QuestionTimeout != 2*time.Hour || config.ControlQueueSize != 8 {
				t.Fatalf("unexpected config: %+v", config)
			}
		})
	}
}

func TestReadConversationConfigBackendFailure(t *testing.T) {
	t.Parallel()
	section := &hostedConversationFileConfig{Enabled: true, Workspace: t.TempDir()}
	section.Codex = &struct {
		Command         string                      `yaml:"command"`
		Model           string                      `yaml:"model"`
		ReasoningEffort string                      `yaml:"reasoning_effort"`
		Options         workflowconfig.CodexOptions `yaml:"options"`
	}{}
	want := errors.New("boom")
	_, _, err := readConversationConfig(section, func(string, workflowconfig.CodexOptions) (runnerpkg.AgentBackend, error) { return nil, want }, os.Stat)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestReadHostedConversationConfigFromFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hosted.yaml")
	content := "organization_id: org_test\nworkos_organization_id: org_workos\npublic_url: https://hub.example\nworkos:\n  client_id: client\nconversation:\n  enabled: true\n  question_timeout: 1h\n  settle_window: 12h\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	config, enabled, err := readHostedConversationConfig(path, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled || !config.Enabled || config.QuestionTimeout != time.Hour || config.SettleWindow != 12*time.Hour {
		t.Fatalf("config = %+v enabled = %v", config, enabled)
	}
	if _, enabled, err := readHostedConversationConfig("", nil); err != nil || enabled {
		t.Fatalf("empty path: enabled = %v err = %v", enabled, err)
	}
	if _, _, err := readHostedConversationConfig(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
