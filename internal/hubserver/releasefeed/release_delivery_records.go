package releasefeed

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Delivery) ReadReleaseDeliveryRecord(ctx context.Context, scope hubscope.NativeScope, item string) (tracker.ReleaseDelivery, bool, error) {
	var record eventstore.ReleaseDelivery
	if s.deps.Repository == nil {
		return record.ReleaseDelivery, false, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ReleaseDelivery(ctx, scope.Project, tracker.NativeWorkItemID(item))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.ReleaseDelivery{}, false, nil
	}
	return record.ReleaseDelivery, err == nil, hubapi.ProjectRecordError(err)
}
