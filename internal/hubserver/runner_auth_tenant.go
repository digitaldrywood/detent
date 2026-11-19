package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerIdentityRecord(ctx context.Context, reader tenantstore.Reader, record runnerstore.Identity) (runnerauth.Identity, error) {
	return hubauth.RunnerIdentityRecord(ctx, reader, record)
}

func (s *Service) authenticateTenantAPIHash(ctx context.Context, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return s.authenticateRepositoryAPIHash(ctx, s.config.TenantRepository, organization, hash, renewalRunner, renewalOrganization)
}

func (s *Service) authenticateRepositoryAPIHash(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return hubscope.NativeCredentialResult(s.requestAuth().AuthenticateRepositoryAPIHash(ctx, repository, organization, hash, renewalRunner, renewalOrganization))
}

func validRunnerCredentialForRenewal(token accessstore.Credential, hash string, now time.Time, allowExpired bool) bool {
	return hubauth.ValidRunnerCredentialForRenewal(token, hash, now, allowExpired)
}
