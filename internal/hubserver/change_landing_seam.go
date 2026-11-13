package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) landingTransitionSource(ctx context.Context, scope hubscope.NativeScope, item corestore.WorkItem) (*tracker.NativeChangeReference, error) {
	value, err := changelanding.New(changelanding.Deps{Repository: s.config.TenantRepository}).TransitionSource(ctx, s.authorization().Scope(scope), item)
	return value, hubapi.FromFeatureError(err)
}
