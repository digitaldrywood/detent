package hubserver

import (
	"sync"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

const (
	coordinatorToolFindTools = "find_tools"
	coordinatorToolCallTool  = "call_tool"
)

func coordinatorCatalogTools() []runner.AgentTool {
	return []runner.AgentTool{
		coordinatorTool(coordinatorToolFindTools,
			"Search every Detent tool the current user's role and grants allow, the same catalog their MCP connection lists: work items, comments, changes, projects, runners, members, billing, workspaces and settings. With a query or toolset, returns matching tools with input schemas; with neither, returns every tool name. Run one with call_tool.",
			`{"type":"object","properties":{"query":{"type":"string","maxLength":256,"description":"Words matched against tool names and descriptions"},"toolset":{"type":"string","maxLength":64,"description":"Return every tool in this toolset"}},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolCallTool,
			"Run a tool found with find_tools, passing arguments that match its input schema. project_id defaults to this conversation's project and request_id is generated when omitted. When a tool takes expected_revision, read the item first and pass its current revision. Read-only tools return their result. Tools that change state are proposed as a card under the user's confirmation preference; never claim a change applied before its result is posted.",
			`{"type":"object","required":["name"],"properties":{"name":{"type":"string","maxLength":128},"arguments":{"type":"object"}},"additionalProperties":false}`),
	}
}

func coordinatorCatalogExcluded(name string) bool {
	switch name {
	case coordinatorToolFindTools, coordinatorToolCallTool, string(chat.ActionStopRun), string(chat.ActionFileIssue), string(chat.ActionSetPriority), operatortool.ActionResult, operatortool.ConnectionInfo, "get_operator_chat", "post_operator_chat", "post_conversation_command", "stream_conversation_events":
		return true
	}
	return coordinatorToolNames()[name]
}

var coordinatorToolNames = sync.OnceValue(func() map[string]bool {
	names := map[string]bool{}
	for _, tool := range (&coordinatorToolset{}).tools() {
		names[tool.Name] = true
	}
	return names
})

// coordinatorCatalogWrite reports whether a confirmed chat action runs a
// catalog tool through the operator executor rather than a coordinator tool.
func coordinatorCatalogWrite(name string) bool {
	definition, ok := operatortool.Lookup(name)
	return ok && !definition.Annotations.ReadOnly && !coordinatorCatalogExcluded(name)
}

func coordinatorCatalogRequirement(action chat.Action) operatortool.Requirement {
	return operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: action.ProjectID}
}
