package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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
	view := hostedInvitationView{ID: row.ID, Email: row.Email, Role: row.Role, CreatedAt: hubapi.FormatHubTime(row.CreatedAt), ExpiresAt: hubapi.FormatHubTime(row.ExpiresAt), Grants: []hostedMemberGrant{}}
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
	var created []time.Time
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
				created = append(created, row.CreatedAt)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(page) < 200 {
			order := make([]int, len(result))
			for i := range order {
				order[i] = i
			}
			slices.SortStableFunc(order, func(a, b int) int {
				if c := created[b].Compare(created[a]); c != 0 {
					return c
				}
				return strings.Compare(result[b].ID, result[a].ID)
			})
			newest := make([]hostedInvitationView, 0, len(result))
			for _, i := range order {
				newest = append(newest, result[i])
			}
			return newest, nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Service) changeAccessGrant(ctx context.Context, credential hubapi.APICredential, user, project string, write, runner, revoke bool) error {
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
	if accessMemberMissing(err) {
		return false, "", sql.ErrNoRows
	}
	return member.Active, member.Role, err
}

func accessMemberMissing(err error) bool {
	return errors.Is(err, corestore.ErrNotFound) && errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied)
}

func (s *Service) changeAccessRole(ctx context.Context, credential hubapi.APICredential, membership auth.Membership, role string) (hostedMemberView, error) {
	if s.config.AccessRepository == nil || credential.Hosted == nil {
		return hostedMemberView{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	scope := nativeScope{organization: s.accessOrganization(), credential: credential, requireHostedAdmin: true}
	currentMember := func(ctx context.Context, r tenantstore.Reader) (accessstore.Member, bool, error) {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return accessstore.Member{}, false, err
		}
		target, err := r.AccessMember(ctx, membership.UserID)
		local := !accessMemberMissing(err)
		if err != nil && local {
			return target, false, err
		}
		if local && (!target.Active || target.MembershipID != membership.ID) {
			return target, false, auth.ErrHostedIdentity
		}
		if role == "owner" || target.Role == "owner" || membership.Role.Slug == "owner" {
			actor, err := r.AccessMember(ctx, credential.Hosted.Subject)
			if err != nil {
				return target, false, operatorAuthorizationError(err)
			}
			if !actor.Active || hubapi.LesserHostedRole(actor.Role, credential.HostedRole) != "owner" {
				return target, false, auth.ErrHostedIdentity
			}
		}
		return target, local, nil
	}
	if err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		_, _, err := currentMember(ctx, r)
		return err
	}); err != nil {
		return hostedMemberView{}, err
	}
	if err := s.config.Hosted.Provider.SetMembershipRole(ctx, membership.ID, role); err != nil {
		return hostedMemberView{}, errors.Join(mutation.ErrUncertain, err)
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.ReadTimeout)
	defer cancel()
	if err := s.accessWrite(persist, scope.organization, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(persist); err != nil {
			return err
		}
		target, local, err := currentMember(persist, w)
		if err != nil || !local {
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
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, membership.UserID)
		return err
	})
	emails := map[string]string{}
	switch {
	case accessMemberMissing(err):
	case err != nil:
		return hostedMemberView{}, err
	case !member.Active || member.MembershipID != membership.ID:
		return hostedMemberView{}, auth.ErrHostedIdentity
	default:
		membership.Role.Slug = hubapi.LesserHostedRole(membership.Role.Slug, member.Role)
		emails[member.UserID] = member.Email
	}
	view, err := s.hostedMemberIdentity(ctx, membership, emails)
	if err != nil {
		return view, err
	}
	var after tracker.ProjectID
	for {
		var page []accessstore.ProjectGrant
		if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			var err error
			page, err = r.AccessProjectGrants(ctx, membership.UserID, after, 200)
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
