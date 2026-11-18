package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) validateAccessInvitationGrants(ctx context.Context, r tenantstore.Reader, credential hubapi.APICredential, email string, grants []hostedMemberGrant) error {
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return operatortool.ErrAccessDenied
	}
	if err := s.authorizeAccessScope(ctx, r, hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}, true); err != nil {
		return err
	}
	tenant, err := r.AccessTenant(ctx)
	if err != nil {
		return hubscope.OperatorAuthorizationError(err)
	}
	if tenant.ProviderID != credential.Hosted.OrganizationID {
		return operatortool.ErrAccessDenied
	}
	selected := make(map[string]bool, len(grants))
	for _, grant := range grants {
		if !hostedplans.SafeID(grant.ProjectID) || selected[grant.ProjectID] {
			return hubapi.NativePublicInvalid("Select each project only once")
		}
		selected[grant.ProjectID] = true
		project, err := r.ProjectRecord(ctx, tracker.ProjectID(grant.ProjectID))
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if project.Core.DeletedAt != nil {
			return operatortool.ErrAccessDenied
		}
		if credential.EntryKey != nil && !credential.EntryKey.Allows(grant.ProjectID) {
			return operatortool.ErrAccessDenied
		}
		projectGrant, err := r.AccessProjectGrant(ctx, credential.Hosted.Subject, tracker.ProjectID(grant.ProjectID), credential.HostedRole)
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if grant.Write && !projectGrant.CanWrite || grant.Runner && !projectGrant.ManageRunner {
			return &hubapi.NativeError{Code: "forbidden", Message: "You can only grant project access that you hold", Status: 403, PublicMessage: true}
		}
	}
	var after string
	for {
		members, err := r.AccessMembersByEmail(ctx, strings.ToLower(email), after, 200)
		if err != nil {
			return err
		}
		for _, member := range members {
			projectAfter := tracker.ProjectID("")
			for {
				rows, err := r.AccessProjectGrants(ctx, member.UserID, projectAfter, 200)
				if err != nil {
					return err
				}
				for _, row := range rows {
					if !selected[string(row.ProjectID)] {
						if err := s.validateAccessInvitationGrant(ctx, r, credential, row.ProjectID, false, false); err != nil {
							return err
						}
					}
					projectAfter = row.ProjectID
				}
				if len(rows) < 200 {
					break
				}
			}
			credentialAfter := ""
			for {
				credentials, err := r.AccessMemberCredentials(ctx, member.UserID, credentialAfter, 200)
				if err != nil {
					return err
				}
				for _, token := range credentials {
					tokenAfter := tracker.ProjectID("")
					for {
						rows, err := r.AccessTokenGrants(ctx, token.ID, tokenAfter, 200)
						if err != nil {
							return err
						}
						for _, row := range rows {
							if !selected[string(row.ProjectID)] {
								if err := s.validateAccessInvitationGrant(ctx, r, credential, row.ProjectID, false, false); err != nil {
									return err
								}
							}
							tokenAfter = row.ProjectID
						}
						if len(rows) < 200 {
							break
						}
					}
					credentialAfter = token.ID
				}
				if len(credentials) < 200 {
					break
				}
			}
		}
		if len(members) < 200 {
			return nil
		}
		after = members[len(members)-1].UserID
	}
}

func (s *Service) validateAccessInvitationGrant(ctx context.Context, r tenantstore.Reader, credential hubapi.APICredential, project tracker.ProjectID, write, runner bool) error {
	row, err := r.ProjectRecord(ctx, project)
	if err != nil {
		return hubscope.OperatorAuthorizationError(err)
	}
	if row.Core.DeletedAt != nil {
		return operatortool.ErrAccessDenied
	}
	if credential.EntryKey != nil {
		if credential.EntryKey.Allows(string(project)) {
			return nil
		}
		return operatortool.ErrAccessDenied
	}
	if credential.Scope == hubapi.APIScopeAdmin && !credential.NativeOnly {
		return nil
	}
	grant, err := r.AccessProjectGrant(ctx, credential.Hosted.Subject, project, credential.HostedRole)
	if err != nil {
		return hubscope.OperatorAuthorizationError(err)
	}
	if write && !grant.CanWrite || runner && !grant.ManageRunner {
		return &hubapi.NativeError{Code: "forbidden", Message: "You can only grant project access that you hold", Status: 403, PublicMessage: true}
	}
	tokenID := credential.ID
	if credential.Hosted != nil && credential.HostedKeyScope != "" && credential.HostedProjectAccess == hubapi.HostedProjectsAll {
		tokenID = credential.HostedPrincipal
	}
	if _, err := r.AccessTokenGrant(ctx, tokenID, project, credential.HostedRole); err != nil {
		return hubscope.OperatorAuthorizationError(err)
	}
	if credential.Hosted != nil {
		if _, err := r.AccessTokenGrant(ctx, credential.HostedPrincipal, project, credential.HostedRole); err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
	}
	return nil
}

func (s *Service) pendingAccessInvitationFor(ctx context.Context, credential hubapi.APICredential, id string) (hostedInvitationView, error) {
	var view hostedInvitationView
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return view, operatortool.ErrAccessDenied
	}
	var row accessstore.Invitation
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if tenant.ProviderID != credential.Hosted.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		row, err = r.AccessInvitation(ctx, id)
		if err != nil {
			if classified := hubscope.OperatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
				return classified
			}
			return hubapi.NativeNotFound()
		}
		if row.AcceptedUserID != "" || !row.ExpiresAt.After(s.config.Clock()) {
			return hubapi.NativeNotFound()
		}
		if row.Role == "owner" && credential.HostedRole != "owner" {
			return &hubapi.NativeError{Code: "forbidden", Message: "You cannot change owner invitations", Status: 403, PublicMessage: true}
		}
		view, err = s.accessInvitationView(ctx, r, row)
		return err
	})
	if err != nil {
		return view, err
	}
	invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, id)
	if err != nil {
		return view, err
	}
	if invitation.ID != id || invitation.OrganizationID != credential.Hosted.OrganizationID || !strings.EqualFold(invitation.Email, row.Email) || invitation.State != "pending" || !invitation.ExpiresAt.After(s.config.Clock()) {
		return view, hubapi.NativeNotFound()
	}
	if invitation.ExpiresAt.Before(row.ExpiresAt) {
		view.ExpiresAt = hubapi.FormatHubTime(invitation.ExpiresAt)
	}
	return view, nil
}

func (s *Service) createAccessInvitation(ctx context.Context, credential hubapi.APICredential, email, role string, grants []hostedMemberGrant) (view hostedInvitationView, resultErr error) {
	if err := s.validateHostedInvitationGrants(ctx, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	invitation, err := s.config.Hosted.Provider.Invite(ctx, credential.Hosted.OrganizationID, email, role, credential.Hosted.Subject)
	if err != nil || invitation.ID == "" || invitation.OrganizationID != credential.Hosted.OrganizationID || !strings.EqualFold(invitation.Email, email) || invitation.State != "pending" {
		return hostedInvitationView{}, errors.Join(mutation.ErrUncertain, err, nativeUnavailable("The invitation could not be sent"))
	}
	now := s.config.Clock()
	expires := invitation.ExpiresAt
	if expires.IsZero() {
		expires = now.Add(24 * time.Hour)
	}
	view = hostedInvitationView{ID: invitation.ID, Email: email, Role: role, CreatedAt: hubapi.FormatHubTime(now), ExpiresAt: hubapi.FormatHubTime(expires), Grants: append([]hostedMemberGrant{}, grants...)}
	err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		scope := hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := s.validateAccessInvitationGrants(ctx, w, credential, email, grants); err != nil {
			return err
		}
		row := accessstore.Invitation{OrganizationID: s.accessOrganization(), ID: invitation.ID, Email: strings.ToLower(email), Role: role, CreatedAt: now, ExpiresAt: expires}
		if err := w.CreateAccessInvitation(ctx, row); err != nil {
			return err
		}
		for _, grant := range grants {
			if err := w.CreateAccessInvitationGrant(ctx, accessstore.InvitationGrant{OrganizationID: s.accessOrganization(), InvitationID: invitation.ID, ProjectID: tracker.ProjectID(grant.ProjectID), CanWrite: grant.Write, ManageRunner: grant.Runner}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return hostedInvitationView{}, errors.Join(mutation.ErrUncertain, err)
	}
	return view, nil
}

func (s *Service) editAccessInvitation(ctx context.Context, credential hubapi.APICredential, id string, grants []hostedMemberGrant) error {
	if grants == nil {
		grants = []hostedMemberGrant{}
	}
	view, err := s.pendingAccessInvitationFor(ctx, credential, id)
	if err != nil {
		return err
	}
	err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		scope := hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return fmt.Errorf("authorize invitation edit: %w", err)
		}
		current, err := w.AccessInvitation(ctx, id)
		if err != nil {
			if classified := hubscope.OperatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
				return classified
			}
			return hubapi.NativeNotFound()
		}
		if current.AcceptedUserID != "" || !current.ExpiresAt.After(s.config.Clock()) || current.Email != view.Email || current.Role != view.Role {
			return hubapi.NativeNotFound()
		}
		currentView, err := s.accessInvitationView(ctx, w, current)
		if err != nil {
			return err
		}
		for _, old := range currentView.Grants {
			if err := s.validateAccessInvitationGrant(ctx, w, credential, tracker.ProjectID(old.ProjectID), false, false); err != nil {
				return fmt.Errorf("validate current invitation grant: %w", err)
			}
		}
		if err := s.validateAccessInvitationGrants(ctx, w, credential, view.Email, grants); err != nil {
			return fmt.Errorf("validate replacement invitation grants: %w", err)
		}
		rows := make([]accessstore.InvitationGrant, 0, len(grants))
		for _, grant := range grants {
			rows = append(rows, accessstore.InvitationGrant{OrganizationID: s.accessOrganization(), InvitationID: id, ProjectID: tracker.ProjectID(grant.ProjectID), CanWrite: grant.Write, ManageRunner: grant.Runner})
		}
		if err := w.ReplaceAccessInvitationGrants(ctx, id, rows); err != nil {
			return fmt.Errorf("replace invitation grants: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("write invitation edit: %w", err)
	}
	return nil
}

func (s *Service) revokeAccessInvitation(ctx context.Context, credential hubapi.APICredential, id string) error {
	var current accessstore.Invitation
	if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}, true); err != nil {
			return err
		}
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if credential.Hosted == nil || tenant.ProviderID != credential.Hosted.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		current, err = r.AccessInvitation(ctx, id)
		return err
	}); err != nil {
		return err
	}
	if current.AcceptedUserID != "" || !current.ExpiresAt.After(s.config.Clock()) {
		return hubapi.NativeNotFound()
	}
	if current.Role == "owner" && credential.HostedRole != "owner" {
		return operatortool.ErrAccessDenied
	}
	audit, recordAudit, err := s.prepareAccessAudit(ctx, credential.Hosted, "action", "DELETE "+hostedOrganizationBase+"/members/invitations/:invitation", "", http.StatusNoContent)
	if err != nil {
		return err
	}
	if err := auth.RevokeInvitationID(ctx, s.config.Hosted.Provider, id); err != nil {
		return errors.Join(operatortool.ErrProviderUnavailable, err)
	}
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}, true); err != nil {
			return err
		}
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if credential.Hosted == nil || tenant.ProviderID != credential.Hosted.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		if _, err := w.DeleteAccessInvitation(ctx, id); err != nil {
			return err
		}
		if recordAudit {
			_, err := w.RecordAccessAudit(ctx, audit)
			return err
		}
		return nil
	})
}

func (s *Service) reserveAccessInvitationSeat(ctx context.Context, credential hubapi.APICredential, email string) (bool, error) {
	runtimePlans := s.database.plansService().Plans()
	if runtimePlans == nil {
		return false, errors.New("hosted entitlements are disabled")
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().Entitlement(ctx, nil, s.config.Clock()))
	if err != nil {
		return false, err
	}
	limit := entitlement.Allowances["members"]
	if limit < 0 {
		limit = 0
	}
	created := false
	var limitErr *hostedplans.LimitError
	err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}, true); err != nil {
			return err
		}
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return hubscope.OperatorAuthorizationError(err)
		}
		if credential.Hosted == nil || tenant.ProviderID != credential.Hosted.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		created, err = w.ReserveAccessMemberReservation(ctx, accessstore.MemberReservation{OrganizationID: s.accessOrganization(), Email: strings.ToLower(email), ExpiresAt: s.config.Clock().Add(time.Duration(runtimePlans.InvitationSeconds) * time.Second)}, s.config.Clock(), limit)
		if errors.Is(err, accessstore.ErrSeatLimit) {
			members, reservations, countErr := w.AccessMemberSeatCounts(ctx, s.config.Clock())
			if countErr != nil {
				return countErr
			}
			limitErr = &hostedplans.LimitError{Resource: "members", Allowance: limit, Consumption: members + reservations}
			return nil
		}
		return err
	})
	if err == nil && limitErr != nil {
		return false, limitErr
	}
	return created, err
}

func nativeUnavailable(message string) error {
	return &hubapi.NativeError{Code: "unavailable", Message: message, Status: 503}
}
