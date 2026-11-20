package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authenticateTenantAPIHash(ctx context.Context, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return s.authenticateRepositoryAPIHash(ctx, s.config.TenantRepository, organization, hash, renewalRunner, renewalOrganization)
}

func (s *Service) authenticateRepositoryAPIHash(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return hubscope.NativeCredentialResult(s.requestAuth().AuthenticateRepositoryAPIHash(ctx, repository, organization, hash, renewalRunner, renewalOrganization))
}
