package platformstore

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TenantKeySnapshot struct {
	Organization Organization
	Key          Key
	Authority    apikey.KeyAuthority
	Resource     string
}

func (r *Repository) PlatformKey(ctx context.Context, input tenantauth.PlatformKeyRequest) (tenantauth.PlatformKey, error) {
	release, err := r.acquireEntry(ctx, corestore.Serving)
	if err != nil {
		return tenantauth.PlatformKey{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	defer release()
	result, err := tenantauth.ReadPlatformKey(ctx, r.pool, input, r.now())
	if errors.Is(err, tenantauth.ErrDenied) {
		return tenantauth.PlatformKey{}, errors.Join(ErrNoSession, err)
	}
	return result, err
}

func (r *Repository) TenantKey(ctx context.Context, id string, organization tracker.OrganizationID) (TenantKeySnapshot, error) {
	result, err := r.PlatformKey(ctx, tenantauth.PlatformKeyRequest{Organization: organization, KeyID: id, Managed: true})
	if err != nil {
		return TenantKeySnapshot{}, err
	}
	return TenantKeySnapshot{
		Organization: Organization{ID: organization, ProviderID: result.ProviderID, Slug: result.Slug, Endpoint: result.Endpoint, Generation: result.Generation, Managed: true, State: "ready"},
		Key:          Key{ID: result.Authority.ID, Hash: result.Hash, Kind: result.Authority.Kind, OwnerSubject: result.OwnerSubject, OwnerEmail: result.OwnerEmail, Permission: string(result.Authority.Permission), ExpiresAt: result.ExpiresAt},
		Authority:    result.Authority, Resource: result.Resource,
	}, nil
}
