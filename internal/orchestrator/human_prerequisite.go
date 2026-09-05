package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
)

func humanDependencyWaitReason(refs []connector.BlockedRef) string {
	var waiting []string
	for _, ref := range refs {
		if ref.HumanOwned && !ref.HumanCompletionReady {
			waiting = append(waiting, ref.Identifier)
		}
	}
	if len(waiting) == 0 {
		return ""
	}
	return "waiting on human prerequisite " + strings.Join(waiting, ", ") + "; closure and completion evidence required"
}

func (o *Orchestrator) attachHumanPrerequisiteTool(request *RunRequest) {
	writer, ok := o.connector.(connector.HumanPrerequisiteWriter)
	if !ok {
		return
	}
	identifier := request.Issue.Identifier
	request.AgentTools = []runner.AgentTool{{
		Name:        "ensure_human_prerequisite",
		Description: "Reuse or create one human-owned Backlog milestone and add a durable dependency to your assigned issue. Use only when this exact milestone prevents the remaining work; finish stub-testable work, independent preparation, and authorized fallbacks first. Preserve approvals. Never supply credentials or private evidence. Reuse the existing key and exact contract. This tool never authorizes publishing, deployment, or destructive actions.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title","task"],"properties":{"title":{"type":"string","minLength":1},"existing_identifier":{"type":"string"},"task":{"type":"object","additionalProperties":false,"required":["schema","key","action","owner","completion_criteria","approval_constraint"],"properties":{"schema":{"type":"integer","const":1},"key":{"type":"string","minLength":1},"action":{"type":"string","minLength":1},"owner":{"type":"string","minLength":1},"completion_criteria":{"type":"string","minLength":1},"approval_constraint":{"type":"string","minLength":1}}}}}`),
	}}
	request.AgentToolHandler = func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if call.Name != "ensure_human_prerequisite" {
			return runner.AgentToolResult{Content: "unsupported tool"}, nil
		}
		var input connector.HumanPrerequisiteRequest
		decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return runner.AgentToolResult{Content: err.Error()}, nil
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return runner.AgentToolResult{Content: "expected one prerequisite request"}, nil
		}
		result, err := writer.EnsureHumanPrerequisite(ctx, identifier, input)
		if err != nil {
			return runner.AgentToolResult{Content: err.Error()}, nil
		}
		return runner.AgentToolResult{Success: true, Content: "Depends on: " + result.Issue.Identifier + "\nPrerequisite is human-owned. Record a structured dependency blocker with human_action: null; keep the dependent lane resumable. Completion is not external-action authorization."}, nil
	}
}

func dependencyWaitTarget(issue connector.Issue, sourceState string) string {
	for _, state := range []string{sourceState, issue.State} {
		if normalizeState(state) == "merging" || normalizeState(state) == "rework" {
			return state
		}
	}
	if dependencyAutoUnblockStartedSignal(issue) {
		return autoPromoteReworkState
	}
	return "Todo"
}
