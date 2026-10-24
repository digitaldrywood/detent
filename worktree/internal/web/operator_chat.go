package web

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/store"
)

func (s *Server) operatorChatTool(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if err := operatortool.ValidateWorkspaceArguments(call); err != nil {
		return operatortool.Result{}, err
	}
	if call.Name == "get_operator_chat" {
		var request operatortool.OperatorChatReadArguments
		if operatortool.DecodeArguments(call.Arguments, &request) != nil || request.Limit < 0 || request.Limit > 200 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		transcript, err := s.chat.OperatorTranscript(ctx, request.Limit)
		if err != nil {
			return operatortool.Result{}, err
		}
		// Provider replies have no fixed length. Reduce whole older messages so
		// callers can retrieve the latest exchange inside the shared result budget.
		for len(transcript.Messages) > 1 {
			result, err := operatorResult(transcript)
			if err == nil {
				return result, nil
			}
			transcript.Messages = transcript.Messages[1:]
			transcript.HasMore = true
		}
		return operatorResult(transcript)
	}
	var request operatortool.OperatorChatArguments
	if operatortool.DecodeArguments(call.Arguments, &request) != nil || strings.TrimSpace(request.ProjectID) == "" || len(request.ProjectID) > 256 || request.RequestID == "" || len(request.RequestID) > 128 || strings.TrimSpace(request.Message) == "" || len(request.Message) > 8192 {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	correlation, err := randomMutationCorrelation()
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: request.ProjectID, Action: call.Name, Source: "mcp", CorrelationID: correlation, Confirmation: "none"}
	outcome := "failed"
	defer func() { s.auditMutation(ctx, m, outcome) }()
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: request.ProjectID})
	if err != nil {
		outcome = "denied"
		return operatortool.Result{}, err
	}
	if err := s.chat.CheckConnection(ctx); err != nil {
		return operatortool.Result{}, err
	}
	if !s.chat.HasProvider() {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	arguments, err := json.Marshal(struct {
		ProjectID string `json:"project_id"`
		Message   string `json:"message"`
	}{request.ProjectID, request.Message})
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	m, err = m.Bind(request.RequestID, json.RawMessage(arguments))
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if result, found, err := s.operatorMutationReplay(ctx, m); found || err != nil {
		outcome = "replayed"
		return result, err
	}
	records, ok := s.store.(store.OperatorMutations)
	if !ok {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	reserved, err := records.ReserveOperatorMutation(ctx, m)
	if err != nil {
		return operatortool.Result{}, safeMutationError(err)
	}
	if !reserved {
		result, _, err := s.operatorMutationReplay(ctx, m)
		return result, err
	}
	ctx = mutation.WithContext(ctx, m)
	connection := operatortool.CurrentConnection(ctx)
	if _, err := s.chat.Send(ctx, connection.ID, request.Message); err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	m.ResourceID = connection.ID
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := records.CompleteOperatorMutation(persistCtx, store.OperatorReceipt{Metadata: m, Outcome: "succeeded"}); err != nil {
		return operatortool.Result{}, mutation.ErrUncertain
	}
	outcome = "succeeded"
	return s.operatorChatTool(ctx, operatortool.Call{Name: "get_operator_chat", Arguments: json.RawMessage(`{"limit":20}`)})
}
