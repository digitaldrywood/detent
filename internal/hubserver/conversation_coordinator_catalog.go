package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

const (
	coordinatorToolFindTools  = "find_tools"
	coordinatorToolCallTool   = "call_tool"
	coordinatorCatalogMatches = 8
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

func coordinatorCatalogExcluded(name string, tools []runner.AgentTool) bool {
	switch name {
	case coordinatorToolFindTools, coordinatorToolCallTool, string(chat.ActionStopRun), string(chat.ActionFileIssue), string(chat.ActionSetPriority), operatortool.ActionResult, operatortool.ConnectionInfo, "get_operator_chat", "post_operator_chat", "post_conversation_command", "stream_conversation_events":
		return true
	}
	return slices.ContainsFunc(tools, func(tool runner.AgentTool) bool { return tool.Name == name })
}

// coordinatorCatalogWrite reports whether a confirmed chat action runs a
// catalog tool through the operator executor rather than a coordinator tool.
func coordinatorCatalogWrite(name string) bool {
	definition, ok := operatortool.Lookup(name)
	return ok && !definition.Annotations.ReadOnly && !coordinatorCatalogExcluded(name, (&coordinatorToolset{}).tools())
}

func (t *coordinatorToolset) catalogContext(ctx context.Context, record conversationRecord) (context.Context, error) {
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return nil, err
	}
	return t.coordinator.service.server.withOperatorCatalog(ctx, credential, string(record.OrganizationID)), nil
}

func (s *Service) coordinatorCatalog(ctx context.Context) ([]operatortool.Definition, error) {
	definitions, err := (hostedOperatorExecutor{service: s}).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	tools := (&coordinatorToolset{}).tools()
	return slices.DeleteFunc(definitions, func(definition operatortool.Definition) bool {
		return coordinatorCatalogExcluded(definition.Name, tools)
	}), nil
}

type coordinatorCatalogEntry struct {
	Name        string          `json:"name"`
	Toolset     string          `json:"toolset,omitempty"`
	ReadOnly    bool            `json:"read_only"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (t *coordinatorToolset) findTools(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		Query   string `json:"query"`
		Toolset string `json:"toolset"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	ctx, err := t.catalogContext(ctx, record)
	if err != nil {
		return nil, err
	}
	definitions, err := t.coordinator.service.server.coordinatorCatalog(ctx)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(args.Query))
	if len(words) == 0 && args.Toolset == "" {
		entries := make([]coordinatorCatalogEntry, 0, len(definitions))
		for _, definition := range definitions {
			entries = append(entries, coordinatorCatalogEntry{Name: definition.Name, Toolset: definition.Meta.Toolset, ReadOnly: definition.Annotations.ReadOnly})
		}
		return map[string]any{"tools": entries, "hint": "Search with query or toolset to get descriptions and input schemas."}, nil
	}
	type match struct {
		definition operatortool.Definition
		score      int
	}
	var matches []match
	for _, definition := range definitions {
		if args.Toolset != "" && definition.Meta.Toolset != args.Toolset {
			continue
		}
		score := 1
		if len(words) > 0 {
			score = 0
			text := strings.ToLower(definition.Name + " " + definition.Description)
			for _, word := range words {
				if strings.Contains(strings.ToLower(definition.Name), word) {
					score += 3
				} else if strings.Contains(text, word) {
					score++
				}
			}
		}
		if score > 0 {
			matches = append(matches, match{definition, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	limit := coordinatorCatalogMatches
	if len(words) == 0 {
		limit = 3 * coordinatorCatalogMatches
	}
	entries := []coordinatorCatalogEntry{}
	for _, match := range matches[:min(len(matches), limit)] {
		definition := match.definition
		entries = append(entries, coordinatorCatalogEntry{Name: definition.Name, Toolset: definition.Meta.Toolset, ReadOnly: definition.Annotations.ReadOnly, Description: definition.Description, InputSchema: definition.InputSchema})
	}
	return map[string]any{"tools": entries, "truncated": len(matches) > limit}, nil
}

func (t *coordinatorToolset) callTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := decodeCoordinatorArgumentsWithLimit(call.Arguments, &args, coordinatorWorkflowArgumentBytes); err != nil {
		return nil, err
	}
	if coordinatorCatalogExcluded(args.Name, (&coordinatorToolset{}).tools()) {
		return nil, fmt.Errorf("%w: call %s directly instead of through call_tool", errCoordinatorToolArguments, args.Name)
	}
	ctx, err := t.catalogContext(ctx, record)
	if err != nil {
		return nil, err
	}
	s := t.coordinator.service.server
	definitions, err := s.coordinatorCatalog(ctx)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(definitions, func(definition operatortool.Definition) bool { return definition.Name == args.Name })
	if index < 0 {
		return nil, fmt.Errorf("%w: %q is not a tool this user can run; search with find_tools", errCoordinatorToolArguments, args.Name)
	}
	definition := definitions[index]
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return nil, err
	}
	if args.Arguments == nil {
		args.Arguments = map[string]json.RawMessage{}
	}
	if _, accepts := schema.Properties["project_id"]; accepts && args.Arguments["project_id"] == nil {
		if args.Arguments["project_id"], err = json.Marshal(record.ProjectID); err != nil {
			return nil, err
		}
	}
	if _, accepts := schema.Properties["request_id"]; accepts && args.Arguments["request_id"] == nil && !definition.Annotations.ReadOnly {
		if args.Arguments["request_id"], err = json.Marshal(coordinatorActionRequestID(t.state.users[len(t.state.users)-1].ID, call)); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(args.Arguments)
	if err != nil {
		return nil, err
	}
	if definition.Annotations.ReadOnly {
		result, err := (hostedOperatorExecutor{service: s}).Execute(ctx, operatortool.Call{Name: definition.Name, Arguments: raw})
		if err != nil {
			return nil, err
		}
		return result.Content, nil
	}
	title := strings.ReplaceAll(definition.Name, "_", " ")
	action := chat.Action{Kind: chat.ActionKind(definition.Name), ProjectID: string(record.ProjectID), Title: title, Arguments: raw, Material: true, Description: "Confirm " + title + ": " + boundRunes(string(raw), coordinatorProposalBodyRunes)}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) executeCoordinatorCatalogCall(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, err := s.bindCoordinatorConnection(ctx, action.RequestID)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	definitions, err := s.coordinatorCatalog(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	if !slices.ContainsFunc(definitions, func(definition operatortool.Definition) bool { return definition.Name == string(action.Kind) }) {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	result, err := (hostedOperatorExecutor{service: s}).Execute(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments})
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	return chat.ActionExecution{Message: action.Title + " completed.", Data: result.Content}, nil
}

func coordinatorCatalogRequirement(action chat.Action) operatortool.Requirement {
	return operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: action.ProjectID}
}
