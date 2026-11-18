package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
