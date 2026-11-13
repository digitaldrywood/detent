package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessinvites"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func (s *Service) accessInvitesService() *accessinvites.Service {
	return accessinvites.New(accessinvites.Deps{Repository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.now, Provider: s.config.Hosted.Provider, Auth: s.requestAuth(), Members: s.accessMembersService(), StaffEmails: s.config.Hosted.StaffEmails, Classify: hubscope.OperatorAuthorizationError})
}
func (s *Service) acceptAccessInvitation(ctx context.Context, identity auth.Identity, token string, byID bool) error {
	return hubapi.FromFeatureError(s.accessInvitesService().Accept(ctx, identity, token, byID))
}
