package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
)

const ToolResultBytes = 64 << 10

const (
	coordinatorToolListAttention = "list_attention"
	coordinatorToolExplainIssue  = "explain_issue"
	coordinatorToolProposeIssue  = "propose_issue"
)

func Tools(families []runner.AgentTool) []runner.AgentTool {
	return append([]runner.AgentTool{
		Tool("read_skill", "Read one built-in Detent skill's guidance by name or alias from the Available skills catalog.", `{"type":"object","required":["name"],"properties":{"name":{"type":"string"}},"additionalProperties":false}`),
		Tool(coordinatorToolListAttention,
			"List issues that need attention, grouped as blocked, waiting_for_input, running and review. Scope is this conversation's project or every project the user can read.",
			`{"type":"object","properties":{"scope":{"type":"string","enum":["project","all_projects"],"description":"project (default) or all_projects"},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum issues per group, default 20"}},"additionalProperties":false}`),
		Tool(coordinatorToolExplainIssue,
			"Explain one issue, including Done and other terminal issues: title, body, workflow state, latest attempt, recent comments and its linked conversation. Resolve issue numbers within this conversation's project and return the native work_item_id for action proposals.",
			`{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string","description":"Issue number (#19 or 19) in this conversation's project, or native work item identifier (wi_...)"}},"additionalProperties":false}`),
		Tool("read_issue_history", "Read current issue records with pagination, including Done and terminal issues. Supply work_item_id to resolve an issue number in this conversation's project; otherwise defaults to the attached issue. Sections include full body, comments, lane and phase history with reasons, attempts with outcomes, dependencies and PR state.", `{"type":"object","required":["section"],"properties":{"work_item_id":{"type":"string","description":"Issue number (#19 or 19) or native work item identifier (wi_...) in this conversation's project; defaults to the attached issue"},"section":{"type":"string","enum":["work_item","work_comments","work_history","work_runs","work_relationships","work_references","work_attempt_receipt"]},"cursor":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer","minimum":1,"maximum":200},"native_attempt_id":{"type":"string"}},"additionalProperties":false}`),
		IssueSplitTool(),
		Tool("load_split_issue_skill", "Load the split-issue skill before proposing an issue decomposition.", `{"type":"object","properties":{},"additionalProperties":false}`),
		Tool(coordinatorToolProposeIssue,
			"Propose a new issue for the user to confirm. This never creates the issue; it prepares a card the user can accept in the app.",
			`{"type":"object","required":["title","objective"],"properties":{"title":{"type":"string","maxLength":500},"objective":{"type":"string","maxLength":4000,"description":"What the issue should achieve, in Markdown"},"project_id":{"type":"string","description":"Target project; defaults to this conversation's project"}},"additionalProperties":false}`),
	}, families...)
}

// handle runs one tool call. Errors are returned to the model as
// {"error": ...} and logged; they never end the turn.
func (t ToolDeps) Handle(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	if t.RunDetailContext != nil {
		ctx = t.RunDetailContext(ctx)
	}
	t.EndSegment(ctx)
	result, err := t.Execute(ctx, call)
	if err != nil {
		if WorkflowTool(call.Name) || call.Name == "get_runners" || call.Name == "set_runner_tier" || t.IsSpriteTool(call.Name) || call.Name == "get_project_integration" || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) || call.Name == t.CatalogCallName {
			err = ActionError(err)
		}
		if WorkflowMutation(call.Name) || call.Name == "set_runner_tier" || t.SpriteMutation(call.Name) || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) {
			if postErr := t.PostActionRefusal(ctx, call.Name, err); postErr != nil {
				t.Logger.Warn("coordinator could not persist refusal", "conversation_id", t.ConversationID, "error", postErr)
			}
		}
		failure := ToolError(err)
		t.Logger.Warn("coordinator tool failed", "conversation_id", t.ConversationID, "tool", call.Name, "error", err, "tool_error", failure.Content)
		return failure, nil
	}
	if !t.ScreenTool(call.Name) {
		record, readErr := t.ReadConversation(ctx)
		if readErr != nil {
			return ToolError(readErr), nil
		}
		if actorCtx, contextErr := t.ActionContext(ctx, record); contextErr == nil {
			credential, authErr := t.Operator(actorCtx)
			if authErr != nil {
				return ToolError(authErr), nil
			}
			if err := t.RestrictReadAudience(actorCtx, record, credential); err != nil {
				return ToolError(err), nil
			}
		} else if record.ReadAudience != "" {
			return ToolError(contextErr), nil
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ToolError(fmt.Errorf("encode %s result: %w", call.Name, err)), nil
	}
	limit := ToolResultBytes
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		limit = hubapi.CoordinatorWorkflowArgumentBytes
	}
	if len(encoded) > limit {
		return ToolError(fmt.Errorf("%s result exceeds %d bytes; narrow the request", call.Name, limit)), nil
	}
	return runner.AgentToolResult{Content: string(encoded), Success: true}, nil
}

func (t ToolDeps) Execute(ctx context.Context, call runner.AgentToolCall) (any, error) {
	record, err := t.ReadConversation(ctx)
	if err != nil {
		return nil, err
	}
	if t.ScreenTool(call.Name) {
		return t.ScreenRead(ctx, record, call)
	}
	if actorCtx, contextErr := t.ActionContext(ctx, record); contextErr == nil {
		credential, authErr := t.Operator(actorCtx)
		if authErr != nil {
			return nil, authErr
		}
		if err := t.AuthorizeRecord(actorCtx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, record, false); err != nil {
			return nil, err
		}
		record.OwnerPrincipalID = credential.ID
		record.OwnerSubject = ""
		if credential.Hosted != nil {
			record.OwnerSubject = credential.Hosted.Subject
		}
	} else if record.ReadAudience != "" {
		return nil, contextErr
	}
	readable, err := t.ReadableProjects(ctx, record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(readable, record.ProjectID) {
		return nil, ErrProjectUnreadable
	}
	if SettingsTool(call.Name) {
		return t.SettingsTool(ctx, record, call)
	}
	if WorkflowTool(call.Name) {
		return t.WorkflowTool(ctx, record, call)
	}
	switch call.Name {
	case "read_skill":
		var args struct {
			Name string `json:"name"`
		}
		if err := DecodeArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Name) == "" {
			return nil, ErrToolArguments
		}
		skill, body, err := skills.ReadBuiltin(args.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": skill.Name, "body": body}, nil
	case "get_runners", "set_runner_tier", "convert_runner_scope":
		return t.RunnerTool(ctx, record, call)
	case "promote_sprites_token":
		return t.PromoteSpritesTokenTool(ctx, record, call)
	case "get_sprite_pool", "set_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", "get_sprite_bootstrap_log":
		return t.SpriteTool(ctx, record, call)
	case "read_issue_history":
		return t.ReadIssueHistory(ctx, record, call)
	case "get_project_integration", "update_project_integration", "move_item", "edit_item", "add_comment":
		return t.ProjectAction(ctx, record, call)
	case "propose_stop_run":
		return t.ProposeStopRun(ctx, record, call)
	case "propose_file_issue", "propose_maintenance_issue", "propose_move_item", "propose_set_priority", "propose_backlog_admission":
		return t.SlashAction(ctx, record, call)
	case coordinatorToolListAttention:
		var args struct {
			Scope string `json:"scope"`
			Limit int    `json:"limit"`
		}
		if err := DecodeArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.ListAttention(ctx, record, readable, args.Scope, args.Limit)
	case coordinatorToolExplainIssue:
		var args struct {
			WorkItemID string `json:"work_item_id"`
		}
		if err := DecodeArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.ExplainIssue(ctx, record, readable, args.WorkItemID)
	case t.CatalogFindName:
		return t.FindTools(ctx, record, call)
	case t.CatalogCallName:
		return t.CallTool(ctx, record, call)
	case "load_split_issue_skill":
		return LoadSplitIssueSkill(call.Arguments)
	case string(chat.ActionArchiveItems):
		return t.ProposeIssueArchive(ctx, record, call)
	case string(chat.ActionIssueSplit):
		return t.ProposeIssueSplit(ctx, record, call)
	case coordinatorToolProposeIssue:
		var args struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
			ProjectID string `json:"project_id"`
		}
		if err := DecodeArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.ProposeIssue(ctx, record, readable, args.Title, args.Objective, args.ProjectID)
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}
