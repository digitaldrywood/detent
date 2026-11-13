package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createAPITokenFor(ctx context.Context, request hubapi.TokenRequest) (hubapi.TokenResponse, error) {
	return s.createAccessToken(ctx, request)
}

func (s *Service) rotateAPITokenFor(ctx context.Context, id string) (hubapi.TokenResponse, error) {
	return s.rotateAccessToken(ctx, id)
}

func (s *Service) revokeAPITokenFor(ctx context.Context, id string) error {
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := s.authorizeCredentialMaintenanceWrite(ctx, w, s.accessOrganization()); err != nil {
			return err
		}
		return w.RevokeAccessCredential(ctx, id, s.config.Clock())
	})
}

func (s *Service) grantNativeTokenFor(ctx context.Context, id, organization, project string) error {
	if s.config.CredentialMaintenance && tracker.OrganizationID(organization) != s.accessOrganization() {
		return accessstore.ErrDenied
	}
	return s.accessWrite(ctx, tracker.OrganizationID(organization), func(w tenantstore.Writer) error {
		if err := s.authorizeCredentialMaintenanceWrite(ctx, w, tracker.OrganizationID(organization)); err != nil {
			return err
		}
		if _, err := w.RunnerIdentityByCredential(ctx, id); !errors.Is(err, corestore.ErrNotFound) {
			if err != nil {
				return err
			}
			return hubapi.NativeNotFound()
		}
		if err := w.ScopeAccessCredentialToNative(ctx, id, s.config.Clock()); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(organization), TokenID: id, ProjectID: tracker.ProjectID(project)})
	})
}

func (s *Service) createNativeOrganizationFor(ctx context.Context, name string) (nativeOrganization, error) {
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return nativeOrganization{}, hubapi.NativePublicInvalid("Organization name is required")
	}
	organization := nativeOrganization{ID: tracker.OrganizationID(hubapi.NewID("org")), Name: name}
	organization.Slug = strings.ReplaceAll(string(organization.ID), "_", "-")
	secret, audit, err := newNativeCursorSecret(s.config, organization.ID)
	if err != nil {
		return nativeOrganization{}, err
	}
	err = s.accessWrite(ctx, organization.ID, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, CreatedAt: s.config.Clock()}); err != nil {
			return err
		}
		return w.EnsureOrganizationSecret(ctx, secret, audit)
	})
	return organization, err
}
