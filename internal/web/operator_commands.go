package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/store"
)

var errOperatorCommandUnavailable = errors.New("operator command is unavailable")

type dashboardOperatorExecutor struct{ server *Server }

type operatorActionResult struct {
	Revision       int64                `json:"revision,omitempty"`
	CommentID      string               `json:"comment_id,omitempty"`
	CorrelationID  string               `json:"correlation_id"`
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
	for _, definition := range append(append(operatortool.CommandCatalog(), operatortool.OperatorChatCatalog()...), operatortool.ChangeCatalog()...) {
		if definition.Name == "post_operator_chat" && !e.server.chat.HasProvider() {
			continue
		}
		if _, fleet := operatortool.FleetDefinition(definition.Name); fleet {
			if s := e.server; !dashboardFleetTool(definition.Name) || !s.fleetToolAvailable(definition.Name) {
				continue
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, dashboardFleetRequirement(definition.Name, "")); err != nil {
				continue
			}
			definitions = append(definitions, definition)
			continue
		}
		if definition.Meta.Toolset == "billing_usage" && definition.Name != operatortool.BudgetOverrideSet && definition.Name != operatortool.BudgetOverrideClear && definition.Name != operatortool.UsageReport && definition.Name != operatortool.IssueExplanation {
			continue
		}
		scope := apikey.ScopeRead
		if !definition.Annotations.ReadOnly && definition.Name != operatortool.ArtifactAccess {
			scope = apikey.ScopeWrite
		}
		if definition.Name == operatortool.ApproveChangeReviewPolicy || definition.Name == operatortool.BindArtifactService {
			scope = apikey.ScopeAdmin
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope}); err == nil {
			definitions = append(definitions, definition)
		}
	}
	definitions = append(definitions, e.server.dashboardProjectTools(ctx)...)
	definitions = append(definitions, e.server.localSettingsTools(ctx)...)
	for _, d := range operatortool.LocalProjectCatalog() {
		scope := apikey.ScopeAdmin
		if d.Annotations.ReadOnly {
			scope = apikey.ScopeRead
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope}); err == nil {
			definitions = append(definitions, d)
		}
	}
	admin, err := e.server.credentialExecutor().ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range admin {
		if operatortool.IsAdministration(d.Name) && !e.server.usesLocalSettingsTool(ctx, d.Name) {
			definitions = append(definitions, d)
		}
	}
	return definitions, nil
}

func (e dashboardOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	s := e.server
	if s.usesLocalSettingsTool(ctx, call.Name) {
		return s.localSettingsExecute(ctx, call)
	}
	if operatortool.IsLocalProjectTool(call.Name) {
		return s.localProjectTool(ctx, call)
	}
	if definition, ok := operatortool.Lookup(call.Name); ok && definition.Meta.Toolset == "projects" {
		return e.server.dashboardProjectRead(ctx, call)
	}
	if d, ok := operatortool.ChangeDefinition(call.Name); ok {
		args, err := operatortool.DecodeChangeArguments(call.Name, call.Arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
		scope := apikey.ScopeRead
		if !d.Annotations.ReadOnly && call.Name != operatortool.ArtifactAccess {
			scope = apikey.ScopeWrite
		}
		if call.Name == operatortool.ApproveChangeReviewPolicy || call.Name == operatortool.BindArtifactService {
			scope = apikey.ScopeAdmin
		}
		ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ProjectID: args.ProjectID})
		if err != nil {
			return operatortool.Result{}, err
		}
		app, err := operatortool.CurrentChanges(ctx)
		if err != nil {
			return operatortool.Result{}, err
		}
		if d.Annotations.ReadOnly {
			value, err := app.ReadChange(ctx, call.Name, args)
			if err != nil {
				return operatortool.Result{}, err
			}
			return operatortool.BoundedChangeResult(value)
		}
		if call.Name == operatortool.ArtifactAccess {
			return s.executeOperatorArtifactAccess(ctx, app, args, call.Arguments)
		}
		result, err := s.executeOperatorMutation(ctx, call)
		if err != nil {
			return result, err
		}
		return result, nil
	}
	if operatortool.IsAdministration(call.Name) {
		return s.credentialExecutor().Execute(ctx, call)
	}
	if call.Name == operatortool.ActionResult {
		var request struct {
			ActionID string `json:"action_id"`
		}
		if operatortool.DecodeArguments(call.Arguments, &request) == nil {
			if action, ok := s.chat.Action(operatortool.CurrentConnection(ctx).ID, request.ActionID); ok && operatortool.IsAdministration(string(action.Kind)) && !s.usesLocalSettingsTool(ctx, string(action.Kind)) {
				return s.credentialExecutor().Execute(ctx, call)
			}
		}
	}
	if dashboardFleetTool(call.Name) {
		definition, _ := operatortool.FleetDefinition(call.Name)
		if definition.Annotations.ReadOnly {
			return s.executeFleetRead(ctx, call)
		}
		if err := operatortool.ValidateFleetArguments(call.Name, call.Arguments); err != nil {
			return operatortool.Result{}, err
		}
		return s.executeOperatorMutation(ctx, call)
	}
	switch call.Name {
	case "get_operator_chat", "post_operator_chat":
		return s.operatorChatTool(ctx, call)
	case operatortool.UsageReport:
		return s.operatorUsageReport(ctx, call.Arguments)
	case operatortool.IssueExplanation:
		var request struct {
			ProjectID string `json:"project_id"`
			Reference string `json:"reference"`
		}
		if operatortool.DecodeArguments(call.Arguments, &request) != nil || strings.TrimSpace(request.ProjectID) == "" || len(request.ProjectID) > 256 || strings.TrimSpace(request.Reference) == "" || len(request.Reference) > 256 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		return operatortool.NewAuthorizedExecutor(s.operatorTools).Execute(ctx, operatortool.Call{Name: operatortool.ExplainItem, Arguments: call.Arguments})
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
			ID           string `json:"connection_id"`
			Organization string `json:"organization_id"`
			Client       string `json:"client"`
		}{connection.ID, connection.Identity.OrganizationID, conversation.Client})
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
		if _, err := operatortool.AuthorizeCurrent(ctx, s.fleetMutationRequirement(ctx, string(action.Kind), action.ProjectID)); err != nil {
			return operatortool.Result{}, err
		}
		return s.operatorActionResult(action)
	case operatortool.MoveItem, operatortool.SetPriority, operatortool.StopRun, operatortool.FileIssue, operatortool.BudgetOverrideSet, operatortool.BudgetOverrideClear:
		return s.executeOperatorMutation(ctx, call)
	default:
		if operatortool.IsWorkTool(call.Name) {
			if call.Name == operatortool.ListComments || call.Name == operatortool.WorkflowTimeline {
				return s.executeWorkRead(ctx, call)
			}
			return s.executeOperatorMutation(ctx, call)
		}
		return operatortool.NewAuthorizedExecutor(s.operatorTools).Execute(ctx, call)
	}
}

func (s *Server) executeOperatorMutation(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	identity := operatortool.ConnectionIdentity(ctx)
	correlation, err := randomMutationCorrelation()
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, Action: call.Name, Source: "mcp", Confirmation: "none", CorrelationID: correlation}
	outcome := "failed"
	defer func() { s.auditMutation(ctx, m, outcome) }()
	requestID, arguments, projectID, err := operatorActionArguments(call.Arguments, dashboardFleetTool(call.Name) || s.usesLocalSettingsTool(ctx, call.Name))
	if err != nil {
		return operatortool.Result{}, err
	}
	if call.Name == operatortool.FileIssue || call.Name == operatortool.StopRun {
		var fields map[string]json.RawMessage
		if json.Unmarshal(arguments, &fields) != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if raw, ok := fields["priority"]; ok {
			var rank int
			if json.Unmarshal(raw, &rank) != nil || rank < 1 || rank > 4 {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
	}
	m.ProjectID = projectID
	m, err = m.Bind(requestID, arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, s.fleetMutationRequirement(ctx, call.Name, projectID))
	if err != nil {
		outcome = "denied"
		return operatortool.Result{}, err
	}
	if operatortool.IsWorkTool(call.Name) {
		if _, err := operatortool.DecodeWorkArguments(call.Name, arguments); err != nil {
			return operatortool.Result{}, err
		}
	}
	if _, ok := operatortool.ChangeDefinition(call.Name); ok {
		if _, err := s.operatorChangeProposal(ctx, call.Name, arguments); err != nil {
			return operatortool.Result{}, err
		}
	}
	if previous, found, err := s.chat.RetryResult(ctx, chatpkg.ActionKind(call.Name), requestID, arguments); err != nil {
		if (call.Name == operatortool.BudgetOverrideSet || call.Name == operatortool.BudgetOverrideClear) && errors.Is(err, operatortool.ErrInvalidArguments) {
			return operatortool.Result{}, mutation.ErrConflict
		}
		return operatortool.Result{}, err
	} else if found {
		outcome = "replayed"
		return s.operatorActionResult(previous)
	}
	if replay, found, err := s.operatorMutationReplay(ctx, m); found || err != nil {
		if err == nil {
			outcome = "replayed"
		}
		return replay, err
	}
	proposal, err := s.operatorActionProposal(ctx, call.Name, arguments)
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	m.ResourceID = proposal.IssueID
	records, ok := s.store.(store.OperatorMutations)
	if !ok {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	reserved, err := records.ReserveOperatorMutation(ctx, m)
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	if !reserved {
		replay, _, err := s.operatorMutationReplay(ctx, m)
		return replay, err
	}
	proposal.RequestID, proposal.Arguments, proposal.Mutation = requestID, arguments, m

	action, err := s.chat.Submit(ctx, proposal)
	if err != nil {
		return operatortool.Result{}, safeMutationError(err)
	}
	m = action.Mutation
	m.ResourceID = action.IssueID
	outcome = string(action.Status)
	return s.operatorActionResult(action)
}

func operatorActionArguments(raw json.RawMessage, allowGlobal bool) (string, json.RawMessage, string, error) {
	var fields map[string]json.RawMessage
	if len(raw) > operatortool.MaxArgumentBytes || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", nil, "", operatortool.ErrInvalidArguments
	}
	var requestID, projectID string
	if json.Unmarshal(fields["request_id"], &requestID) != nil || requestID == "" || len(requestID) > 128 || (!allowGlobal && (json.Unmarshal(fields["project_id"], &projectID) != nil || strings.TrimSpace(projectID) == "")) || len(projectID) > 256 {
		return "", nil, "", operatortool.ErrInvalidArguments
	}
	if field, ok := fields["project_id"]; ok {
		if json.Unmarshal(field, &projectID) != nil {
			return "", nil, "", operatortool.ErrInvalidArguments
		}
	}
	delete(fields, "request_id")
	for key, raw := range fields {
		if key == "priority" {
			var rank int
			if json.Unmarshal(raw, &rank) == nil && (rank < 0 || rank > 4) {
				return "", nil, "", operatortool.ErrInvalidArguments
			}
		}
		limit := 256
		if key == "description" || key == "body" {
			limit = 32768
		}
		if key == "evidence" {
			limit = 4096
		}
		if key == "cursor" {
			limit = 2048
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
	if s.usesLocalSettingsTool(ctx, name) {
		return s.localSettingsAction(name, arguments)
	}
	if operatortool.IsLocalProjectTool(name) {
		return localProjectAction(name, arguments)
	}
	if dashboardFleetTool(name) {
		return s.fleetActionProposal(ctx, name, arguments)
	}
	var result chatpkg.ToolResult
	var err error
	switch name {
	case operatortool.BudgetOverrideSet, operatortool.BudgetOverrideClear:
		return s.budgetActionProposal(ctx, name, arguments)
	case operatortool.MoveItem:
		result, err = s.chatMoveProposal(ctx, arguments)
	case operatortool.SetPriority:
		result, err = s.chatPriorityProposal(ctx, arguments)
	case operatortool.StopRun:
		result, err = s.chatStopProposal(ctx, arguments)
	case operatortool.FileIssue:
		result, err = s.chatFileIssueProposal(ctx, arguments)
	default:
		if operatortool.IsWorkTool(name) {
			return s.workActionProposal(ctx, name, arguments)
		}
		if _, ok := operatortool.ChangeDefinition(name); ok {
			return s.operatorChangeProposal(ctx, name, arguments)
		}
		return chatpkg.Action{}, operatortool.ErrUnknownTool
	}
	if err != nil || result.Proposal == nil {
		return chatpkg.Action{}, errOperatorCommandUnavailable
	}
	return *result.Proposal, nil
}

func (s *Server) validateOperatorAction(ctx context.Context, action chatpkg.Action) error {
	identity := operatortool.ConnectionIdentity(ctx)
	m := action.Mutation
	if m.Source != "mcp" || m.PrincipalID != identity.PrincipalID || m.OrganizationID != identity.OrganizationID || m.ProjectID != action.ProjectID || m.Action != string(action.Kind) || m.CorrelationID == "" {
		return operatortool.ErrAccessDenied
	}
	bound, err := m.Bind(action.RequestID, action.Arguments)
	if err != nil || bound.RetryIdentity != m.RetryIdentity || bound.InputHash != m.InputHash {
		return operatortool.ErrAccessDenied
	}
	name := string(action.Kind)
	if name == operatortool.ApproveChangeReviewPolicy || name == operatortool.BindArtifactService {
		var err error
		ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: action.ProjectID})
		if err != nil {
			return err
		}
	}
	current, err := s.operatorActionProposal(ctx, name, action.Arguments)
	if err != nil {
		return errOperatorCommandUnavailable
	}
	expected := action
	expected.ID, expected.ConnectionID, expected.OrganizationID, expected.Client, expected.RequestID = "", "", "", "", ""
	expected.Mutation = current.Mutation
	expected.Arguments, expected.Status, expected.Result = nil, "", ""
	expected.CreatedAt, expected.ResolvedAt = current.CreatedAt, nil
	if !reflect.DeepEqual(current, expected) {
		return errOperatorCommandUnavailable
	}
	return nil
}

func (s *Server) operatorActionResult(action chatpkg.Action) (operatortool.Result, error) {
	return operatorResult(operatorActionResult{action.Revision, action.CommentID, action.Mutation.CorrelationID, action.ID, action.ConnectionID, action.OrganizationID, action.ProjectID, action.IssueID, action.Identifier, action.ResourceURL, action.Client, action.Kind, action.Arguments, action, action.Status, operatortool.ActionResult})
}

func operatorResult(value any) (operatortool.Result, error) {
	content, err := json.Marshal(value)
	if err != nil || len(content) > operatortool.MaxResultBytes {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	return operatortool.Result{Content: content}, nil
}

func (s *Server) operatorMutationReplay(ctx context.Context, m mutation.Metadata) (operatortool.Result, bool, error) {
	records, ok := s.store.(store.OperatorMutations)
	if !ok {
		return operatortool.Result{}, false, errOperatorCommandUnavailable
	}
	receipt, found, err := records.OperatorMutation(ctx, m)
	if err != nil {
		return operatortool.Result{}, found, safeMutationError(err)
	}
	if !found {
		return operatortool.Result{}, false, nil
	}
	if receipt.Outcome != "succeeded" && receipt.Outcome != "rejected" {
		return operatortool.Result{}, true, mutation.ErrUncertain
	}
	if localSettingsReceipt(m.Action) {
		result, err := operatorResult(struct {
			Status      string          `json:"status"`
			Receipt     json.RawMessage `json:"configuration_receipt"`
			CompletedAt time.Time       `json:"completed_at"`
		}{receipt.Outcome, receipt.ConfigurationJSON, receipt.CompletedAt})
		return result, true, err
	}
	result, err := operatorResult(struct {
		Revision      int64     `json:"revision,omitempty"`
		CommentID     string    `json:"comment_id,omitempty"`
		CorrelationID string    `json:"correlation_id"`
		CompletedAt   time.Time `json:"completed_at"`
		Confirmation  string    `json:"confirmation"`
		Status        string    `json:"status"`
		ProjectID     string    `json:"project_id"`
		ResourceID    string    `json:"resource_id,omitempty"`
		Identifier    string    `json:"identifier,omitempty"`
		URL           string    `json:"url,omitempty"`
	}{receipt.Revision, receipt.CommentID, receipt.CorrelationID, receipt.CompletedAt, receipt.Confirmation, receipt.Outcome, receipt.ProjectID, receipt.ResourceID, receipt.Identifier, receipt.URL})
	return result, true, err
}

func safeMutationError(err error) error {
	if errors.Is(err, mutation.ErrConflict) {
		return mutation.ErrConflict
	}
	if errors.Is(err, mutation.ErrUncertain) {
		return mutation.ErrUncertain
	}
	return errOperatorCommandUnavailable
}

// AuditAction is the shared chat approval/submission audit callback. It uses
// the originating actor even when a different authenticated operator approves.
func (s *Server) AuditAction(ctx context.Context, action chatpkg.Action, outcome string) {
	m := action.Mutation
	if m.Source != "mcp" {
		return
	}
	m.ResourceID = action.IssueID
	if outcome == "approved" || outcome == "rejected" {
		m.Confirmation = outcome
	}
	if outcome == "rejected" {
		if records, ok := s.store.(store.OperatorMutations); ok {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if err := records.CompleteOperatorMutation(persistCtx, store.OperatorReceipt{Metadata: m, Outcome: "rejected"}); err != nil {
				s.logger.WarnContext(ctx, "operator rejection receipt unavailable")
			}
		}
	}
	s.auditMutation(ctx, m, outcome)
}

func (s *Server) auditMutation(ctx context.Context, m mutation.Metadata, outcome string) {
	// Receipt uniqueness belongs only to the operation record, not audit attempts.
	m.RetryIdentity, m.InputHash = "", ""
	raw, err := json.Marshal(struct {
		mutation.Metadata
		Outcome string `json:"outcome"`
	}{m, outcome})
	if err != nil {
		return
	}
	s.logger.InfoContext(ctx, "operator mutation", "audit", string(raw))
	if s.store == nil {
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if _, err := s.store.RecordWorkflowPhaseEvent(auditCtx, store.WorkflowPhaseEvent{ProjectID: m.ProjectID, IssueID: m.ResourceID, PhaseType: store.WorkflowPhaseTypeOperatorAction, PhaseName: m.Action, Status: outcome, StartedAt: now, FinishedAt: now, EndpointFamily: "mcp", MetadataJSON: string(raw)}); err != nil {
		s.logger.WarnContext(ctx, "operator mutation audit unavailable")
	}
}

func randomMutationCorrelation() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
