package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

var errOperatorCommandUnavailable = errors.New("operator command is unavailable")

type dashboardOperatorExecutor struct{ server *Server }

type operatorActionResult struct {
	ActionID       string               `json:"action_id"`
	ConnectionID   string               `json:"connection_id"`
	OrganizationID string               `json:"organization_id"`
	ProjectID      string               `json:"project_id"`
	ResourceID     string               `json:"resource_id,omitempty"`
	Identifier     string               `json:"identifier,omitempty"`
	URL            string               `json:"url,omitempty"`
	Client         string               `json:"client"`
	Action         chatpkg.ActionKind   `json:"action"`
	Arguments      json.RawMessage      `json:"arguments"`
	Preview        chatpkg.Action       `json:"preview"`
	Status         chatpkg.ActionStatus `json:"status"`
	ApprovalURL    string               `json:"approval_url"`
	ResultTool     string               `json:"result_tool"`
}

func (e dashboardOperatorExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.server.chat.AttachConnection(ctx)
}

func (e dashboardOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	definitions, err := operatortool.NewAuthorizedExecutor(e.server.operatorTools).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range operatortool.CommandCatalog() {
		if definition.Annotations.ReadOnly {
			definitions = append(definitions, definition)
		} else if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite}); err == nil {
			definitions = append(definitions, definition)
		}
	}
	return definitions, nil
}

func (e dashboardOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	s := e.server
	switch call.Name {
	case operatortool.ConnectionInfo:
		if err := operatortool.DecodeArguments(call.Arguments, &struct{}{}); err != nil {
			return operatortool.Result{}, err
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
			return operatortool.Result{}, err
		}
		if err := s.chat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		connection := operatortool.CurrentConnection(ctx)
		conversation := s.chat.Conversation(connection.ID)
		return operatorResult(struct {
			ID           string                 `json:"connection_id"`
			Organization string                 `json:"organization_id"`
			Client       string                 `json:"client"`
			Mode         chatpkg.ConnectionMode `json:"mode"`
			URL          string                 `json:"setup_url"`
		}{connection.ID, connection.Identity.OrganizationID, conversation.Client, conversation.Mode, s.operatorApprovalURL(connection.ID)})
	case operatortool.ActionResult:
		var request struct {
			ActionID string `json:"action_id"`
		}
		if err := operatortool.DecodeArguments(call.Arguments, &request); err != nil || request.ActionID == "" || len(request.ActionID) > 256 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
			return operatortool.Result{}, err
		}
		if err := s.chat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		connection := operatortool.CurrentConnection(ctx)
		action, ok := s.chat.Action(connection.ID, request.ActionID)
		if !ok {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, OrganizationID: action.OrganizationID, ProjectID: action.ProjectID}); err != nil {
			return operatortool.Result{}, err
		}
		return s.operatorActionResult(action)
	case operatortool.MoveItem, operatortool.SetPriority, operatortool.StopRun, operatortool.FileIssue:
		requestID, arguments, projectID, err := operatorActionArguments(call.Arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
		ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: projectID})
		if err != nil {
			return operatortool.Result{}, err
		}
		if previous, found, err := s.chat.RetryResult(ctx, chatpkg.ActionKind(call.Name), requestID, arguments); err != nil {
			return operatortool.Result{}, err
		} else if found {
			return s.operatorActionResult(previous)
		}
		proposal, err := s.operatorActionProposal(ctx, call.Name, arguments)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		proposal.RequestID, proposal.Arguments = requestID, arguments
		action, err := s.chat.Submit(ctx, proposal)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		return s.operatorActionResult(action)
	default:
		return operatortool.NewAuthorizedExecutor(s.operatorTools).Execute(ctx, call)
	}
}

func operatorActionArguments(raw json.RawMessage) (string, json.RawMessage, string, error) {
	var fields map[string]json.RawMessage
	if len(raw) > operatortool.MaxArgumentBytes || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", nil, "", operatortool.ErrInvalidArguments
	}
	var requestID, projectID string
	if json.Unmarshal(fields["request_id"], &requestID) != nil || requestID == "" || len(requestID) > 128 || json.Unmarshal(fields["project_id"], &projectID) != nil || strings.TrimSpace(projectID) == "" || len(projectID) > 256 {
		return "", nil, "", operatortool.ErrInvalidArguments
	}
	delete(fields, "request_id")
	for key, raw := range fields {
		if key == "priority" {
			var rank int
			if json.Unmarshal(raw, &rank) == nil && (rank < 1 || rank > 4) {
				return "", nil, "", operatortool.ErrInvalidArguments
			}
		}
		limit := 256
		if key == "description" {
			limit = 32768
		}
		if key == "reason" {
			limit = 1120
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && len(value) > limit {
			return "", nil, "", operatortool.ErrInvalidArguments
		}
		if key == "labels" {
			var labels []string
			if json.Unmarshal(raw, &labels) != nil || len(labels) > 64 {
				return "", nil, "", operatortool.ErrInvalidArguments
			}
			for _, label := range labels {
				if len(label) > 256 {
					return "", nil, "", operatortool.ErrInvalidArguments
				}
			}
		}
	}
	arguments, err := json.Marshal(fields)
	if err != nil {
		return "", nil, "", operatortool.ErrInvalidArguments
	}
	return requestID, arguments, strings.TrimSpace(projectID), nil
}

func (s *Server) operatorActionProposal(ctx context.Context, name string, arguments json.RawMessage) (chatpkg.Action, error) {
	var result chatpkg.ToolResult
	var err error
	switch name {
	case operatortool.MoveItem:
		result, err = s.chatMoveProposal(ctx, arguments)
	case operatortool.SetPriority:
		result, err = s.chatPriorityProposal(ctx, arguments)
	case operatortool.StopRun:
		result, err = s.chatStopProposal(ctx, arguments)
	case operatortool.FileIssue:
		result, err = s.chatFileIssueProposal(ctx, arguments)
	default:
		return chatpkg.Action{}, operatortool.ErrUnknownTool
	}
	if err != nil || result.Proposal == nil {
		return chatpkg.Action{}, errOperatorCommandUnavailable
	}
	return *result.Proposal, nil
}

func (s *Server) validateOperatorAction(ctx context.Context, action chatpkg.Action) error {
	name := string(action.Kind)
	current, err := s.operatorActionProposal(ctx, name, action.Arguments)
	if err != nil {
		return errOperatorCommandUnavailable
	}
	expected := action
	expected.ID, expected.ConnectionID, expected.OrganizationID, expected.Client, expected.RequestID = "", "", "", "", ""
	expected.Arguments, expected.Mode, expected.Status, expected.Result = nil, "", "", ""
	expected.CreatedAt, expected.ResolvedAt = current.CreatedAt, nil
	if !reflect.DeepEqual(current, expected) {
		return errOperatorCommandUnavailable
	}
	return nil
}

func (s *Server) operatorApprovalURL(id string) string {
	conversation := s.chat.Conversation(id)
	return strings.TrimRight(conversation.ApprovalBaseURL, "/") + "/chat/approval?connection_id=" + id
}

func (s *Server) operatorActionResult(action chatpkg.Action) (operatortool.Result, error) {
	return operatorResult(operatorActionResult{action.ID, action.ConnectionID, action.OrganizationID, action.ProjectID, action.IssueID, action.Identifier, action.ResourceURL, action.Client, action.Kind, action.Arguments, action, action.Status, s.operatorApprovalURL(action.ConnectionID), operatortool.ActionResult})
}

func operatorResult(value any) (operatortool.Result, error) {
	content, err := json.Marshal(value)
	if err != nil || len(content) > operatortool.MaxResultBytes {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	return operatortool.Result{Content: content}, nil
}
