package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBoardReplayLimit = 2048
const nativeBoardTrimLimit = 32

func (s *Service) maintainNativeBoard(ctx context.Context) {
	if s.database.postgres != nil {
		tenant := hubmaintenance.Tenant{Organization: s.appOrganizationID(), Generation: 1}
		if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
			tenant.Generation = s.config.Hosted.SharedEntry.Generation
		}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
		if _, err := s.WorkItemPageMaintenanceFeature().Step(budget.Context(ctx, tenant), tenant, budget); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "refresh work item page", "error", err)
		}
	}
	if s.config.TenantRepository != nil {
		organization := tracker.OrganizationID(s.appOrganizationID())
		var next tracker.ProjectID
		err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			var err error
			next, err = w.TrimBoardDeltas(ctx, s.boardRetentionScope.Project)
			return err
		})
		if err == nil {
			s.boardRetentionScope = hubscope.NativeScope{Organization: organization, Project: next}
		} else if !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "trim native board history", "error", err)
		}
	}
	if err := s.maintainTeamUnread(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "refresh team unread summary", "error", err)
	}
	if err := s.maintainBoardRecords(ctx); err != nil {
		failures := []error{err}
		if batch, ok := err.(interface{ Unwrap() []error }); ok {
			failures = batch.Unwrap()
		}
		for _, failure := range failures {
			if errors.Is(failure, context.Canceled) {
				continue
			}
			attributes := []any{"error", failure}
			var card *boardRefreshError
			if errors.As(failure, &card) {
				attributes = append(attributes, "work_item_id", card.item, "step", card.step)
			}
			s.config.Logger.ErrorContext(ctx, "refresh native board card", attributes...)
		}
	}
}
