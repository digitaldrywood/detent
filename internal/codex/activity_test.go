package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

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
		wantOmitted      []string
		wantMaxBytes     int
		contentFromError bool
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
		{
			name:             "failed mcp result sanitizes raw diagnostic fallback",
			method:           "item/completed",
			params:           `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-4","type":"mcpToolCall","server":"codex_apps","tool":"github.create_pull_request","arguments":{"body":"Fixes #1775","credential":"argument-secret","message":"argument message"},"result":null,"error":null,"diagnostic":{"code":"mcp_elicitation_rejected","detail":{"message":"user rejected MCP tool call","httpStatus":403,"authorization":"Bearer secret","body":"private PR body"},"credentials":"connector-secret"},"status":"failed"}}`,
			wantType:         UpdateToolCompleted,
			wantTool:         "codex_apps/github.create_pull_request",
			wantContent:      `{"code":"mcp_elicitation_rejected","message":"user rejected MCP tool call","http_status":403}`,
			wantErrorMessage: `{"code":"mcp_elicitation_rejected","message":"user rejected MCP tool call","http_status":403}`,
			wantOmitted:      []string{"Fixes #1775", "argument-secret", "argument message", "Bearer secret", "private PR body", "connector-secret", "authorization", "credentials"},
			wantMaxBytes:     2048,
		},
		{
			name:             "failed mcp result bounds raw diagnostic fallback",
			method:           "item/completed",
			params:           `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-5","type":"mcpToolCall","server":"codex_apps","tool":"github.create_pull_request","result":null,"error":null,"diagnostic":{"code":"connector_failure","message":"` + strings.Repeat("x", 4096) + `","statusCode":502,"body":"private connector payload"},"status":"failed"}}`,
			wantType:         UpdateToolCompleted,
			wantTool:         "codex_apps/github.create_pull_request",
			wantErrorMessage: `{"code":"connector_failure","message":"` + strings.Repeat("x", 64),
			wantOmitted:      []string{"private connector payload"},
			wantMaxBytes:     2048,
			contentFromError: true,
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
			if update.Type != tt.wantType || update.Tool != tt.wantTool || (!tt.contentFromError && update.Delta != tt.wantContent) ||
				update.BackendErrorBody != tt.wantErrorBody || (tt.wantMaxBytes == 0 && update.BackendErrorMessage != tt.wantErrorMessage) {
				t.Fatalf("update = %#v, want type %q tool %q content %q", update, tt.wantType, tt.wantTool, tt.wantContent)
			}
			if tt.contentFromError && update.Delta != update.BackendErrorMessage {
				t.Fatalf("Delta = %q, want sanitized BackendErrorMessage %q", update.Delta, update.BackendErrorMessage)
			}
			if tt.wantMaxBytes > 0 {
				if len(update.BackendErrorMessage) > tt.wantMaxBytes {
					t.Fatalf("BackendErrorMessage bytes = %d, want <= %d", len(update.BackendErrorMessage), tt.wantMaxBytes)
				}
				if !strings.HasPrefix(update.BackendErrorMessage, tt.wantErrorMessage) {
					t.Fatalf("BackendErrorMessage = %q, want prefix %q", update.BackendErrorMessage, tt.wantErrorMessage)
				}
				if !json.Valid([]byte(update.BackendErrorMessage)) {
					t.Fatalf("BackendErrorMessage = %q, want valid JSON", update.BackendErrorMessage)
				}
			}
			for _, omitted := range tt.wantOmitted {
				if strings.Contains(update.BackendErrorMessage, omitted) {
					t.Fatalf("BackendErrorMessage = %q, want %q omitted", update.BackendErrorMessage, omitted)
				}
			}
		})
	}
}
