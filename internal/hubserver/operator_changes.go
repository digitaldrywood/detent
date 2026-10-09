package hubserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hubOperatorExecutor struct{ service *Service }

func (e hubOperatorExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.service.operatorChat.AttachConnection(ctx)
}
func (e hubOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := e.service.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	out := []operatortool.Definition{}
	for _, d := range operatortool.ChangeCatalog() {
		if d.Name == operatortool.ArtifactLibrary {
			continue // The library belongs to the daemon dashboard application.
		}
		scope := apikey.ScopeRead
		if !d.Annotations.ReadOnly && d.Name != operatortool.ArtifactAccess {
			scope = apikey.ScopeWrite
		}
		if d.Name == operatortool.ApproveChangeReviewPolicy || d.Name == operatortool.BindArtifactService || d.Name == operatortool.ReviewChange && e.service.config.Hosted != nil {
			scope = apikey.ScopeAdmin
		}
		requirement := operatortool.Requirement{Scope: scope}
		if scope == apikey.ScopeWrite {
			requirement.ResourceKind = "work_item"
		}
		if _, err := e.service.authorizeCatalog(ctx, requirement); err == nil {
			out = append(out, d)
		}
	}
	return out, nil
}
func (e hubOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (result operatortool.Result, resultErr error) {
	s := e.service
	if call.Name == operatortool.ConnectionInfo || call.Name == operatortool.ActionResult {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
			return result, err
		}
		if err := s.operatorChat.CheckConnection(ctx); err != nil {
			return result, err
		}
		id := operatortool.CurrentConnection(ctx).ID
		if call.Name == operatortool.ConnectionInfo {
			if operatortool.DecodeArguments(call.Arguments, &struct{}{}) != nil {
				return result, operatortool.ErrInvalidArguments
			}
			c := s.operatorChat.Conversation(id)
			return hubChangeResult(struct {
				ID           string `json:"connection_id"`
				Organization string `json:"organization_id"`
			}{id, c.OrganizationID})
		}
		var args struct {
			ID string `json:"action_id"`
		}
		if operatortool.DecodeArguments(call.Arguments, &args) != nil || args.ID == "" || len(args.ID) > 256 {
			return result, operatortool.ErrInvalidArguments
		}
		action, ok := s.operatorChat.Action(id, args.ID)
		if !ok {
			return result, operatortool.ErrAccessDenied
		}
		if _, _, err := s.hubChangeAuthority(ctx, string(action.Kind), action.Arguments); err != nil {
			return result, err
		}
		return s.hubActionResult(action)
	}
	args, err := operatortool.DecodeChangeArguments(call.Name, call.Arguments)
	if err != nil {
		return result, err
	}
	definition, _ := operatortool.ChangeDefinition(call.Name)
	var m mutation.Metadata
	outcome := "failed"
	if !definition.Annotations.ReadOnly {
		m, err = changeMutationMetadata(ctx, call.Name, args, call.Arguments)
		if err != nil {
			return result, err
		}
		defer func() { s.hubChangeAudit(ctx, m, outcome) }()
	}
	ctx, app, err := s.hubChangeAuthority(ctx, call.Name, call.Arguments)
	if err != nil {
		return result, err
	}
	if definition.Annotations.ReadOnly {
		value, err := app.ReadChange(ctx, call.Name, args)
		if err != nil {
			return result, hubSafeChangeError(fmt.Errorf("read change: %w", err))
		}
		return operatortool.BoundedChangeResult(value)
	}
	value, err := app.MutateChange(mutation.WithContext(ctx, m), call.Name, args)
	if err != nil {
		return result, hubSafeChangeError(err)
	}
	outcome = "succeeded"
	return operatortool.BoundedChangeResult(value)
}

func (s *Service) hubChangeAuthority(ctx context.Context, name string, raw json.RawMessage) (context.Context, operatortool.ChangeApplication, error) {
	args, err := operatortool.DecodeChangeArguments(name, raw)
	if err != nil {
		return ctx, nil, err
	}
	d, _ := operatortool.ChangeDefinition(name)
	scope := apikey.ScopeRead
	if !d.Annotations.ReadOnly && name != operatortool.ArtifactAccess {
		scope = apikey.ScopeWrite
	}
	if name == operatortool.ApproveChangeReviewPolicy || name == operatortool.BindArtifactService || name == operatortool.ReviewChange && s.config.Hosted != nil {
		scope = apikey.ScopeAdmin
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ProjectID: args.ProjectID})
	if err != nil {
		return ctx, nil, err
	}
	app, err := operatortool.CurrentChanges(ctx)
	if err != nil {
		return ctx, nil, err
	}
	// Resolve the complete nested resource before replay, preview or side effect.
	readName := operatortool.GetChangeReviewPolicy
	if args.ItemID != "" {
		readName = operatortool.ListChanges
	}
	if args.ChangeID != "" {
		readName = operatortool.GetChange
	}
	if name == operatortool.ArtifactAccess {
		readName = operatortool.GetArtifactReference
	}
	if args.ItemID == "" {
		return ctx, app, nil
	}
	value, err := app.ReadChange(ctx, readName, args)
	if err != nil {
		return ctx, nil, hubSafeChangeError(fmt.Errorf("resolve change authority: %w", err))
	}
	if name == operatortool.ArtifactAccess {
		matched := false
		for _, r := range value.Artifacts {
			if r.ArtifactID == args.ArtifactID && r.Revision == args.Revision && r.SHA256 == args.SHA256 {
				matched = true
			}
		}
		if !matched {
			return ctx, nil, operatortool.ErrAccessDenied
		}
	}
	return ctx, app, nil
}
func changeMutationMetadata(ctx context.Context, name string, args operatortool.ChangeArguments, raw json.RawMessage) (mutation.Metadata, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return mutation.Metadata{}, operatortool.ErrServiceUnavailable
	}
	i := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: i.PrincipalID, OrganizationID: i.OrganizationID, ProjectID: args.ProjectID, ResourceID: args.ItemID, Action: name, Source: "mcp", Confirmation: "none", CorrelationID: hex.EncodeToString(b[:])}
	return m.Bind(args.RequestID, raw)
}
func (s *Service) hubChangeAudit(ctx context.Context, m mutation.Metadata, outcome string) {
	m.RetryIdentity = ""
	m.InputHash = ""
	s.config.Logger.InfoContext(ctx, "operator mutation", "audit", m, "outcome", outcome)
}
func (e hubOperatorExecutor) AuditAction(ctx context.Context, action chat.Action, outcome string) {
	e.service.hubChangeAudit(ctx, action.Mutation, outcome)
}
func (e hubOperatorExecutor) ExecuteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	ctx, app, err := e.service.hubChangeAuthority(ctx, string(action.Kind), action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	args, err := operatortool.DecodeChangeArguments(string(action.Kind), action.Arguments)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	if action.Kind == chat.ActionKind(operatortool.ApproveChangeReviewPolicy) {
		current, err := app.ReadChange(ctx, operatortool.GetChangeReviewPolicy, args)
		if err != nil || current.Policy == nil || current.Policy.ID != action.CurrentState {
			return chat.ActionExecution{}, mutation.ErrConflict
		}
	}
	value, err := app.MutateChange(mutation.WithContext(ctx, action.Mutation), string(action.Kind), args)
	if err != nil {
		return chat.ActionExecution{}, hubSafeChangeError(err)
	}
	return chat.ActionExecution{Message: "Change command completed.", ResourceID: args.ItemID, Identifier: value.ChangeID, URL: value.URL}, nil
}

func (s *Service) hubActionResult(action chat.Action) (operatortool.Result, error) {
	return hubChangeResult(struct {
		Action     chat.Action       `json:"preview"`
		ID         string            `json:"action_id"`
		Status     chat.ActionStatus `json:"status"`
		ResultTool string            `json:"result_tool"`
	}{action, action.ID, action.Status, operatortool.ActionResult})
}
func hubChangeResult(value any) (operatortool.Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return operatortool.Result{}, operatortool.ErrServiceUnavailable
	}
	return operatortool.Result{Content: raw}, nil
}
func hubSafeChangeError(err error) error {
	if errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, sql.ErrNoRows) {
		return operatortool.ErrAccessDenied
	}
	if errors.Is(err, mutation.ErrConflict) || isNativeConflict(err) {
		return mutation.ErrConflict
	}
	var failure *nativeError
	if errors.As(err, &failure) {
		if failure.status == 404 || failure.status == 403 || failure.status == 401 {
			return operatortool.ErrAccessDenied
		}
		if failure.Code == "idempotency_conflict" {
			return mutation.ErrConflict
		}
	}
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return operatortool.ErrInvalidArguments
	}
	return fmt.Errorf("%w: %w", operatortool.ErrServiceUnavailable, err)
}
