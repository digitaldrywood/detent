package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readReleaseDeliveryRecord(ctx context.Context, scope nativeScope, item string) (tracker.ReleaseDelivery, bool, error) {
	var record eventstore.ReleaseDelivery
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ReleaseDelivery(ctx, scope.project, tracker.NativeWorkItemID(item))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.ReleaseDelivery{}, false, nil
	}
	return record.ReleaseDelivery, err == nil, err
}
