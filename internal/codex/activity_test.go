package codex

import "testing"

func TestUpdateFromMessageEmitsToolActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		method           string
		params           string
		wantType         UpdateType
		wantTool         string
		wantContent      string
		wantErrorBody    string
		wantErrorMessage string
	}{
		{
			name:        "command starts",
			method:      "item/started",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"go test ./...","status":"inProgress","commandActions":[],"cwd":"/tmp"},"startedAtMs":1}`,
			wantType:    UpdateToolStarted,
			wantTool:    "commandExecution",
			wantContent: "go test ./...",
		},
		{
			name:        "command output streams",
			method:      "item/commandExecution/outputDelta",
			params:      `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"ok package"}`,
			wantType:    UpdateToolOutput,
			wantTool:    "command",
			wantContent: "ok package",
		},
		{
			name:        "mcp result completes",
			method:      "item/completed",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-2","type":"mcpToolCall","server":"github","tool":"get_issue","arguments":{},"result":{"content":[{"type":"text","text":"issue body"}]},"status":"completed"}}`,
			wantType:    UpdateToolCompleted,
			wantTool:    "github/get_issue",
			wantContent: `{"content":[{"type":"text","text":"issue body"}]}`,
		},
		{
			name:        "mcp start preserves attempted arguments",
			method:      "item/started",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-3","type":"mcpToolCall","server":"codex_apps","tool":"github.create_pull_request","arguments":{"repository_full_name":"acme/widgets","head":"detent/widgets-18"},"result":null,"status":"inProgress"}}`,
			wantType:    UpdateToolStarted,
			wantTool:    "codex_apps/github.create_pull_request",
			wantContent: `{"repository_full_name":"acme/widgets","head":"detent/widgets-18"}`,
		},
		{
			name:             "failed mcp result surfaces structured error instead of null",
			method:           "item/completed",
			params:           `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-3","type":"mcpToolCall","server":"codex_apps","tool":"github.create_pull_request","arguments":{"repository_full_name":"acme/widgets","head":"detent/widgets-18"},"result":null,"error":{"message":"HTTP 502: upstream unavailable"},"status":"failed"}}`,
			wantType:         UpdateToolCompleted,
			wantTool:         "codex_apps/github.create_pull_request",
			wantContent:      "HTTP 502: upstream unavailable",
			wantErrorBody:    `{"message":"HTTP 502: upstream unavailable"}`,
			wantErrorMessage: "HTTP 502: upstream unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			update, ok, err := updateFromMessage(notificationMessage(t, tt.method, tt.params))
			if err != nil {
				t.Fatalf("updateFromMessage() error = %v", err)
			}
			if !ok {
				t.Fatal("updateFromMessage() ok = false, want true")
			}
			if update.Type != tt.wantType || update.Tool != tt.wantTool || update.Delta != tt.wantContent ||
				update.BackendErrorBody != tt.wantErrorBody || update.BackendErrorMessage != tt.wantErrorMessage {
				t.Fatalf("update = %#v, want type %q tool %q content %q", update, tt.wantType, tt.wantTool, tt.wantContent)
			}
		})
	}
}
