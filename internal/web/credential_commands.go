package web

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/store"
)

type credentialCommands struct{ server *Server }

func (s *Server) credentialExecutor() *operatoradmin.Executor {
	names := []string{operatortool.OrganizationSession}
	if s.apiKeys != nil && s.store != nil {
		names = append(names, operatortool.CredentialList, operatortool.CredentialCreate, operatortool.CredentialRotate, operatortool.CredentialRevoke)
	}
	return &operatoradmin.Executor{App: credentialCommands{s}, Chat: s.chat, Names: names}
}

func (a credentialCommands) Authorize(ctx context.Context, name string, in operatoradmin.Input, resource string) error {
	if name == operatortool.OrganizationSession {
		return nil
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationWide: true}); err != nil {
		return err
	}
	s := a.server
	if s.apiKeys == nil || s.store == nil {
		return operatoradmin.ErrUnavailable
	}
	// Sensitive receipts are usable only while the delivered credential is active.
	if resource != "" && (name == operatortool.CredentialCreate || name == operatortool.CredentialRotate) {
		key, err := s.store.APIKey(ctx, resource)
		if err != nil || key.RevokedAt != nil || key.ExpiresAt != nil && !key.ExpiresAt.After(apiNow()) {
			return operatortool.ErrAccessDenied
		}
	}
	if in.CredentialID != "" {
		if _, err := s.store.APIKey(ctx, in.CredentialID); err != nil {
			return operatortool.ErrAccessDenied
		}
	}
	return nil
}

func (a credentialCommands) Read(ctx context.Context, name string, in operatoradmin.Input) (any, error) {
	if name == operatortool.OrganizationSession {
		return struct {
			Principal    string `json:"principal_id"`
			Organization string `json:"organization_id"`
		}{operatortool.ConnectionIdentity(ctx).PrincipalID, operatortool.ConnectionIdentity(ctx).OrganizationID}, nil
	}
	keys, err := a.server.store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]apiKeyResponse, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, apiKeyJSON(key, apiNow()))
	}
	return operatoradmin.Page(rows, in), nil
}

func (a credentialCommands) Preview(ctx context.Context, name string, in operatoradmin.Input) (operatoradmin.Preview, error) {
	if name == operatortool.CredentialCreate {
		if _, err := apikey.ExpiresAt(in.ExpiresIn, apiNow()); err != nil {
			return operatoradmin.Preview{}, operatortool.ErrInvalidArguments
		}
		return operatoradmin.Preview{Summary: "Create API key", Current: in}, nil
	}
	key, err := a.server.store.APIKey(ctx, in.CredentialID)
	if err != nil {
		return operatoradmin.Preview{}, err
	}
	if name == operatortool.CredentialRotate {
		if _, err := apikey.GraceDuration(in.Grace); err != nil {
			return operatoradmin.Preview{}, operatortool.ErrInvalidArguments
		}
	}
	view := apiKeyJSON(key, apiNow())
	view.LastUsedAt = nil
	return operatoradmin.Preview{ResourceID: key.ID, Summary: name + ": " + key.Name, Current: struct {
		Key   apiKeyResponse      `json:"key"`
		Input operatoradmin.Input `json:"input"`
	}{view, in}}, nil
}

func (a credentialCommands) Execute(ctx context.Context, name string, in operatoradmin.Input, m mutation.Metadata) (operatoradmin.Output, error) {
	s := a.server
	records, ok := s.store.(store.OperatorMutations)
	if !ok {
		return operatoradmin.Output{}, operatoradmin.ErrUnavailable
	}
	if receipt, found, err := records.OperatorMutation(ctx, m); err != nil {
		return operatoradmin.Output{}, err
	} else if found {
		if receipt.Outcome != "succeeded" {
			return operatoradmin.Output{}, mutation.ErrUncertain
		}
		return operatoradmin.Output{ResourceID: receipt.ResourceID, URL: receipt.URL}, nil
	}
	reserved, err := records.ReserveOperatorMutation(ctx, m)
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if !reserved {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	claimed, err := records.ClaimOperatorMutation(ctx, m)
	if err != nil || !claimed {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	var created apikey.CreatedKey
	var output operatoradmin.Output
	switch name {
	case operatortool.CredentialCreate:
		created, err = s.apiKeys.Create(ctx, apikey.CreateRequest{Name: in.Name, Scopes: in.Scopes, ProjectIDs: in.ProjectIDs, ExpiresIn: in.ExpiresIn})
	case operatortool.CredentialRotate:
		created, err = s.apiKeys.Rotate(ctx, in.CredentialID, in.Grace)
	case operatortool.CredentialRevoke:
		err = s.apiKeys.Revoke(ctx, in.CredentialID)
		output.ResourceID = in.CredentialID
	default:
		return operatoradmin.Output{}, operatoradmin.ErrUnavailable
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if created.Key.ID != "" {
		output.ResourceID = created.Key.ID
		output.Data, err = json.Marshal(apiKeyCreatedResponse{Key: apiKeyJSON(created.Key, apiNow()), Token: created.Token})
		if err != nil {
			return operatoradmin.Output{}, err
		}
	}
	m.ResourceID = output.ResourceID
	// The durable receipt never stores the token or key hash.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := records.CompleteOperatorMutation(persistCtx, store.OperatorReceipt{Metadata: m, Outcome: "succeeded", CompletedAt: time.Now().UTC()}); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return output, nil
}

func (a credentialCommands) Audit(ctx context.Context, m mutation.Metadata, outcome string) {
	a.server.auditMutation(ctx, m, outcome)
}

func (s *Server) executeCredentialAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.credentialExecutor().ExecuteAction(ctx, action)
}

func (a credentialCommands) AuthorizeOutput(ctx context.Context, name string, in operatoradmin.Input, output operatoradmin.Output) error {
	if (name != operatortool.CredentialCreate && name != operatortool.CredentialRotate) || len(output.Data) == 0 {
		return nil
	}
	var delivered apiKeyCreatedResponse
	if json.Unmarshal(output.Data, &delivered) != nil {
		return operatortool.ErrAccessDenied
	}
	key, err := a.server.store.APIKey(ctx, output.ResourceID)
	if err != nil || key.KeyHash != apikey.HashToken(delivered.Token) {
		return operatortool.ErrAccessDenied
	}
	return nil
}
