package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrProjectParticipants = errors.New("project lifecycle participants are unavailable")

func projectRecordActor(scope nativeScope) tenantstore.ProjectActor {
	actor := scope.actor()
	result := tenantstore.ProjectActor{CredentialID: scope.credential.ID, PrincipalID: actor.PrincipalID}
	if scope.credential.Hosted != nil {
		identity := scope.credential.Hosted
		result.HostedSubject = identity.Subject
		actual := identity.Subject
		if identity.SupportActor != "" {
			actual = identity.SupportActor
		}
		result.Audit = accessstore.Audit{OrganizationID: scope.organization, SessionID: identity.SessionID, ActualActor: actual, EffectiveUser: identity.Subject, Reason: identity.SupportReason, StartedAt: identity.CreatedAt, ExpiresAt: identity.ExpiresAt}
	}
	return result
}

type tenantProjectParticipants struct {
	service *Service
}

func (p tenantProjectParticipants) CreateProject(ctx context.Context, w tenantstore.Writer, actor tenantstore.ProjectActor, record projectstore.Record) error {
	s := p.service
	now := s.config.now().UTC()
	if s.database.plans() != nil {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, w, now)
		if err != nil {
			return err
		}
		if err := requireHostedFeatureInEntitlement(entitlement, "collaboration"); err != nil {
			return err
		}
		if limit, limited := entitlement.Allowances["projects"]; limited && limit < math.MaxInt64 {
			used, err := w.ProjectAllowanceUsage(ctx, limit)
			if err != nil {
				return err
			}
			if used > limit {
				return &hostedLimitError{Resource: "projects", Allowance: limit, Consumption: used - 1}
			}
		}
	}
	if err := w.InitializeProjectAccess(ctx, record.Core.ID, actor.CredentialID); err != nil {
		return err
	}
	if err := s.applyRunnerTenantUsage(ctx, w, now, "project-create/"+string(record.Core.ID), false, false); err != nil {
		return err
	}
	audit := actor.Audit
	audit.OrganizationID, audit.ProjectID = record.Core.OrganizationID, record.Core.ID
	audit.Event, audit.Route, audit.Status = "project_created", "create_hosted_project", http.StatusCreated
	audit.RecordedAt, audit.RecordedAtNS = now, now.UnixNano()
	_, err := w.RecordAccessAudit(ctx, audit)
	return err
}

func (p tenantProjectParticipants) UpdateProject(ctx context.Context, w tenantstore.Writer, actor tenantstore.ProjectActor, _ projectstore.Record, after projectstore.Record) error {
	s := p.service
	now := s.config.now().UTC()
	if s.database.plans() != nil {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, w, now)
		if err != nil {
			return err
		}
		if err := requireHostedFeatureInEntitlement(entitlement, "collaboration"); err != nil {
			return err
		}
	}
	if err := s.applyRunnerTenantUsage(ctx, w, now, "project-update/"+string(after.Core.ID)+"/"+strconv.FormatInt(after.Settings.Revision, 10), false, false); err != nil {
		return err
	}
	audit := actor.Audit
	audit.OrganizationID, audit.ProjectID = after.Core.OrganizationID, after.Core.ID
	audit.Event, audit.Route, audit.Status = "project_updated", "update_project_integration", http.StatusOK
	audit.RecordedAt, audit.RecordedAtNS = now, now.UnixNano()
	_, err := w.RecordAccessAudit(ctx, audit)
	return err
}

func (p tenantProjectParticipants) RetireProject(ctx context.Context, w tenantstore.Writer, actor tenantstore.ProjectActor, record projectstore.Record, now time.Time) error {
	s := p.service
	now = now.UTC()
	if s.database.plans() != nil {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, w, now)
		if err != nil {
			return err
		}
		if err := requireHostedFeatureInEntitlement(entitlement, "collaboration"); err != nil {
			return err
		}
	}
	if err := s.applyRunnerTenantUsage(ctx, w, now, "project-retire/"+string(record.Core.ID)+"/"+strconv.FormatInt(record.Settings.Revision, 10), false, false); err != nil {
		return err
	}
	cleaner, ok := w.(accessstore.ProjectAccessCleaner)
	if !ok {
		return ErrProjectParticipants
	}
	if err := cleaner.DeleteAccessProjectGrants(ctx, record.Core.ID); err != nil {
		return err
	}
	audit := actor.Audit
	audit.OrganizationID, audit.ProjectID = record.Core.OrganizationID, record.Core.ID
	audit.Event, audit.Route, audit.Status = "project_retired", "delete_cloud_project", http.StatusNoContent
	audit.RecordedAt, audit.RecordedAtNS = now, now.UnixNano()
	_, err := w.RecordAccessAudit(ctx, audit)
	return err
}

func (s *Service) createHostedTenantProject(ctx context.Context, credential apiCredential, name string, states []tracker.NativeState) (string, error) {
	if states == nil {
		states = HostedProjectStates()
	}
	key, err := nativeCommandHash([]string{s.config.Hosted.OrganizationID, credential.Hosted.Subject, name})
	if err != nil {
		return "", err
	}
	request := createNativeProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Name: name, States: states}
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}
	raw, err := s.executeProjectRecordMutation(ctx, scope, nativeCommandOptions{OperationID: "create_hosted_project", Feature: "collaboration"}, request.Mutation, request)
	if err != nil {
		return "", err
	}
	var record tracker.NativeProject
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", err
	}
	return string(record.ID), nil
}
