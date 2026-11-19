package coordinator

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorWorkArguments struct {
	operatortool.WorkArguments
	ReasonDetail string `json:"reason_detail,omitempty"`
}

type coordinatorActionArguments struct {
	WorkflowMarkdown  *string   `json:"workflow_markdown"`
	WorkItemID        string    `json:"work_item_id"`
	State             string    `json:"state"`
	ReasonDetail      string    `json:"reason_detail"`
	Title             *string   `json:"title"`
	Body              *string   `json:"body"`
	Labels            *[]string `json:"labels"`
	Priority          *int      `json:"priority"`
	RepositoryEnabled *bool     `json:"repository_enabled"`
	Intake            *string   `json:"intake"`
	Projection        *string   `json:"projection"`
}

func (d ProjectActionDeps) Preview(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall, requireConfirmation ...bool) (any, error) {
	ctx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	requirement := operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)}
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		requirement.Scope = apikey.ScopeAdmin
		if call.Name == "get_project_integration" {
			requirement.Scope = apikey.ScopeRead
		}
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, operatortool.ErrAccessDenied
	}
	scope.Project = record.ProjectID
	var args coordinatorActionArguments
	limit := hubapi.CoordinatorToolArgumentBytes
	if call.Name == "update_project_integration" {
		limit = hubapi.CoordinatorWorkflowArgumentBytes
	}
	if err := DecodeArgumentsWithLimit(call.Arguments, &args, limit); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	var allowed []string
	switch call.Name {
	case "get_project_integration":
		allowed = []string{}
	case "update_project_integration":
		allowed = []string{"repository_enabled", "intake", "projection", "workflow_markdown"}
	case operatortool.MoveItem:
		allowed = []string{"work_item_id", "state", "reason_detail"}
	case operatortool.EditItem:
		allowed = []string{"work_item_id", "title", "body", "labels", "priority"}
	case operatortool.AddComment:
		allowed = []string{"work_item_id", "body"}
	default:
		return nil, operatortool.ErrUnknownTool
	}
	for key, value := range fields {
		if !slices.Contains(allowed, key) || string(value) == "null" {
			return nil, operatortool.ErrInvalidArguments
		}
	}
	requestID := ActionRequestID(d.MessageID(), call)
	action := chat.Action{RequiresConfirmation: len(requireConfirmation) > 0 && requireConfirmation[0], ConversationID: record.ID, Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Title: strings.ReplaceAll(call.Name, "_", " "), Material: true}
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		project, err := d.ReadProject(ctx, scope)
		current := integrationpage.Record(project)
		available := d.GitHubAvailable
		current.GitHubTransportAvailable = &available
		if err != nil {
			return nil, err
		}
		current.WorkflowMarkdown, err = d.ReadWorkflow(ctx, project)
		if err != nil {
			return nil, err
		}
		if call.Name == "get_project_integration" {
			return current, nil
		}
		if len(fields) == 0 {
			return nil, operatortool.ErrInvalidArguments
		}
		if (args.RepositoryEnabled != nil || args.Intake != nil || args.Projection != nil) && !*current.GitHubTransportAvailable {
			return nil, hubapi.NativePublicInvalid("Hub GitHub integration is unavailable. Associate the runner checkout in project setup and approve the runner's repository policy with GitHub PR landing enabled.")
		}
		input := operatortool.IntegrationInput{WorkflowMarkdown: args.WorkflowMarkdown, ExpectedRevision: current.Revision, Intake: current.Intake, Projection: current.Projection, RepositoryEnabled: current.RepositoryEnabled}
		if args.WorkflowMarkdown != nil {
			if current.Authority["workflow"] == "repository" {
				return nil, hubapi.NativeWorkflowInvalid("Workflow is controlled by the repository; propose a reviewed definition change, then approve its trusted policy revision and verify runner adoption")
			}
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(*args.WorkflowMarkdown)})
			if err != nil {
				return nil, hubapi.NativeWorkflowInvalid(err.Error())
			}
			if workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
				return nil, hubapi.NativeWorkflowInvalid("Cloud workflow must use tracker.kind hub_native")
			}
			if err := workflow.Config.Validate(); err != nil {
				return nil, hubapi.NativeWorkflowInvalid(err.Error())
			}
			if err := workflow.Config.ValidateNativeWorkflow(); err != nil {
				return nil, hubapi.NativeWorkflowInvalid(err.Error())
			}
		}
		if current.Intake == "automatic" {
			input.Intake = ""
		}
		if args.RepositoryEnabled != nil {
			input.RepositoryEnabled = *args.RepositoryEnabled
		}
		if args.Intake != nil {
			input.Intake = *args.Intake
		}
		if args.Projection != nil {
			input.Projection = *args.Projection
		}
		action.Arguments, err = json.Marshal(map[string]any{"project_id": action.ProjectID, "request_id": requestID, "input": input})
		if err != nil {
			return nil, err
		}
		if _, err := d.PreviewCommand(ctx, operatortool.Call{Name: call.Name, Arguments: action.Arguments}); err != nil {
			return nil, err
		}
		proposedIntake := cmp.Or(input.Intake, current.Intake)
		action.Description = fmt.Sprintf("Hub repository integration: %t → %t; intake: %s → %s; projection: %s → %s (revision %d).", current.RepositoryEnabled, input.RepositoryEnabled, current.Intake, proposedIntake, current.Projection, input.Projection, current.Revision)
		if args.WorkflowMarkdown != nil {
			diff, err := SettingsDiff(map[string]any{"workflow_markdown": current.WorkflowMarkdown}, map[string]any{"workflow_markdown": *args.WorkflowMarkdown})
			if err != nil {
				return nil, err
			}
			action.Description += "\n" + diff
		}
	} else {
		if args.WorkItemID == "" || len(args.WorkItemID) > 256 {
			return nil, operatortool.ErrInvalidArguments
		}
		current, err := d.ObserveIssue(ctx, scope, args.WorkItemID)
		if err != nil {
			return nil, err
		}
		if current.Profile != "native" {
			return nil, hubapi.NativePublicInvalid("Chat changes require a native issue")
		}
		action.IssueID, action.Identifier, action.CurrentState, action.Revision = args.WorkItemID, args.WorkItemID, current.State, int64(current.Revision)
		request := coordinatorWorkArguments{WorkArguments: operatortool.WorkArguments{ProjectID: action.ProjectID, Identifier: args.WorkItemID, ExpectedRevision: int64(current.Revision), Title: args.Title, Body: args.Body, Labels: args.Labels, Priority: args.Priority}}
		if call.Name == operatortool.MoveItem {
			if strings.TrimSpace(args.State) == "" || len(args.State) > 256 {
				return nil, operatortool.ErrInvalidArguments
			}
			action.TargetState = args.State
			request.State = args.State
			request.ReasonDetail = strings.TrimSpace(args.ReasonDetail)
			if err := d.ValidateMoveAnswer(ctx, scope, current, request.State, request.ReasonDetail); err != nil {
				return nil, err
			}
		} else {
			raw, err := json.Marshal(request)
			if err != nil {
				return nil, err
			}
			if call.Name == operatortool.AddComment {
				request.ExpectedRevision = 0
				raw, err = json.Marshal(request)
				if err != nil {
					return nil, err
				}
			}
			if _, err := operatortool.DecodeWorkArguments(call.Name, raw); err != nil {
				return nil, err
			}
		}
		action.Arguments, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
		action.Description = fmt.Sprintf("Issue %s: %s (revision %d).", current.Title, current.State, current.Revision)
		if call.Name == operatortool.MoveItem {
			action.Description = fmt.Sprintf("Move %s from %s to %s.", current.Title, current.State, args.State)
			if request.ReasonDetail != "" {
				action.Description += "\nRecord the human's answer on the item: " + request.ReasonDetail
			}
		}
	}
	return d.Submit(ctx, record, call, action)
}

func SubmitAction(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall, action chat.Action, messageID func() string, model string, publish func(context.Context, string, conversations.ConversationMessageRecord) error) (any, error) {
	if action.RequestID == "" {
		action.RequestID = ActionRequestID(messageID(), call)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	action.Mutation = mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: action.ProjectID, ResourceID: action.IssueID, Action: call.Name, Source: "chat", CorrelationID: hubapi.NewID("luna")}
	var err error
	action.Mutation, err = action.Mutation.Bind(action.RequestID, action.Arguments)
	if err != nil {
		return nil, err
	}
	if action.Description == "" {
		action.Description = action.Title
	}
	action.OrganizationID = string(record.OrganizationID)
	action.ConversationID = record.ID
	action.ID = action.RequestID
	action.Agent = Agent(model)
	data, err := json.Marshal(map[string]any{"operator_action": map[string]any{"action": action, "conversation_id": record.ID}})
	if err != nil {
		return nil, err
	}
	message := conversations.ConversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: action.Description, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
	if err := publish(ctx, record.ID, message); err != nil {
		return nil, err
	}
	return map[string]any{"action_id": action.ID, "preview": action.Arguments, "status": "proposed", "message": "The client received the proposed call. It controls confirmation and submits the call; do not claim execution succeeded."}, nil
}

func (s ActionDispatchDeps) Execute(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	if action.ConversationID == "" {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	ctx, err := s.Authorize(ctx, action)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	agent, err := s.ProposalAgent(ctx, action)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	ctx = s.WithAgent(ctx, agent)
	if s.CatalogWrite(string(action.Kind)) {
		return s.ExecuteCatalog(ctx, action)
	}
	ctx = mutation.WithContext(ctx, action.Mutation)
	if SettingsTool(string(action.Kind)) {
		return s.ExecuteSettings(ctx, action)
	}
	if action.Kind == chat.ActionArchiveItems {
		return s.ExecuteArchive(ctx, action)
	}
	if action.Kind == chat.ActionIssueSplit {
		return s.ExecuteSplit(ctx, action)
	}
	if string(action.Kind) == "set_runner_tier" || string(action.Kind) == "convert_runner_scope" {
		return s.ExecuteRunner(ctx, action)
	}
	if string(action.Kind) == "promote_sprites_token" {
		return s.PromoteSpriteToken(ctx, action)
	}
	if SpriteMutation(string(action.Kind)) {
		return s.ExecuteSprite(ctx, action)
	}
	if WorkflowMutation(string(action.Kind)) {
		return s.ExecuteWorkflow(ctx, action)
	}
	if string(action.Kind) == "update_project_integration" {
		_, err := s.ExecuteProject(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments})
		if err != nil {
			return chat.ActionExecution{}, err
		}
		return chat.ActionExecution{Message: "Integration updated."}, nil
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.Project = tracker.ProjectID(action.ProjectID)
	if action.Kind == chat.ActionStopRun {
		return s.StopRun(ctx, scope, action)
	}
	if action.Kind == chat.ActionFileIssue {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(action.Arguments, &fields); err != nil {
			return chat.ActionExecution{}, err
		}
		delete(fields, "request_id")
		raw, err := json.Marshal(fields)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		request, err := operatortool.DecodeFileIssue(raw)
		if err != nil || request.ProjectID != action.ProjectID {
			return chat.ActionExecution{}, operatortool.ErrInvalidArguments
		}
		var priority *int
		if request.Priority != nil {
			priority = new(*request.Priority - 1)
		}
		result, err := s.CreateIssue(ctx, scope, tracker.CreateIssue{Mutation: tracker.MutationForContext(ctx, action.RequestID), Title: request.Title, Body: request.Description, State: request.State, Labels: request.Labels, Priority: priority})
		if err != nil {
			return chat.ActionExecution{}, ActionError(err)
		}
		return chat.ActionExecution{Message: "Issue filed.", Data: result}, nil
	}
	var request coordinatorWorkArguments
	if err := operatortool.DecodeArguments(action.Arguments, &request); err != nil {
		return chat.ActionExecution{}, err
	}
	if request.ProjectID != action.ProjectID || request.Identifier != action.IssueID {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	if action.Kind == chat.ActionMoveItem {
		_, err = s.TransitionIssue(ctx, scope, request.Identifier, tracker.Transition{Mutation: tracker.MutationForContext(ctx, action.RequestID), ExpectedRevision: tracker.Revision(request.ExpectedRevision), State: request.State, Reason: "user_requested", ReasonDetail: request.ReasonDetail}, action.ConversationID)
	} else {
		if _, err := operatortool.DecodeWorkArguments(string(action.Kind), action.Arguments); err != nil {
			return chat.ActionExecution{}, err
		}
		_, err = s.MutateIssue(ctx, scope, string(action.Kind), request.WorkArguments)
	}
	if err != nil {
		return chat.ActionExecution{}, ActionError(err)
	}
	message := "Issue updated."
	if action.Kind == chat.ActionMoveItem {
		message = "Moved issue to " + request.State + "."
		if strings.TrimSpace(request.ReasonDetail) != "" {
			message += " Recorded the human's answer on the item as a comment and transition reason."
		}
	} else if string(action.Kind) == operatortool.AddComment {
		message = "Comment added."
	}
	return chat.ActionExecution{Message: message, ResourceID: action.IssueID}, nil
}

func (s ActionAuthorizationDeps) Authorize(ctx context.Context, action chat.Action) (context.Context, error) {
	requirement := operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: action.IssueID}
	if WorkflowMutation(string(action.Kind)) || string(action.Kind) == "set_runner_tier" || string(action.Kind) == "convert_runner_scope" || string(action.Kind) == "promote_sprites_token" || string(action.Kind) == "update_project_integration" || SpriteMutation(string(action.Kind)) {
		requirement.Scope, requirement.ResourceKind, requirement.ResourceID = apikey.ScopeAdmin, "", ""
	}
	if SettingsTool(string(action.Kind)) {
		requirement = s.SettingsRequirement(string(action.Kind), action.ProjectID)
	}
	if s.CatalogWrite(string(action.Kind)) {
		requirement = s.CatalogRequirement(action)
	}
	if action.Kind == chat.ActionArchiveItems || action.Kind == chat.ActionFileIssue {
		requirement.ResourceKind = ""
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return nil, err
	}
	credential, err := s.Operator(ctx)
	if err != nil {
		return nil, err
	}
	record, err := s.ReadConversation(ctx, hubscope.NativeScope{Organization: tracker.OrganizationID(action.OrganizationID), Project: tracker.ProjectID(action.ProjectID), Credential: credential}, action.ConversationID)
	if err != nil {
		return nil, operatortool.ErrAccessDenied
	}
	if !Handles(record) {
		return nil, hubapi.NativePublicInvalid("This chat is linked; request the change in a project chat")
	}
	return ctx, nil
}

func PostRefusal(ctx context.Context, id, name string, failure error, publish func(context.Context, string, conversations.ConversationMessageRecord) error) error {
	message := conversations.ConversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: strings.ReplaceAll(name, "_", " ") + " refused: " + conversations.BoundRunes(failure.Error(), ErrorRunes), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
	return publish(ctx, id, message)
}
