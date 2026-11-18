package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) hostedProviderOrganization(ctx context.Context) (string, error) {

	var id string
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		id = tenant.ProviderID
		return err
	})
	return id, err
}
