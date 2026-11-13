package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// These commands are shared with dashboard adapters. They own persistence and
// provider effects; MCP only supplies typed input and trusted mutation context.
func (s *Service) hostedMembersFor(ctx context.Context, credential hubapi.APICredential) (hostedMembersResponse, error) {
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	emails, err := s.hostedMemberEmails(ctx)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	grants, err := s.hostedMemberGrants(ctx)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	response := hostedMembersResponse{Roles: auth.OrganizationRoles(), Members: []hostedMemberView{}, Invitations: []hostedInvitationView{}}
	for _, member := range memberships {
		if member.OrganizationID != credential.Hosted.OrganizationID || member.Status != "active" {
			continue
		}
		// Local removal/downgrade is authoritative even before provider propagation.
		var active bool
		var role string
		var err error
		active, role, err = s.accessLocalMember(ctx, member.UserID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return response, err
		}
		if err == nil && !active {
			continue
		}
		if err == nil {
			member.Role.Slug = hubapi.LesserHostedRole(member.Role.Slug, role)
		}
		response.MemberCount++
		if !manage && member.UserID != credential.Hosted.Subject {
			continue
		}
		view, err := s.hostedMemberIdentity(ctx, member, emails)
		if err != nil {
			return response, err
		}
		if list, ok := grants[member.UserID]; ok {
			view.Grants = list
		}
		response.Members = append(response.Members, view)
	}
	sortHostedMembers(response.Members)
	if manage {
		response.Invitations, err = s.hostedPendingInvitations(ctx)
	}
	return response, err
}

func (s *Service) inviteHostedMemberFor(ctx context.Context, credential hubapi.APICredential, email, role, key string, grants []hostedMemberGrant) (hostedInvitationView, error) {
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return hostedInvitationView{}, operatortool.ErrAccessDenied
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, email) {
		return hostedInvitationView{}, operatortool.ErrInvalidArguments
	}
	grants = append([]hostedMemberGrant{}, grants...)
	sort.Slice(grants, func(i, j int) bool { return grants[i].ProjectID < grants[j].ProjectID })
	if err := s.validateHostedInvitationGrants(ctx, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	command := hostedCommand{actor: credential.ID, operation: "POST /api/v2/organizations/" + s.config.Hosted.OrganizationID + "/members/invitations", key: key, input: struct {
		Email, Role string
		Grants      []hostedMemberGrant `json:",omitempty"`
	}{email, role, grants}}
	claimed, raw, err := s.claimHostedOperation(ctx, command)
	if err != nil {
		return hostedInvitationView{}, err
	}
	if !claimed {
		var view hostedInvitationView
		err := json.Unmarshal(raw, &view)
		return view, err
	}
	_, err = s.reserveHostedInvitationSeatFor(ctx, credential, email)
	if err != nil {
		s.abandonHostedOperation(ctx, command)
		return hostedInvitationView{}, err
	}
	view, err := s.sendReservedHostedInvitationFor(ctx, credential, email, role, grants)
	if err != nil {
		return hostedInvitationView{}, err
	}

	if _, err := s.completeHostedOperation(ctx, command, view); err != nil {
		return hostedInvitationView{}, err
	}
	return view, nil
}

func (s *Service) removeHostedMemberFor(ctx context.Context, credential hubapi.APICredential, id string) error {
	member, err := s.hostedManagedMemberFor(ctx, credential, id, true)
	if err != nil {
		return err
	}
	scope := hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential, RequireHostedAdmin: true}
	audit, err := s.prepareAccessAudit(ctx, credential.Hosted, "action", "DELETE "+hostedOrganizationBase+"/members/:member", "", http.StatusNoContent)
	if err != nil {
		return err
	}
	if err := s.accessWrite(ctx, scope.Organization, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.RevokeAccessMember(ctx, member.UserID, s.config.now()); err != nil {
			return err
		}
		if audit != nil {
			_, err := w.RecordAccessAudit(ctx, *audit)
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return s.config.Hosted.Provider.RevokeMembership(ctx, member.ID)
}

func (s *Service) changeHostedRoleFor(ctx context.Context, credential hubapi.APICredential, id, role string) (hostedMemberView, error) {
	if !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return hostedMemberView{}, operatortool.ErrAccessDenied
	}
	member, err := s.hostedManagedMemberFor(ctx, credential, id, role != "owner")
	if err != nil {
		return hostedMemberView{}, err
	}
	return s.changeAccessRole(ctx, credential, member, role)
}

func (s *Service) changeHostedGrantFor(ctx context.Context, credential hubapi.APICredential, id, project string, write, runner, revoke bool) (hostedMemberView, error) {
	member, err := s.hostedMemberByID(ctx, credential, id)
	if err != nil {
		if classified := hubscope.OperatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			return hostedMemberView{}, classified
		}
		return hostedMemberView{}, hubapi.NativeNotFound()
	}
	if !hostedplans.SafeID(member.UserID) || !hostedplans.SafeID(project) {
		return hostedMemberView{}, hubapi.NativePublicInvalid("Select a member and project")
	}
	if err := s.hostedGrant(ctx, credential, member.UserID, project, write, runner, revoke); err != nil {
		return hostedMemberView{}, err
	}
	return s.hostedMemberResponse(ctx, member)
}

func (s *Service) revokeHostedInvitationFor(ctx context.Context, credential hubapi.APICredential, id string) error {
	return s.revokeAccessInvitation(ctx, credential, id)
}

func (s *Service) resendHostedInvitationFor(ctx context.Context, credential hubapi.APICredential, id string) error {
	view, err := s.pendingHostedInvitationFor(ctx, credential, id)
	if err != nil {
		return err
	}
	if err := s.validateHostedInvitationGrants(ctx, credential, view.Email, view.Grants); err != nil {
		return err
	}
	if err := auth.ResendInvitationID(ctx, s.config.Hosted.Provider, id); err != nil {
		return errors.Join(operatortool.ErrProviderUnavailable, err)
	}
	return nil
}

func (s *Service) pendingHostedInvitationFor(ctx context.Context, credential hubapi.APICredential, id string) (hostedInvitationView, error) {
	return s.pendingAccessInvitationFor(ctx, credential, id)
}

func (s *Service) sendReservedHostedInvitationFor(ctx context.Context, credential hubapi.APICredential, email, role string, grants []hostedMemberGrant) (view hostedInvitationView, resultErr error) {
	grants = append([]hostedMemberGrant{}, grants...)
	if err := s.validateHostedInvitationGrants(ctx, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	return s.createAccessInvitation(ctx, credential, email, role, grants)
}

func (s *Service) validateHostedGrants(ctx context.Context, credential hubapi.APICredential, grants []hostedMemberGrant) error {
	return s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
			return &hubapi.NativeError{Code: "forbidden", Message: "You cannot manage project grants", Status: 403, PublicMessage: true}
		}
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
		seen := map[string]bool{}
		for _, grant := range grants {
			if !hostedplans.SafeID(grant.ProjectID) || seen[grant.ProjectID] {
				return hubapi.NativePublicInvalid("Select each project only once")
			}
			seen[grant.ProjectID] = true
			if err := s.validateAccessInvitationGrant(ctx, r, credential, tracker.ProjectID(grant.ProjectID), grant.Write, grant.Runner); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) validateHostedInvitationGrants(ctx context.Context, credential hubapi.APICredential, email string, grants []hostedMemberGrant) error {
	return s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		return s.validateAccessInvitationGrants(ctx, r, credential, email, grants)
	})
}

func (s *Service) editHostedInvitationFor(ctx context.Context, credential hubapi.APICredential, id string, grants []hostedMemberGrant) (resultErr error) {
	return s.editAccessInvitation(ctx, credential, id, grants)
}

func (s *Service) validateHostedInvitationEdit(ctx context.Context, credential hubapi.APICredential, view hostedInvitationView, grants []hostedMemberGrant) error {
	previous := make([]hostedMemberGrant, 0, len(view.Grants))
	for _, grant := range view.Grants {
		previous = append(previous, hostedMemberGrant{ProjectID: grant.ProjectID})
	}
	if err := s.validateHostedGrants(ctx, credential, previous); err != nil {
		return err
	}
	return s.validateHostedInvitationGrants(ctx, credential, view.Email, grants)
}
