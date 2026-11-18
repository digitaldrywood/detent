package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type LookupRepository interface {
	LookupOrganization(context.Context, tracker.OrganizationID) (accessstore.Organization, error)
	LookupAttachment(context.Context, tracker.OrganizationID, string, string) (bool, error)
}

func LookupOrganization(ctx context.Context, repository Repository, org tracker.OrganizationID) (accessstore.Organization, error) {
	if owner, ok := repository.(LookupRepository); ok {
		return owner.LookupOrganization(ctx, org)
	}
	var result accessstore.Organization
	err := repository.ReadTenant(ctx, org, corestore.Serving, func(r Reader) error {
		var err error
		result, err = r.AccessOrganization(ctx)
		return err
	})
	return result, err
}

func LookupAttachment(ctx context.Context, repository Repository, org tracker.OrganizationID, id, key string) (bool, error) {
	if owner, ok := repository.(LookupRepository); ok {
		return owner.LookupAttachment(ctx, org, id, key)
	}
	var exists bool
	err := repository.ReadTenant(ctx, org, corestore.Serving, func(r Reader) error {
		var err error
		exists, err = r.AttachmentObjectExists(ctx, id, key)
		return err
	})
	return exists, err
}
