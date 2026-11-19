package changefiles

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadContentReference(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID, key string) (attachment.ArchiveReference, error) {
	if organization == "" || repository == nil {
		return attachment.ArchiveReference{}, hubcontent.ErrContentStorageUnconfigured
	}
	var ref attachment.ArchiveReference
	err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(hubrecords.AttemptArchiveReader)
		if !ok {
			return hubcontent.ErrContentStorageUnconfigured
		}
		object, err := reader.ArchiveObjectByKey(ctx, key)
		if err != nil {
			return err
		}
		ref = object.Reference
		if object.OrganizationID != organization || ref.Key != key || ref.Validate(string(organization)) != nil || ref.CustomerOwned || ref.BindingID != "" || ref.SchemaVersion != 1 {
			return artifact.ErrIntegrity
		}
		return nil
	})
	return ref, err
}
