package chat

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func authorizeAction(ctx context.Context, connection operatortool.Connection, action Action) (context.Context, error) {
	requirement := operatortool.Requirement{
		Scope: apikey.ScopeWrite, OrganizationID: action.OrganizationID, ProjectID: action.ProjectID,
	}
	if operatortool.IsAdministration(string(action.Kind)) {
		requirement = operatortool.AdministrationRequirement(string(action.Kind))
		requirement.OrganizationID, requirement.ProjectID = action.OrganizationID, action.ProjectID
	}
	switch string(action.Kind) {
	case "apply_local_project_policy", "resume_local_project", "drain_local_project", "detach_local_project":
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
	_, err := s.attachConnection(ctx)
	return err
}

func (s *Service) attachConnection(ctx context.Context) (*session, error) {
	connection := operatortool.CurrentConnection(ctx)
	if connection.ID == "" || len(connection.ID) > 256 || !connection.Identity.Valid() || connection.Resolve == nil {
		return nil, operatortool.ErrAccessDenied
	}
	current := s.ensureSession(connection.ID)
	current.mu.Lock()
	defer current.mu.Unlock()
	if err := s.restoreSession(ctx, connection.ID, current); err != nil {
		return nil, err
	}
	if current.connection != nil {
		if current.connection.Identity != connection.Identity {
			return nil, operatortool.ErrAccessDenied
		}
		connection.Client = current.connection.Client
		current.connection = &connection
		return current, nil
	}
	current.connection = &connection
	if err := s.persistSession(ctx, current); err != nil {
		current.connection = nil
		return nil, err
	}
	return current, nil
}

func (s *Service) connectionSession(ctx context.Context) (*session, error) {
	connection := operatortool.CurrentConnection(ctx)
	current := s.session(connection.ID)
	if current == nil {
		var err error
		current, err = s.attachConnection(ctx)
		if err != nil {
			return nil, err
		}
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
		if (previous.Kind == ActionKind(operatortool.BillingCheckout) || previous.Kind == ActionKind(operatortool.CreditCheckout)) && previous.Status == ActionFailed && s.actions != nil {
			result, executeErr := s.actions.ExecuteAction(executionContext, previous)
			outcome := "succeeded"
			if executeErr != nil {
				outcome = "failed"
			}
			s.auditAction(executionContext, previous, outcome)
			_, err = s.resolveExecution(executionContext, current, index, result, executeErr)
			if err != nil {
				return Action{}, true, err
			}
			previous = current.actions[index]
		}
		return cloneActions([]Action{previous})[0], true, nil
	}
	return Action{}, false, nil
}

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
	action.Mutation.Confirmation = "none"
	var idErr error
	action.ID, idErr = s.newID()
	if idErr != nil {
		return Action{}, idErr
	}
	action.CreatedAt = s.now().UTC()
	ctx, err := authorizeAction(ctx, *current.connection, action)
	if err != nil {
		return Action{}, err
	}
	if s.actions == nil {
		return Action{}, ErrUnavailable
	}
	current.actions = append(current.actions, cloneActions([]Action{action})[0])
	index := len(current.actions) - 1
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
	_, err = s.resolveExecution(ctx, current, index, result, executeErr)
	return cloneActions(current.actions[index : index+1])[0], err
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
