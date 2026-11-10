package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) acceptAccessInvitation(ctx context.Context, identity auth.Identity, token string, byID bool) error {
	if identity.Hosted == nil || !identity.EmailVerified || token == "" || len(token) > 512 {
		return auth.ErrHostedIdentity
	}
	var invitation auth.Invitation
	var err error
	if byID {
		invitation, err = auth.LookupInvitationID(ctx, s.config.Hosted.Provider, token)
	} else {
		invitation, err = s.config.Hosted.Provider.Invitation(ctx, token)
	}
	if err != nil {
		return operatorAuthorizationError(err)
	}
	var stored accessstore.Invitation
	err = s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if invitation.OrganizationID != tenant.ProviderID || !strings.EqualFold(invitation.Email, identity.Email) || !invitation.ExpiresAt.After(s.config.now()) {
			return auth.ErrHostedIdentity
		}
		stored, err = r.AccessInvitation(ctx, invitation.ID)
		if err != nil {
			return err
		}
		if stored.AcceptedUserID != "" || !strings.EqualFold(stored.Email, identity.Email) || !stored.ExpiresAt.After(s.config.now()) || !auth.ValidOrganizationRole(stored.Role) {
			return auth.ErrHostedIdentity
		}
		return nil
	})
	if err != nil {
		return err
	}
	prepared, err := s.prepareAccessMember(ctx, identity)
	if err != nil {
		return err
	}
	if invitation.State == "pending" {
		if byID {
			err = auth.AcceptInvitationID(ctx, s.config.Hosted.Provider, token, identity.Subject)
		} else {
			err = s.config.Hosted.Provider.AcceptInvitation(ctx, token, identity.Subject)
		}
		if err != nil {
			current, lookupErr := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, invitation.ID)
			if lookupErr != nil {
				return errors.Join(tenantauth.ErrUnavailable, err, lookupErr)
			}
			if current.State == "pending" {
				if errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
					return err
				}
				return errors.Join(tenantauth.ErrUnavailable, err)
			}
			if current.ID != invitation.ID || current.OrganizationID != invitation.OrganizationID || !strings.EqualFold(current.Email, invitation.Email) || current.State != "accepted" || current.AcceptedUserID != identity.Subject {
				return auth.ErrHostedIdentity
			}
		}
	} else if invitation.State != "accepted" || invitation.AcceptedUserID != identity.Subject {
		return auth.ErrHostedIdentity
	}
	memberIdentity := *identity.Hosted
	memberIdentity.OrganizationID = invitation.OrganizationID
	membership, err := s.hostedMembership(ctx, &memberIdentity)
	if err != nil {
		return err
	}
	if membership.Role.Slug != stored.Role {
		return auth.ErrHostedIdentity
	}
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		current, err := w.AccessInvitation(ctx, invitation.ID)
		if err != nil {
			return err
		}
		if current.Role != stored.Role {
			return auth.ErrHostedIdentity
		}
		if err := w.AcceptAccessInvitation(ctx, stored.ID, identity.Email, identity.Subject, s.config.now()); err != nil {
			return err
		}
		if err := s.storeAccessMember(ctx, w, identity, membership, prepared); err != nil {
			return err
		}
		if err := w.ClearAccessMemberGrants(ctx, identity.Subject); err != nil {
			return err
		}
		member, err := w.AccessMember(ctx, identity.Subject)
		if err != nil {
			return err
		}
		var after tracker.ProjectID
		for {
			grants, err := w.AccessInvitationGrants(ctx, stored.ID, after, 200)
			if err != nil {
				return err
			}
			for _, grant := range grants {
				if err := w.SetAccessProjectGrant(ctx, accessstore.ProjectGrant{OrganizationID: s.accessOrganization(), UserID: identity.Subject, ProjectID: grant.ProjectID, CanWrite: grant.CanWrite, ManageRunner: grant.ManageRunner}); err != nil {
					return err
				}
				if err := w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: s.accessOrganization(), TokenID: member.PrincipalID, ProjectID: grant.ProjectID}); err != nil {
					return err
				}
				after = grant.ProjectID
			}
			if len(grants) < 200 {
				if member.Active && (member.Role == "owner" || member.Role == "admin") {
					return w.SyncHostedAdministratorProjectAccess(ctx, member.UserID, member.PrincipalID)
				}
				return nil
			}
		}
	})
}
