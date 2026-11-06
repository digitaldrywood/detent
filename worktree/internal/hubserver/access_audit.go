package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) recordAccessAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) error {
	if identity == nil {
		return nil
	}
	actor := identity.Subject
	if identity.SupportActor != "" {
		actor = identity.SupportActor
	}
	now := s.config.now()
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
			return err
		}
		store := s.runDetailStore()
		if store == nil {
			return artifact.ErrStorage
		}
		ref, err := store.StoreDetail(ctx, string(row.OrganizationID), raw)
		if err != nil {
			return err
		}
		if ref.Validate(string(row.OrganizationID)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
			return artifact.ErrIntegrity
		}
		verified, err := store.ReadDetail(ctx, string(row.OrganizationID), ref)
		if err != nil {
			return err
		}
		if !bytes.Equal(verified, raw) {
			return artifact.ErrIntegrity
		}
		row.Mutation = &ref
	}
	return s.accessWrite(ctx, row.OrganizationID, func(w tenantstore.Writer) error { _, err := w.RecordAccessAudit(ctx, row); return err })
}

func (s *Service) logoutAccessSession(ctx context.Context, hash string, identity *auth.HostedIdentity) (operatortool.SignOutResult, error) {
	if err := s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.RevokeAccessSession(ctx, hash, s.config.now()) }); err != nil {
		return operatortool.SignOutResult{}, err
	}
	outcome := operatortool.SignOutResult{SignedOut: true, ProviderConfirmed: true, AuditRecorded: true}
	if identity != nil {
		outcome.AuditRecorded = s.recordAccessAudit(ctx, identity, "session_ended", "/logout", "", http.StatusOK) == nil
		outcome.ProviderConfirmed = s.config.Hosted.Provider.RevokeSession(ctx, identity.SessionID) == nil
	}
	return outcome, nil
}

func (s *Service) recordEntryAccessKeyAudit(ctx context.Context, audit *entryKeyAudit, status int) error {
	if audit.readOnly() && status < http.StatusBadRequest {
		return nil
	}
	claims := audit.claims
	now := s.config.now()
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if audit.rowID != 0 {
			return w.UpdateAccessKeyAudit(ctx, audit.rowID, claims.Key.ID, audit.operation, tracker.ProjectID(audit.project), audit.resource, status)
		}
		var err error
		audit.rowID, err = w.RecordAccessAudit(ctx, accessstore.Audit{OrganizationID: s.accessOrganization(), SessionID: claims.Key.ID, ActualActor: claims.Subject, EffectiveUser: claims.Subject, Event: "key_call", Route: audit.operation, ProjectID: tracker.ProjectID(audit.project), Status: status, StartedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, RecordedAt: now, RecordedAtNS: now.UnixNano(), KeyID: claims.Key.ID, KeyKind: claims.Key.Kind, KeyOwner: claims.Subject, Resource: audit.resource})
		return err
	})
}
