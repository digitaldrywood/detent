package hubauth

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Authenticator) AuthorizeOrganization(ctx context.Context, scope Input) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if scope.Credential.Hosted != nil {
		if string(scope.Organization) != s.deps.Hosted.OrganizationID {
			return hubapi.NotFound()
		}
		return nil
	}

	if s.deps.AccessRepository == nil {
		return hubapi.NotFound()
	}
	return s.read(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if _, err := r.AccessTenant(ctx); err != nil {
			return err
		}
		if scope.Credential.Scope == Admin && !scope.Credential.NativeOnly || scope.Credential.Scope == Operator && !scope.Credential.NativeOnly {
			return nil
		}
		grants, err := r.AccessTokenGrants(ctx, scope.Credential.ID, "", 1)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return hubapi.NotFound()
		}
		return nil
	})
}
