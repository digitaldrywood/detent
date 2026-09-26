package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestReadConversationConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		section     *hostedConversationFileConfig
		wantEnabled bool
		wantErr     bool
		want        hubserver.ConversationConfig
	}{
		{name: "absent section disables", section: nil},
		{name: "disabled section disables", section: &hostedConversationFileConfig{Enabled: false}},
		{name: "enabled with defaults", section: &hostedConversationFileConfig{Enabled: true}, wantEnabled: true, want: hubserver.ConversationConfig{Enabled: true}},
		{
			name:        "enabled with values",
			section:     &hostedConversationFileConfig{Enabled: true, Model: " gpt-6-astra ", QuestionTimeout: "2h", SettleWindow: "6h", ControlQueueSize: 8},
			wantEnabled: true,
			want:        hubserver.ConversationConfig{Enabled: true, Model: "gpt-6-astra", QuestionTimeout: 2 * time.Hour, SettleWindow: 6 * time.Hour, ControlQueueSize: 8},
		},
		{name: "invalid timeout", section: &hostedConversationFileConfig{Enabled: true, QuestionTimeout: "soon"}, wantErr: true},
		{name: "invalid settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "eventually"}, wantErr: true},
		{name: "negative settle window", section: &hostedConversationFileConfig{Enabled: true, SettleWindow: "-1h"}, wantErr: true},
		{name: "negative queue size", section: &hostedConversationFileConfig{Enabled: true, ControlQueueSize: -1}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, enabled, err := readConversationConfig(test.section)
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
			if config != test.want {
				t.Fatalf("config = %+v, want %+v", config, test.want)
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
	config, enabled, err := readHostedConversationConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled || !config.Enabled || config.QuestionTimeout != time.Hour || config.SettleWindow != 12*time.Hour {
		t.Fatalf("config = %+v enabled = %v", config, enabled)
	}
	if _, enabled, err := readHostedConversationConfig(""); err != nil || enabled {
		t.Fatalf("empty path: enabled = %v err = %v", enabled, err)
	}
	if _, _, err := readHostedConversationConfig(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
