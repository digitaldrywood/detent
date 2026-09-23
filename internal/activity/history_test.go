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

func TestRolloutHistoryWorkerFallback(t *testing.T) {
	for _, tc := range []struct {
		name, profile, configured string
		host, worker              bool
		want                      string
	}{
		{name: "legacy", host: true, want: "host"},
		{name: "worker", profile: ".detent-worker", worker: true, want: "worker"},
		{name: "launchd", profile: ".detent-launchd", worker: true, want: "worker"},
		{name: "prefer worker", profile: ".detent-worker", host: true, worker: true, want: "worker"},
		{name: "explicit profile legacy", configured: ".detent-worker", host: true, want: "host"},
		{name: "explicit launchd", profile: ".detent-launchd", configured: ".detent-launchd", host: true, worker: true, want: "worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, item := range []struct {
				enabled      bool
				dir, content string
			}{{tc.host, filepath.Join(home, "sessions"), "host"}, {tc.worker, filepath.Join(home, tc.profile, "sessions"), "worker"}} {
				if !item.enabled {
					continue
				}
				if err := os.MkdirAll(item.dir, 0700); err != nil {
					t.Fatal(err)
				}
				body := `{"type":"event_msg","payload":{"type":"agent_message","message":"` + item.content + `"}}`
				if err := os.WriteFile(filepath.Join(item.dir, "rollout-thread.jsonl"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reader := NewRolloutHistoryReader(filepath.Join(home, tc.configured), t.TempDir())
			page, err := reader.Page(t.Context(), HistoryQuery{ProviderThreadID: "thread"})
			if err != nil || len(page.Events) != 1 || page.Events[0].Content != tc.want {
				t.Fatalf("page=%+v err=%v", page, err)
			}
		})
	}
}
