package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accessMembers(ctx context.Context) ([]accessstore.Member, error) {
	result := []accessstore.Member{}
	var after string
	for {
		var page []accessstore.Member
		if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			page, err = r.AccessMembers(ctx, after, 200)
			return err
		}); err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < 200 {
			return result, nil
		}
		after = page[len(page)-1].UserID
	}
}

func (s *Service) accessMemberEmails(ctx context.Context) (map[string]string, error) {
	members, err := s.accessMembers(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, member := range members {
		if member.Active {
			result[member.UserID] = member.Email
		}
	}
	return result, nil
}

func (s *Service) accessMemberGrants(ctx context.Context) (map[string][]hostedMemberGrant, error) {
	members, err := s.accessMembers(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string][]hostedMemberGrant{}
	for _, member := range members {
		var after tracker.ProjectID
		for {
			var page []accessstore.ProjectGrant
			if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
				var err error
				page, err = r.AccessProjectGrants(ctx, member.UserID, after, 200)
				return err
			}); err != nil {
				return nil, err
			}
			for _, grant := range page {
				result[member.UserID] = append(result[member.UserID], hostedMemberGrant{ProjectID: string(grant.ProjectID), Write: grant.CanWrite, Runner: grant.ManageRunner})
				after = grant.ProjectID
			}
			if len(page) < 200 {
				break
			}
		}
	}
	return result, nil
}

func (s *Service) accessInvitationView(ctx context.Context, r accessstore.Reader, row accessstore.Invitation) (hostedInvitationView, error) {
	view := hostedInvitationView{ID: row.ID, Email: row.Email, Role: row.Role, CreatedAt: formatHubTime(row.CreatedAt), ExpiresAt: formatHubTime(row.ExpiresAt), Grants: []hostedMemberGrant{}}
	var after tracker.ProjectID
	for {
		page, err := r.AccessInvitationGrants(ctx, row.ID, after, 200)
		if err != nil {
			return view, err
		}
		for _, grant := range page {
			view.Grants = append(view.Grants, hostedMemberGrant{ProjectID: string(grant.ProjectID), Write: grant.CanWrite, Runner: grant.ManageRunner})
			after = grant.ProjectID
		}
		if len(page) < 200 {
			return view, nil
		}
	}
}

func (s *Service) accessPendingInvitations(ctx context.Context) ([]hostedInvitationView, error) {
	result := []hostedInvitationView{}
	var after string
	for {
		var page []accessstore.Invitation
		err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			page, err = r.AccessInvitations(ctx, after, 200)
			if err != nil {
				return err
			}
			for _, row := range page {
				if row.AcceptedUserID != "" || !row.ExpiresAt.After(s.config.now()) {
					continue
				}
				view, err := s.accessInvitationView(ctx, r, row)
				if err != nil {
					return err
				}
				result = append(result, view)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(page) < 200 {
			return result, nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Service) changeAccessGrant(ctx context.Context, credential apiCredential, user, project string, write, runner, revoke bool) error {
	scope := nativeScope{organization: s.accessOrganization(), credential: credential, requireHostedAdmin: true}
	if _, _, err := s.hostedMutationIdentity(ctx, credential); err != nil {
		return err
	}
	return s.accessWrite(ctx, scope.organization, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		scope.project = tracker.ProjectID(project)
		if err := s.authorizeAccessScope(ctx, w, scope, write || runner); err != nil {
			return err
		}
		member, err := w.AccessMember(ctx, user)
		if err != nil {
			return err
		}
		if !member.Active {
			return auth.ErrHostedIdentity
		}
		if revoke {
			if err := w.DeleteAccessProjectGrant(ctx, user, scope.project); err != nil {
				return err
			}
			return w.DeleteAccessTokenGrant(ctx, member.PrincipalID, scope.project)
		}
		if err := w.SetAccessProjectGrant(ctx, accessstore.ProjectGrant{OrganizationID: scope.organization, UserID: user, ProjectID: scope.project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: member.PrincipalID, ProjectID: scope.project})
	})
}

func (s *Service) accessLocalMember(ctx context.Context, user string) (bool, string, error) {
	var member accessstore.Member
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error { var err error; member, err = r.AccessMember(ctx, user); return err })
	if errors.Is(err, corestore.ErrNotFound) && errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return false, "", nil
	}
	return member.Active, member.Role, err
}

func (s *Service) changeAccessRole(ctx context.Context, credential apiCredential, membership auth.Membership, role string) (hostedMemberView, error) {
	if s.config.AccessRepository == nil || credential.Hosted == nil {
		return hostedMemberView{}, workItemStorageUnavailable()
	}
	scope := nativeScope{organization: s.accessOrganization(), credential: credential, requireHostedAdmin: true}
	currentMember := func(ctx context.Context, r tenantstore.Reader) (accessstore.Member, error) {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return accessstore.Member{}, err
		}
		target, err := r.AccessMember(ctx, membership.UserID)
		if err != nil {
			return target, err
		}
		if !target.Active || target.MembershipID != membership.ID {
			return target, auth.ErrHostedIdentity
		}
		if role == "owner" || target.Role == "owner" {
			actor, err := r.AccessMember(ctx, credential.Hosted.Subject)
			if err != nil {
				return target, operatorAuthorizationError(err)
			}
			if !actor.Active || lesserHostedRole(actor.Role, credential.HostedRole) != "owner" {
				return target, auth.ErrHostedIdentity
			}
		}
		return target, nil
	}
	if err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		_, err := currentMember(ctx, r)
		return err
	}); err != nil {
		return hostedMemberView{}, err
	}
	if err := s.config.Hosted.Provider.SetMembershipRole(ctx, membership.ID, role); err != nil {
		return hostedMemberView{}, errors.Join(mutation.ErrUncertain, err)
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubReadTimeout)
	defer cancel()
	if err := s.accessWrite(persist, scope.organization, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(persist); err != nil {
			return err
		}
		target, err := currentMember(persist, w)
		if err != nil {
			return err
		}
		target.Role, target.UpdatedAt = role, s.config.now()
		return w.UpdateAccessMember(persist, target)
	}); err != nil {
		return hostedMemberView{}, errors.Join(mutation.ErrUncertain, err)
	}
	view, err := s.hostedMemberResponse(persist, membership)
	if err != nil {
		return view, errors.Join(mutation.ErrUncertain, err)
	}
	return view, nil
}

func (s *Service) accessMemberResponse(ctx context.Context, membership auth.Membership) (hostedMemberView, error) {
	var member accessstore.Member
	if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, membership.UserID)
		return err
	}); err != nil {
		return hostedMemberView{}, err
	}
	if !member.Active || member.MembershipID != membership.ID {
		return hostedMemberView{}, auth.ErrHostedIdentity
	}
	membership.Role.Slug = lesserHostedRole(membership.Role.Slug, member.Role)
	view, err := s.hostedMemberIdentity(ctx, membership, map[string]string{member.UserID: member.Email})
	if err != nil {
		return view, err
	}
	var after tracker.ProjectID
	for {
		var page []accessstore.ProjectGrant
		if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			page, err = r.AccessProjectGrants(ctx, member.UserID, after, 200)
			return err
		}); err != nil {
			return view, err
		}
		for _, grant := range page {
			view.Grants = append(view.Grants, hostedMemberGrant{ProjectID: string(grant.ProjectID), Write: grant.CanWrite, Runner: grant.ManageRunner})
		}
		if len(page) < 200 {
			return view, nil
		}
		after = page[len(page)-1].ProjectID
	}
}
