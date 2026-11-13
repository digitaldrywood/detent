package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
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
	return nativeCredentialResult(s.requestAuth().AuthenticateRepositoryAPIHash(ctx, repository, organization, hash, renewalRunner, renewalOrganization))
}

func (s *Service) readRunnerIdentityTenant(ctx context.Context, organization tracker.OrganizationID, runnerID string) (runnerauth.Identity, error) {
	if s.config.TenantRepository == nil {
		return runnerauth.Identity{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var identity runnerauth.Identity
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.RunnerIdentity(ctx, runnerID)
		if err != nil {
			return err
		}
		if record.OrganizationID != organization || record.RemovedAt != nil {
			return corestore.ErrNotFound
		}
		identity, err = runnerIdentityRecord(ctx, reader, record)
		return err
	})
	return identity, err
}

func validRunnerCredentialForRenewal(token accessstore.Credential, hash string, now time.Time, allowExpired bool) bool {
	return hubauth.ValidRunnerCredentialForRenewal(token, hash, now, allowExpired)
}
