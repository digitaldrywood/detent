package codex

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestUpdateFromMessageEmitsToolActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		method           string
		params           string
		wantType         UpdateType
		wantTool         string
		wantCommand      string
		wantActions      []runner.NativeCommandAction
		wantCWD          string
		wantExitCode     *int
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
			wantCommand: "go test ./...",
			wantCWD:     "/tmp",
			wantContent: "go test ./...",
		},
		{
			name:        "native command actions preserve input and original command",
			method:      "item/started",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"/bin/zsh -lc 'go test ./...'","commandActions":[{"type":"unknown","command":"go test ./..."}],"status":"inProgress"}}`,
			wantType:    UpdateToolStarted,
			wantTool:    "commandExecution",
			wantCommand: "/bin/zsh -lc 'go test ./...'",
			wantContent: "go test ./...",
			wantActions: []runner.NativeCommandAction{{Type: "unknown", Command: "go test ./..."}},
		},
		{
			name:        "mixed native actions retain every command",
			method:      "item/started",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"/bin/zsh -lc 'cat AGENTS.md; go test ./...'","commandActions":[{"type":"read","command":"cat AGENTS.md","name":"AGENTS.md","path":"AGENTS.md"},{"type":"unknown","command":"go test ./..."}],"status":"inProgress"}}`,
			wantType:    UpdateToolStarted,
			wantTool:    "commandExecution",
			wantCommand: "/bin/zsh -lc 'cat AGENTS.md; go test ./...'",
			wantContent: "cat AGENTS.md; go test ./...",
			wantActions: []runner.NativeCommandAction{{Type: "read", Command: "cat AGENTS.md", Name: "AGENTS.md", Path: "AGENTS.md"}, {Type: "unknown", Command: "go test ./..."}},
		},
		{
			name:        "incomplete native actions stay opaque",
			method:      "item/started",
			params:      `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"/bin/zsh -lc 'cat AGENTS.md; go test ./...'","commandActions":[{"type":"read","command":"cat AGENTS.md"},{"type":"unknown"}],"status":"inProgress"}}`,
			wantType:    UpdateToolStarted,
			wantTool:    "commandExecution",
			wantCommand: "/bin/zsh -lc 'cat AGENTS.md; go test ./...'",
			wantContent: "/bin/zsh -lc 'cat AGENTS.md; go test ./...'",
			wantActions: []runner.NativeCommandAction{{Type: "read", Command: "cat AGENTS.md"}, {Type: "unknown"}},
		},
		{
			name:     "native read without command retains path and type",
			method:   "item/started",
			params:   `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"private wrapper","cwd":"/private/workspace/nested","commandActions":[{"type":"read","name":"CLAUDE.md","path":"CLAUDE.md"},{"type":"futureOpaque","name":"private name"}],"status":"inProgress"}}`,
			wantType: UpdateToolStarted, wantTool: "commandExecution", wantCommand: "private wrapper", wantContent: "private wrapper", wantCWD: "/private/workspace/nested",
			wantActions: []runner.NativeCommandAction{{Type: "read", Name: "CLAUDE.md", Path: "CLAUDE.md"}, {Type: "futureOpaque", Name: "private name"}},
		},
		{
			name:         "failed command retains command and exit code",
			method:       "item/completed",
			params:       `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"git push origin HEAD && exit 19","commandActions":[{"type":"unknown","command":"git push origin HEAD"},{"type":"unknown","command":"exit 19"}],"status":"failed","exitCode":19,"aggregatedOutput":"branch updated; later assertion failed"}}`,
			wantType:     UpdateToolCompleted,
			wantTool:     "commandExecution",
			wantCommand:  "git push origin HEAD && exit 19",
			wantExitCode: intPointer(19),
			wantContent:  "branch updated; later assertion failed",
			wantActions:  []runner.NativeCommandAction{{Type: "unknown", Command: "git push origin HEAD"}, {Type: "unknown", Command: "exit 19"}},
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
		{
			name:             "failed mcp result skips compound sensitive containers",
			method:           "item/completed",
			params:           `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-6","type":"mcpToolCall","server":"codex_apps","tool":"github.create_pull_request","result":null,"error":null,"diagnostic":{"code":"connector_failure","requestBody":{"message":"private request"},"authTokenData":{"message":"private token"},"clientSecretValue":{"message":"private secret"},"credentialEnvelope":{"message":"private credential"}},"status":"failed"}}`,
			wantType:         UpdateToolCompleted,
			wantTool:         "codex_apps/github.create_pull_request",
			wantContent:      `{"code":"connector_failure"}`,
			wantErrorMessage: `{"code":"connector_failure"}`,
			wantOmitted:      []string{"private request", "private token", "private secret", "private credential"},
			wantMaxBytes:     2048,
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
			if len(update.NativeActions) != len(tt.wantActions) || (len(tt.wantActions) > 0 && !reflect.DeepEqual(update.NativeActions, tt.wantActions)) {
				t.Fatalf("native actions = %+v, want %+v", update.NativeActions, tt.wantActions)
			}
			agent := agentUpdateFromCodex(update)
			if !reflect.DeepEqual(agent.NativeActions, update.NativeActions) || agent.CWD != update.CWD {
				t.Fatal("native action evidence lost at backend boundary")
			}
			if update.CWD != tt.wantCWD {
				t.Fatalf("cwd = %q, want %q", update.CWD, tt.wantCWD)
			}
			if update.Command != tt.wantCommand || !equalIntPointers(update.ExitCode, tt.wantExitCode) {
				t.Fatalf("command evidence = command %q exit %#v, want command %q exit %#v", update.Command, update.ExitCode, tt.wantCommand, tt.wantExitCode)
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

func intPointer(value int) *int {
	return &value
}

func equalIntPointers(left *int, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func BenchmarkToolLifecycleNativeActions(b *testing.B) {
	for _, size := range []int{32, 8100} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			command := "go test " + strings.Repeat("x", size)
			params, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"id": "tool", "type": "commandExecution", "command": "/bin/zsh -lc '" + command + "'", "commandActions": []map[string]string{{"type": "unknown", "command": command}}, "status": "inProgress"}})
			message := Message{Method: "item/started", Params: params}
			b.ReportAllocs()
			for b.Loop() {
				if _, ok, err := toolLifecycleUpdate(message); !ok || err != nil {
					b.Fatalf("tool lifecycle: ok=%v err=%v", ok, err)
				}
			}
		})
	}
}
