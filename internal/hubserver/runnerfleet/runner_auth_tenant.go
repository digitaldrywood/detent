package runnerfleet

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d IdentityDeps) ReadRunnerIdentityTenant(ctx context.Context, organization tracker.OrganizationID, runnerID string) (runnerauth.Identity, error) {
	if d.Repository == nil {
		return runnerauth.Identity{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var identity runnerauth.Identity
	err := d.Repository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.RunnerIdentity(ctx, runnerID)
		if err != nil {
			return err
		}
		if record.OrganizationID != organization || record.RemovedAt != nil {
			return corestore.ErrNotFound
		}
		identity, err = hubauth.RunnerIdentityRecord(ctx, reader, record)
		return err
	})
	return identity, err
}
