package activity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRolloutHistoryReaderPagesCodexEvents(t *testing.T) {
	t.Parallel()

	codexRoot := t.TempDir()
	sessions := filepath.Join(codexRoot, "sessions", "2026", "07", "10")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	rollout := filepath.Join(sessions, "rollout-2026-thread-1156.jsonl")
	contents := `{"timestamp":"2026-07-10T15:00:00Z","type":"event_msg","payload":{"type":"agent_message","message":"first"}}
{"timestamp":"2026-07-10T15:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec_command","input":"go test ./..."}}
{"timestamp":"2026-07-10T15:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","output":"ok package"}}
`
	if err := os.WriteFile(rollout, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	reader := NewRolloutHistoryReader(codexRoot, t.TempDir())
	page, err := reader.Page(context.Background(), HistoryQuery{ProviderThreadID: "thread-1156", Limit: 2})
	if err != nil {
		t.Fatalf("Page() error = %v", err)
	}
	if len(page.Events) != 2 || !page.HasMore || page.Events[0].Content != "first" || page.Events[1].Kind != "tool_started" {
		t.Fatalf("first page = %#v", page)
	}

	page, err = reader.Page(context.Background(), HistoryQuery{ProviderThreadID: "thread-1156", Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("Page(second) error = %v", err)
	}
	if len(page.Events) != 1 || page.HasMore || page.Events[0].Content != "ok package" {
		t.Fatalf("second page = %#v", page)
	}
}
