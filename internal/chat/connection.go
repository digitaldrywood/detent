package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type ConnectionMode string

const (
	ConfirmationMode ConnectionMode = "confirmation"
	YOLOMode         ConnectionMode = "yolo"
)

type humanApprovalKey struct{}

// WithOperatorApproval is used only after dashboard browser authentication and
// form validation. No MCP executor or transport constructs this context.
func WithOperatorApproval(ctx context.Context, identity operatortool.Identity) context.Context {
	return context.WithValue(ctx, humanApprovalKey{}, identity)
}

func authorizeHuman(ctx context.Context, organization string) error {
	identity, ok := ctx.Value(humanApprovalKey{}).(operatortool.Identity)
	if !ok || !identity.Valid() || identity.OrganizationID != organization {
		return operatortool.ErrAccessDenied
	}
	return nil
}

func authorizeAction(ctx context.Context, connection operatortool.Connection, action Action) (context.Context, error) {
	requirement := operatortool.Requirement{
		Scope: apikey.ScopeWrite, OrganizationID: action.OrganizationID, ProjectID: action.ProjectID,
	}
	if operatortool.IsAdministration(string(action.Kind)) {
		requirement = operatortool.AdministrationRequirement(string(action.Kind))
		requirement.OrganizationID, requirement.ProjectID = action.OrganizationID, action.ProjectID
	}
	switch string(action.Kind) {
	case "apply_local_project_policy", "drain_local_project", "detach_local_project":
		requirement.Scope = apikey.ScopeAdmin
	case operatortool.CreateRunnerEnrollment, operatortool.RevokeRunnerEnrollment, operatortool.RevokeRunnerIdentity, operatortool.UpdateRunnerRouting, operatortool.UpdateRunnerHost, operatortool.UpdateRunnerCapacity:
		// The hub dashboard grants runner administration separately from project
		// writes; a member with all current runner grants need not be an owner.
		requirement.Scope, requirement.ResourceKind = apikey.ScopeAdmin, "runners"
	}
	return operatortool.AuthorizeCurrent(operatortool.WithConnection(ctx, connection), requirement)
}

// AttachConnection stores the original authority in the existing bounded chat
// session. Reusing its ID with a different authenticated identity is denied.
func (s *Service) AttachConnection(ctx context.Context) error {
	connection := operatortool.CurrentConnection(ctx)
	if connection.ID == "" || len(connection.ID) > 256 || !connection.Identity.Valid() || connection.Resolve == nil {
		return operatortool.ErrAccessDenied
	}
	current := s.session(connection.ID)
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.connection != nil {
		if current.connection.Identity != connection.Identity {
			return operatortool.ErrAccessDenied
		}
		return nil
	}
	current.connection = &connection
	current.mode = ConfirmationMode
	return nil
}

// CheckConnection uses the existing session without recreating expired or
// evicted retry receipts. Only trusted transport setup attaches a connection.
func (s *Service) connectionSession(ctx context.Context) (*session, error) {
	connection := operatortool.CurrentConnection(ctx)
	now := s.now().UTC()
	s.mu.Lock()
	current := s.sessions[connection.ID]
	if current != nil && now.Sub(current.lastUsedAt) > s.sessionTTL {
		delete(s.sessions, connection.ID)
		current = nil
	}
	if current != nil {
		current.lastUsedAt = now
	}
	s.mu.Unlock()
	if current == nil {
		return nil, operatortool.ErrAccessDenied
	}
	current.mu.Lock()
	valid := current.connection != nil && current.connection.Identity == connection.Identity
	current.mu.Unlock()
	if !valid {
		return nil, operatortool.ErrAccessDenied
	}
	return current, nil
}

func (s *Service) CheckConnection(ctx context.Context) error {
	_, err := s.connectionSession(ctx)
	return err
}

// CheckBrowserSession binds hosted approval to the original authenticated
// browser session without exposing its credential or granting it new powers.
func (s *Service) CheckBrowserSession(ctx context.Context, id string) error {
	identity := operatortool.ConnectionIdentity(ctx)
	s.mu.Lock()
	current := s.sessions[id]
	s.mu.Unlock()
	if current == nil || identity.SessionID == "" {
		return operatortool.ErrAccessDenied
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.connection == nil || current.connection.Identity.SessionID != identity.SessionID || current.connection.Identity.OrganizationID != identity.OrganizationID {
		return operatortool.ErrAccessDenied
	}
	return nil
}

// RetryResult returns the original receipt without proposing a new action against
// a changed board. Reusing a retry identity with changed arguments is denied.
func (s *Service) RetryResult(ctx context.Context, kind ActionKind, requestID string, arguments json.RawMessage) (Action, bool, error) {
	current, err := s.connectionSession(ctx)
	if err != nil {
		return Action{}, false, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	for index, previous := range current.actions {
		if previous.RequestID != requestID {
			continue
		}
		if previous.Kind != kind || !bytes.Equal(previous.Arguments, arguments) {
			return Action{}, false, operatortool.ErrInvalidArguments
		}
		executionContext, err := authorizeAction(ctx, *current.connection, previous)
		if err != nil {
			return Action{}, false, err
		}
		// Purchases already approved by the operator resume only the existing
		// application's durable checkout intent. Other failed effects retain
		// their receipt: they have no provider key that permits a safe retry.
		if (previous.Kind == ActionKind(operatortool.BillingCheckout) || previous.Kind == ActionKind(operatortool.CreditCheckout)) && previous.Status == ActionFailed && (previous.Mutation.Confirmation == "approved" || previous.Mutation.Confirmation == "yolo") && s.actions != nil {
			result, executeErr := s.actions.ExecuteAction(executionContext, previous)
			outcome := "succeeded"
			if executeErr != nil {
				outcome = "failed"
			}
			s.auditAction(executionContext, previous, outcome)
			_, err = s.resolveExecution(current, index, result, executeErr)
			if err != nil {
				return Action{}, true, err
			}
			previous = current.actions[index]
		}
		return cloneActions([]Action{previous})[0], true, nil
	}
	return Action{}, false, nil
}

// SetConnectionMode accepts only a trusted browser operator decision. The mode
// lives on the server session, never in initialize metadata or request headers.
func (s *Service) SetConnectionMode(ctx context.Context, id string, mode ConnectionMode) error {
	current := s.session(id)
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.connection == nil || mode != ConfirmationMode && mode != YOLOMode || current.connection.RequireConfirmation && mode != ConfirmationMode {
		return operatortool.ErrAccessDenied
	}
	if err := authorizeHuman(ctx, current.connection.Identity.OrganizationID); err != nil {
		return err
	}
	browser, ok := ctx.Value(humanApprovalKey{}).(operatortool.Identity)
	if !ok || current.connection.Identity.SessionID != "" && browser.PrincipalID != current.connection.Identity.PrincipalID {
		return operatortool.ErrAccessDenied
	}

	if _, err := operatortool.AuthorizeCurrent(operatortool.WithConnection(ctx, *current.connection), operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	current.mode = mode
	return nil
}

// RequiresConfirmation classifies arguments, independently of tool annotations.
// Unknown action kinds fail closed, including future billing/access commands.
func RequiresConfirmation(action Action) bool {
	if action.Mutation.Source == "chat" {
		return true
	}
	if required, known := operatortool.ProjectConfirmation(string(action.Kind), action.Arguments); known {
		return required
	}
	switch action.Kind {
	case ActionKind(operatortool.CreditAutoFund):
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		return json.Unmarshal(action.Arguments, &input) != nil || input.Enabled == nil || *input.Enabled
	case ActionSetPriority, "create_workspace", "create_conversation", "patch_conversation", "upload_conversation_attachment":
		return false
	case "post_conversation_command":
		var request struct {
			Input struct {
				Kind string `json:"kind"`
			} `json:"input"`
		}
		if json.Unmarshal(action.Arguments, &request) != nil {
			return true
		}
		return request.Input.Kind != "message" && request.Input.Kind != "answer"
	case "create_project_action", "patch_project_action":
		var request struct {
			Input struct {
				Command               *string `json:"command"`
				RunOnWorktreeCreation *bool   `json:"run_on_worktree_creation"`
			} `json:"input"`
		}
		if json.Unmarshal(action.Arguments, &request) != nil {
			return true
		}
		return request.Input.Command != nil || request.Input.RunOnWorktreeCreation != nil
	case ActionKind(operatortool.CreateChange), ActionKind(operatortool.PublishChangeVersion), ActionKind(operatortool.DiscussChange), ActionKind(operatortool.ViewChangeFile), ActionKind(operatortool.ArtifactAccess):
		return false
	case ActionKind(operatortool.ReviewChange):
		var args operatortool.ChangeArguments
		if json.Unmarshal(action.Arguments, &args) != nil {
			return true
		}
		return args.Decision != "commented"
	case ActionKind(operatortool.OrganizationSwitch):
		return false
	case ActionKind(operatortool.ResumeProvisioning):
		return true
	case ActionKind(operatortool.BudgetOverrideClear), ActionKind(operatortool.Refresh), ActionKind(operatortool.AcknowledgeWarnings), ActionKind(operatortool.SetQueuePriority), ActionKind(operatortool.AddComment), ActionKind(operatortool.EditComment), ActionKind(operatortool.SetDependency), ActionKind(operatortool.RestoreItem), ActionKind(operatortool.AcknowledgeParks), ActionKind(operatortool.OrderItem):
		return false
	case ActionKind(operatortool.EditItem):
		return action.Material
	case ActionKind(operatortool.RecoverAttempt):
		return action.Destination != "inspect"
	case ActionKind(operatortool.UpdateFleetRunner), ActionKind(operatortool.UpdateFleetHost), ActionKind(operatortool.UpdateRunnerRouting), ActionKind(operatortool.UpdateRunnerHost):
		return action.MaterialChange
	case ActionFileIssue:
		return action.Material || strings.EqualFold(action.State, "Done") || strings.EqualFold(action.State, "Cancelled")
	case ActionMoveItem:
		if action.NativeWorkflow {
			return action.Material
		}
		ordinarySource := strings.EqualFold(action.CurrentState, "Backlog") || strings.EqualFold(action.CurrentState, "Todo")
		ordinaryTarget := strings.EqualFold(action.TargetState, "Todo") || strings.EqualFold(action.TargetState, "Backlog")
		return action.Material || !ordinarySource || !ordinaryTarget
	default:
		return true
	}
}

// Submit executes ordinary writes directly. Material actions retain the exact
// preview, and only Confirm with a browser operator context can execute them.
// A request ID is a retry identity, never an approval token.
func (s *Service) Submit(ctx context.Context, action Action) (Action, error) {
	connection := operatortool.CurrentConnection(ctx)
	current, connectionErr := s.connectionSession(ctx)
	if connectionErr != nil {
		return Action{}, connectionErr
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if action.RequestID == "" || len(action.RequestID) > 128 || len(action.Arguments) > operatortool.MaxArgumentBytes {
		return Action{}, operatortool.ErrInvalidArguments
	}
	var arguments bytes.Buffer
	if err := json.Compact(&arguments, action.Arguments); err != nil {
		return Action{}, operatortool.ErrInvalidArguments
	}
	action.Arguments = append(json.RawMessage(nil), arguments.Bytes()...)
	for _, previous := range current.actions {
		if previous.RequestID != action.RequestID {
			continue
		}
		if previous.Kind != action.Kind || !bytes.Equal(previous.Arguments, action.Arguments) {
			return Action{}, operatortool.ErrInvalidArguments
		}
		if _, err := authorizeAction(ctx, *current.connection, previous); err != nil {
			return Action{}, err
		}
		return cloneActions([]Action{previous})[0], nil
	}
	action.ConnectionID = connection.ID
	action.OrganizationID = connection.Identity.OrganizationID
	action.Client = current.connection.Client
	action.Mode = current.mode
	action.Mutation.Mode = string(current.mode)
	action.Mutation.Confirmation = "none"
	if RequiresConfirmation(action) {
		action.Mutation.Confirmation = "pending"
	}
	if RequiresConfirmation(action) && current.mode == YOLOMode && !current.connection.RequireConfirmation {
		action.Mutation.Confirmation = "yolo"
	}
	var idErr error
	action.ID, idErr = s.newID()
	if idErr != nil {
		return Action{}, idErr
	}
	action.CreatedAt, action.Status = s.now().UTC(), ActionPending
	ctx, err := authorizeAction(ctx, *current.connection, action)
	if err != nil {
		return Action{}, err
	}
	current.actions = append(current.actions, cloneActions([]Action{action})[0])
	index := len(current.actions) - 1
	if !current.connection.RequireConfirmation && (!RequiresConfirmation(action) || current.mode == YOLOMode) {
		if s.actions == nil {
			return Action{}, ErrUnavailable
		}
		result, executeErr := s.actions.ExecuteAction(ctx, action)
		outcome := "succeeded"
		if executeErr != nil {
			outcome = "failed"
		}
		auditAction := action
		if executeErr == nil && result.ResourceID != "" {
			auditAction.IssueID = result.ResourceID
		}
		s.auditAction(ctx, auditAction, outcome)
		_, err = s.resolveExecution(current, index, result, executeErr)
	}
	return cloneActions(current.actions[index : index+1])[0], err
}

func (s *Service) RejectConnectionAction(ctx context.Context, id, actionID string) (Conversation, error) {
	current := s.session(id)
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.connection == nil {
		return Conversation{}, operatortool.ErrAccessDenied
	}
	if err := authorizeHuman(ctx, current.connection.Identity.OrganizationID); err != nil {
		return Conversation{}, err
	}
	index := actionIndex(current.actions, actionID)
	if index < 0 {
		return s.conversation(current), ErrActionNotFound
	}
	if current.actions[index].Status != ActionPending {
		return s.conversation(current), ErrActionNotPending
	}
	s.auditAction(ctx, current.actions[index], "rejected")
	return s.rejectAction(current, index)
}

// ConnectionResult is the only credential delivery path. Browser conversations
// never serialize this data. The caller must also recheck resource ownership.
func (s *Service) ConnectionResult(ctx context.Context, actionID string) (json.RawMessage, error) {
	current, err := s.connectionSession(ctx)
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	index := actionIndex(current.actions, actionID)
	if index < 0 {
		return nil, ErrActionNotFound
	}
	action := current.actions[index]
	if _, err := authorizeAction(ctx, *current.connection, action); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), action.resultData...), nil
}

func (s *Service) OriginatingContext(ctx context.Context, id, principal, organization string) (context.Context, error) {
	s.mu.Lock()
	current := s.sessions[id]
	s.mu.Unlock()
	if current == nil {
		return nil, operatortool.ErrAccessDenied
	}
	current.mu.Lock()
	if current.connection == nil || current.connection.Identity.PrincipalID != principal || current.connection.Identity.OrganizationID != organization {
		current.mu.Unlock()
		return nil, operatortool.ErrAccessDenied
	}
	connection := *current.connection
	current.mu.Unlock()
	ctx = operatortool.WithConnection(ctx, connection)
	if err := s.CheckConnection(ctx); err != nil {
		return nil, err
	}
	return operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
}
