package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

func Catalog(ctx context.Context, list func(context.Context) ([]operatortool.Definition, error), excluded func(string) bool) ([]operatortool.Definition, error) {
	definitions, err := list(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(definitions, func(definition operatortool.Definition) bool { return excluded(definition.Name) }), nil
}

func (d CatalogToolDeps) catalogContext(ctx context.Context, record conversations.ConversationRecord) (context.Context, error) {
	ctx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := d.Operator(ctx)
	if err != nil {
		return nil, err
	}
	return d.WithOperator(ctx, credential, string(record.OrganizationID)), nil
}

func (d CatalogToolDeps) FindTools(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		Query   string `json:"query"`
		Toolset string `json:"toolset"`
	}
	if err := DecodeArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	ctx, err := d.catalogContext(ctx, record)
	if err != nil {
		return nil, err
	}
	definitions, err := d.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(args.Query))
	if len(words) == 0 && args.Toolset == "" {
		entries := make([]CatalogEntry, 0, len(definitions))
		for _, definition := range definitions {
			entries = append(entries, CatalogEntry{Name: definition.Name, Toolset: definition.Meta.Toolset, ReadOnly: definition.Annotations.ReadOnly})
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
	limit := catalogMatches
	if len(words) == 0 {
		limit = 3 * catalogMatches
	}
	entries := []CatalogEntry{}
	for _, match := range matches[:min(len(matches), limit)] {
		definition := match.definition
		entries = append(entries, CatalogEntry{Name: definition.Name, Toolset: definition.Meta.Toolset, ReadOnly: definition.Annotations.ReadOnly, Description: definition.Description, InputSchema: definition.InputSchema})
	}
	return map[string]any{"tools": entries, "truncated": len(matches) > limit}, nil
}

func (d CatalogToolDeps) CallTool(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := DecodeArgumentsWithLimit(call.Arguments, &args, hubapi.CoordinatorWorkflowArgumentBytes); err != nil {
		return nil, err
	}
	if d.Excluded(args.Name) {
		return nil, fmt.Errorf("%w: call %s directly instead of through call_tool", ErrToolArguments, args.Name)
	}
	ctx, err := d.catalogContext(ctx, record)
	if err != nil {
		return nil, err
	}
	definitions, err := d.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(definitions, func(definition operatortool.Definition) bool { return definition.Name == args.Name })
	if index < 0 {
		return nil, fmt.Errorf("%w: %q is not a tool this user can run; search with find_tools", ErrToolArguments, args.Name)
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
		if args.Arguments["request_id"], err = json.Marshal(ActionRequestID(d.MessageID(), call)); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(args.Arguments)
	if err != nil {
		return nil, err
	}
	if definition.Annotations.ReadOnly {
		result, err := d.Execute(ctx, operatortool.Call{Name: definition.Name, Arguments: raw})
		if err != nil {
			return nil, err
		}
		return result.Content, nil
	}
	title := strings.ReplaceAll(definition.Name, "_", " ")
	action := chat.Action{Kind: chat.ActionKind(definition.Name), ProjectID: string(record.ProjectID), Title: title, Arguments: raw, Material: true, Description: "Confirm " + title + ": " + conversations.BoundRunes(string(raw), proposalBodyRunes)}
	return d.Submit(ctx, record, call, action)
}

func ExecuteCatalogCall(ctx context.Context, action chat.Action, deps CatalogExecutionDeps) (chat.ActionExecution, error) {
	ctx, err := deps.BindConnection(ctx, action.RequestID)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	definitions, err := deps.Catalog(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	if !slices.ContainsFunc(definitions, func(definition operatortool.Definition) bool { return definition.Name == string(action.Kind) }) {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	result, err := deps.Execute(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments})
	if err != nil {
		return chat.ActionExecution{}, ActionError(err)
	}
	return chat.ActionExecution{Message: action.Title + " completed.", Data: result.Content}, nil
}

type CatalogEntry struct {
	Name        string          `json:"name"`
	Toolset     string          `json:"toolset,omitempty"`
	ReadOnly    bool            `json:"read_only"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

const catalogMatches = 8
