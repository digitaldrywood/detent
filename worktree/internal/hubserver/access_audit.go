package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) recordAccessAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) error {
	row, err := s.prepareAccessAudit(ctx, identity, event, route, project, status)
	if err != nil || row == nil {
		return err
	}
	return s.accessWrite(ctx, row.OrganizationID, func(w tenantstore.Writer) error { _, err := w.RecordAccessAudit(ctx, *row); return err })
}

func (s *Service) prepareWorkItemMutationAudit(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions) (*accessstore.Audit, error) {
	identity := scope.Credential.Hosted
	if identity == nil {
		return nil, nil
	}
	actor := scope.Actor()
	metadata, ok := mutation.FromContext(ctx)
	switch {
	case ok:
		metadata.ActorKind, metadata.PrincipalID, metadata.ViaKey = actor.Kind, actor.PrincipalID, actor.ViaKey
	case actor.ViaKey != "":
		metadata = mutation.Metadata{PrincipalID: actor.PrincipalID, ActorKind: actor.Kind, ViaKey: actor.ViaKey, OrganizationID: string(scope.Organization), ProjectID: string(scope.Project), ResourceID: options.Item, Action: options.OperationID, Source: "api", Confirmation: "none", CorrelationID: hubapi.NewID("api")}
	default:
		return nil, nil
	}
	route := metadata.Action
	switch metadata.Source {
	case "mcp":
		route = "mcp " + route
	case "api":
	default:
		return nil, nil
	}
	return s.prepareAccessAudit(mutation.WithContext(ctx, metadata), identity, "action", route, string(scope.Project), http.StatusOK)
}

func (s *Service) prepareAccessAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) (*accessstore.Audit, error) {
	if identity == nil {
		return nil, nil
	}
	if _, ok := mutation.FromContext(ctx); !ok && status < http.StatusBadRequest {
		switch event {
		case "billing_viewed", "billing_exported":
			if identity.SupportActor == "" {
				return nil, nil
			}
		case "action", "administration":
			method, _, _ := strings.Cut(route, " ")
			switch method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				if identity.SupportActor == "" {
					return nil, nil
				}
			}
		}
	}
	actor := identity.Subject
	if identity.SupportActor != "" {
		actor = identity.SupportActor
	}
	now := s.config.Clock()
	account := decodeAccountFields(event)
	row := accessstore.Audit{OrganizationID: s.accessOrganization(), SessionID: identity.SessionID, ActualActor: actor, EffectiveUser: identity.Subject, Reason: identity.SupportReason, Event: event, Route: route, ProjectID: tracker.ProjectID(project), Status: status, StartedAt: identity.CreatedAt, ExpiresAt: identity.ExpiresAt, RecordedAt: now, RecordedAtNS: now.UnixNano(), AccountKind: account.Kind, AccountRetry: account.Retry}
	if m, ok := mutation.FromContext(ctx); ok {
		if m.ActorKind != "" && m.ActorKind != "human" {
			row.ActualActor = m.PrincipalID
		}
		m.PrincipalID, m.OrganizationID = row.ActualActor, string(s.accessOrganization())
		m.RetryIdentity, m.InputHash = "", ""
		raw, err := json.Marshal(struct {
			mutation.Metadata
			Outcome string `json:"outcome"`
		}{m, event})
		if err != nil {
			return nil, err
		}
		store := s.runDetailStore()
		if store == nil {
			return nil, artifact.ErrStorage
		}
		ref, err := store.StoreDetail(ctx, string(row.OrganizationID), raw)
		if err != nil {
			return nil, err
		}
		if ref.Validate(string(row.OrganizationID)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
			return nil, artifact.ErrIntegrity
		}
		verified, err := store.ReadDetail(ctx, string(row.OrganizationID), ref)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(verified, raw) {
			return nil, artifact.ErrIntegrity
		}
		row.Mutation = &ref
	}
	return &row, nil
}

func (s *Service) logoutAccessSession(ctx context.Context, hash string, identity *auth.HostedIdentity) (operatortool.SignOutResult, error) {
	if err := s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.RevokeAccessSession(ctx, hash, s.config.Clock()) }); err != nil {
		return operatortool.SignOutResult{}, err
	}
	outcome := operatortool.SignOutResult{SignedOut: true, ProviderConfirmed: true, AuditRecorded: true}
	if identity != nil {
		outcome.AuditRecorded = s.recordAccessAudit(ctx, identity, "session_ended", "/logout", "", http.StatusOK) == nil
		outcome.ProviderConfirmed = s.config.Hosted.Provider.RevokeSession(ctx, identity.SessionID) == nil
	}
	return outcome, nil
}

func (s *Service) recordEntryAccessKeyAudit(ctx context.Context, audit *hubscope.EntryKeyAudit, status int) error {
	if audit == nil || audit.ReadOnly() && status < http.StatusBadRequest {
		return nil
	}
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		return s.recordEntryAccessKeyAuditWith(ctx, w, audit, status)
	})
}

func (s *Service) recordEntryAccessKeyAuditWith(ctx context.Context, w tenantstore.Writer, audit *hubscope.EntryKeyAudit, status int) error {
	if audit == nil {
		return nil
	}
	if audit.ReadOnly() && status < http.StatusBadRequest {
		return nil
	}
	claims := audit.Claims
	now := s.config.Clock()
	if audit.RowID != 0 {
		return w.UpdateAccessKeyAudit(ctx, audit.RowID, claims.Key.ID, audit.Operation, tracker.ProjectID(audit.Project), audit.Resource, status)
	}
	var err error
	audit.RowID, err = w.RecordAccessAudit(ctx, accessstore.Audit{OrganizationID: s.accessOrganization(), SessionID: claims.Key.ID, ActualActor: claims.Subject, EffectiveUser: claims.Subject, Event: "key_call", Route: audit.Operation, ProjectID: tracker.ProjectID(audit.Project), Status: status, StartedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, RecordedAt: now, RecordedAtNS: now.UnixNano(), KeyID: claims.Key.ID, KeyKind: claims.Key.Kind, KeyOwner: claims.Subject, Resource: audit.Resource})
	return err
}
