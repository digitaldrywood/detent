package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createAPITokenFor(ctx context.Context, request tokenRequest) (tokenResponse, error) {
	return s.createAccessToken(ctx, request)
}

func (s *Service) rotateAPITokenFor(ctx context.Context, id string) (tokenResponse, error) {
	return s.rotateAccessToken(ctx, id)
}

func (s *Service) revokeAPITokenFor(ctx context.Context, id string) error {
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.RevokeAccessCredential(ctx, id, s.config.now()) })
}

func (s *Service) grantNativeTokenFor(ctx context.Context, id, organization, project string) error {
	return s.accessWrite(ctx, tracker.OrganizationID(organization), func(w tenantstore.Writer) error {
		return w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(organization), TokenID: id, ProjectID: tracker.ProjectID(project)})
	})
}

func (s *Service) createNativeOrganizationFor(ctx context.Context, name string) (nativeOrganization, error) {
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return nativeOrganization{}, nativePublicInvalid("Organization name is required")
	}
	organization := nativeOrganization{ID: tracker.OrganizationID(newNativeID("org")), Name: name}
	organization.Slug = strings.ReplaceAll(string(organization.ID), "_", "-")
	err := s.accessWrite(ctx, organization.ID, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, CreatedAt: s.config.now()})
	})
	return organization, err
}
