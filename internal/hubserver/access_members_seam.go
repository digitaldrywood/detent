package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessmembers"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type accessMemberPreparation = accessmembers.Preparation

func (s *Service) accessMembersService() *accessmembers.Service {
	return accessmembers.New(accessmembers.Deps{Repository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.now, GenerateToken: s.config.generateToken})
}

func (s *Service) prepareAccessMember(ctx context.Context, identity auth.Identity) (accessMemberPreparation, error) {
	prepared, err := s.accessMembersService().Prepare(ctx, identity)
	return prepared, hubapi.FromFeatureError(err)
}

func (s *Service) storeAccessMember(ctx context.Context, w tenantstore.Writer, identity auth.Identity, membership auth.Membership, prepared accessMemberPreparation) error {
	return hubapi.FromFeatureError(s.accessMembersService().Store(ctx, w, identity, membership, prepared))
}
func (s *Service) addHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership) error {
	return s.storeHostedMember(ctx, identity, membership, "")
}

func (s *Service) storeHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership, organizationName string) error {
	return hubapi.FromFeatureError(s.accessMembersService().Publish(ctx, identity, membership, organizationName))
}
