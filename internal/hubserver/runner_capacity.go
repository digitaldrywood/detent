package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) authorizeRunnerCapacity(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) error {
	return hubapi.FromFeatureError(s.runnerAdviceService().Authorize(ctx, reader, s.authInput(scope)))
}

func (s *Service) readRunnerCapacity(ctx context.Context, scope hubscope.NativeScope, resource, backend string) (any, error) {
	value, err := s.runnerAdviceService().Read(ctx, s.authInput(scope), resource, backend)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) updateRunnerCapacityCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerCapacityChange) (any, error) {
	return s.persistRunnerCapacityCommand(ctx, scope, resource, request)
}
