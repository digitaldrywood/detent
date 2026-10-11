package hubserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

func coordinatorSlashActionTools() []runner.AgentTool {
	return []runner.AgentTool{
		coordinatorTool("propose_stop_run", "Propose releasing the exact active runner attempt and routing its item. Explicit client confirmation is required; the runner stops through existing lease-loss handling.", `{"type":"object","required":["work_item_id","destination"],"properties":{"work_item_id":{"type":"string"},"destination":{"type":"string","enum":["Blocked","Backlog","Cancelled","Todo"]},"priority":{"type":"integer","minimum":1,"maximum":4},"reason":{"type":"string","maxLength":280}},"additionalProperties":false}`),
		coordinatorTool("propose_file_issue", "Propose filing an issue in this project. Show a card; explicit client confirmation is required.", `{"type":"object","required":["title","description"],"properties":{"title":{"type":"string"},"description":{"type":"string"},"state":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false}`),
		coordinatorTool("propose_maintenance_issue", "Propose filing a maintenance issue in this project. Show a card; explicit client confirmation is required.", `{"type":"object","required":["title","description"],"properties":{"title":{"type":"string"},"description":{"type":"string"},"state":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false}`),
		coordinatorTool("propose_move_item", "Propose moving an issue to a lane. Show a card; explicit client confirmation is required.", `{"type":"object","required":["work_item_id","state"],"properties":{"work_item_id":{"type":"string"},"state":{"type":"string"}},"additionalProperties":false}`),
		coordinatorTool("propose_set_priority", "Propose setting issue priority: 1 Urgent, 2 High, 3 Normal, 4 Low. Show a card; explicit client confirmation is required.", `{"type":"object","required":["work_item_id","priority"],"properties":{"work_item_id":{"type":"string"},"priority":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false}`),
		coordinatorTool("propose_backlog_admission", "Propose admitting a Backlog issue to Todo after reviewing its criteria. Show a card; explicit client confirmation is required.", `{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string"}},"additionalProperties":false}`),
	}
}

func (t *coordinatorToolset) slashAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	if call.Name == "propose_file_issue" || call.Name == "propose_maintenance_issue" {
		var args struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			State       string   `json:"state,omitempty"`
			Labels      []string `json:"labels,omitempty"`
			Priority    *int     `json:"priority,omitempty"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		request := operatortool.FileIssueArguments{ProjectID: string(record.ProjectID), Title: args.Title, Description: args.Description, State: args.State, Labels: args.Labels, Priority: args.Priority}
		raw, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		if _, err := operatortool.DecodeFileIssue(raw); err != nil {
			return nil, err
		}
		action := chat.Action{ConversationID: record.ID, Kind: chat.ActionFileIssue, ProjectID: string(record.ProjectID), Title: args.Title, Description: "File issue: " + args.Title, Material: true, RequiresConfirmation: true}
		action.RequestID = coordinatorActionRequestID(t.state.users[len(t.state.users)-1].ID, call)
		action.Arguments, err = json.Marshal(struct {
			operatortool.FileIssueArguments
			RequestID string `json:"request_id"`
		}{request, action.RequestID})
		if err != nil {
			return nil, err
		}
		return t.submitCoordinatorAction(ctx, record, call, action)
	}
	var args struct {
		WorkItemID string `json:"work_item_id"`
		State      string `json:"state,omitempty"`
		Priority   *int   `json:"priority,omitempty"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.project = record.ProjectID
	issue, err := t.coordinator.service.server.resolveOperatorNativeItem(ctx, t.coordinator.service.store.db, scope, args.WorkItemID)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"work_item_id": string(issue.WorkItemID)}
	name := operatortool.MoveItem
	targetState := args.State
	switch call.Name {
	case "propose_move_item":
		if args.Priority != nil || strings.TrimSpace(args.State) == "" {
			return nil, operatortool.ErrInvalidArguments
		}
		fields["state"] = args.State
	case "propose_backlog_admission":
		if args.Priority != nil || args.State != "" || issue.State != "Backlog" {
			return nil, nativeInvalid("Admission requires a Backlog item")
		}
		targetState = "Todo"
		fields["state"] = targetState
	case "propose_set_priority":
		if args.State != "" || args.Priority == nil || *args.Priority < 1 || *args.Priority > 4 {
			return nil, operatortool.ErrInvalidArguments
		}
		name = operatortool.EditItem
		fields["priority"] = *args.Priority - 1
	default:
		return nil, operatortool.ErrUnknownTool
	}
	if name == operatortool.MoveItem {
		project, err := readNativeProject(ctx, t.coordinator.service.store.db, scope)
		if err != nil {
			return nil, err
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, targetState, scope.credential.Scope); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return t.projectAction(ctx, record, runner.AgentToolCall{Name: name, Arguments: raw}, true)
}
